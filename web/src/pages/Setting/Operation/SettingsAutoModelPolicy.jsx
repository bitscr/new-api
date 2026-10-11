/*
Copyright (C) 2026 bitscr

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For licensing inquiries, please open an issue at https://github.com/bitscr/new-api/issues
*/

import React, { useEffect, useRef, useState } from 'react';
import {
  Banner,
  Button,
  Col,
  Form,
  Row,
  Spin,
  Typography,
} from '@douyinfe/semi-ui';
import { API, compareObjects, showError, showSuccess, showWarning } from '../../../helpers';
import { useTranslation } from 'react-i18next';

const { Text } = Typography;

const AUTO_MODEL_OPTION_KEYS = [
  'AutoModelMaxAttempts',
  'AutoModelPermanentCooldownHours',
  'AutoModelPermanentCooldownMaxDays',
  'AutoModelScoreDecayMinutes',
  'AutoModelPermanentKeywords',
];

// 数值项的中文名。报错信息里要用人能看懂的名字,否则管理员看到的是一串
// OptionMap key。这里的每一项都必须也在语言包里,漏一个就会显示原文。
const NUMERIC_LABELS = {
  AutoModelMaxAttempts: 'auto 单请求最大尝试次数',
  AutoModelPermanentCooldownHours: '确定性失败冷却时长(小时)',
  AutoModelPermanentCooldownMaxDays: '确定性失败冷却封顶(天)',
  AutoModelScoreDecayMinutes: '评分衰减窗口(分钟)',
};

/**
 * auto 的失败判据与时长。
 *
 * 判据硬编码认不出上游冒出的新错误串,那些失败只会拿 15 分钟冷却然后被反复重试。
 * 这里让管理员把日志里看到的永久性报错补成关键词,以及调整四个时长/次数上限。
 *
 * 红线:关键词只对真实业务请求的失败生效,只升级冷却时长,作用域仍是单个
 * (模型, 渠道) 组合——不会禁用渠道,也不引入任何主动探测。
 */
export default function SettingsAutoModelPolicy(props) {
  const { t } = useTranslation();
  const [loading, setLoading] = useState(false);
  const [inputs, setInputs] = useState({
    AutoModelMaxAttempts: '4',
    AutoModelPermanentCooldownHours: '24',
    AutoModelPermanentCooldownMaxDays: '30',
    AutoModelScoreDecayMinutes: '30',
    AutoModelPermanentKeywords: '',
  });
  const refForm = useRef();
  const [inputsRow, setInputsRow] = useState(inputs);

  const onSubmit = () => {
    const updateArray = compareObjects(inputs, inputsRow);
    if (!updateArray.length) return showWarning(t('你似乎并没有修改什么'));

    // 四个数值都必须是 >= 1 的整数:写 0 会让 auto 直接不可用。
    for (const key of AUTO_MODEL_OPTION_KEYS.slice(0, 4)) {
      const raw = String(inputs[key] ?? '').trim();
      const value = Number(raw);
      if (!Number.isInteger(value) || value < 1) {
        // 曾经这里是 t(`${key} 必须是大于等于 1 的整数`),模板串永远对不上语言包,
        // 英文界面会直接显示中文。用人能看懂的名字,整句走语言包。
        return showError(
          t('{{name}} 必须是大于等于 1 的整数', { name: t(NUMERIC_LABELS[key] || key) }),
        );
      }
    }

    const requestQueue = updateArray.map((item) => {
      return API.put('/api/option/', {
        key: item.key,
        value: String(inputs[item.key] ?? ''),
      });
    });
    setLoading(true);
    Promise.all(requestQueue)
      .then((res) => {
        // 后端校验失败返回的是 HTTP 200 + success:false,不是 HTTP 错误。
        // 只看 res.includes(undefined) 会把"关键词被拒绝"显示成"保存成功",
        // 管理员以为配好了,实际判据没生效——必须读出 body 里的 success。
        const rejected = res.find(
          (item) => !item || !item.data || item.data.success === false,
        );
        if (rejected) {
          const message =
            rejected && rejected.data && rejected.data.message
              ? rejected.data.message
              : t('保存失败，请重试');
          return showError(message);
        }
        showSuccess(t('保存成功'));
        props.refresh();
      })
      .catch(() => {
        showError(t('保存失败，请重试'));
      })
      .finally(() => {
        setLoading(false);
      });
  };

  useEffect(() => {
    const currentInputs = {};
    for (const key of AUTO_MODEL_OPTION_KEYS) {
      if (props.options && props.options[key] !== undefined) {
        currentInputs[key] = String(props.options[key]);
      } else {
        currentInputs[key] = inputs[key];
      }
    }
    setInputs(currentInputs);
    setInputsRow(structuredClone(currentInputs));
    if (refForm.current) refForm.current.setValues(currentInputs);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [props.options]);

  return (
    <Spin spinning={loading}>
      <Form
        values={inputs}
        getFormApi={(formAPI) => (refForm.current = formAPI)}
        style={{ marginBottom: 15 }}
      >
        <Form.Section text={t('auto 路由失败判据与时长')}>
          <Banner
            type='info'
            description={t(
              '判据只影响 auto 请求真实失败的冷却时长,作用域是「模型 + 渠道」组合,不会禁用渠道。',
            )}
            style={{ marginBottom: 16 }}
          />
          <Row gutter={16}>
            <Col xs={24} sm={12} md={8} lg={8} xl={8}>
              <Form.InputNumber
                field={'AutoModelMaxAttempts'}
                label={t('auto 单请求最大尝试次数')}
                min={1}
                extraText={t('含首次,默认 4;命名模型的重试次数不受此值影响')}
                onChange={(value) =>
                  setInputs({ ...inputs, AutoModelMaxAttempts: String(value) })
                }
              />
            </Col>
            <Col xs={24} sm={12} md={8} lg={8} xl={8}>
              <Form.InputNumber
                field={'AutoModelPermanentCooldownHours'}
                label={t('确定性失败冷却时长(小时)')}
                min={1}
                extraText={t('默认 24;抖动失败(限流/超时/5xx)仍是 15 分钟起')}
                onChange={(value) =>
                  setInputs({
                    ...inputs,
                    AutoModelPermanentCooldownHours: String(value),
                  })
                }
              />
            </Col>
            <Col xs={24} sm={12} md={8} lg={8} xl={8}>
              <Form.InputNumber
                field={'AutoModelPermanentCooldownMaxDays'}
                label={t('确定性失败冷却封顶(天)')}
                min={1}
                extraText={t('默认 30;每犯一次翻倍,不超过此值')}
                onChange={(value) =>
                  setInputs({
                    ...inputs,
                    AutoModelPermanentCooldownMaxDays: String(value),
                  })
                }
              />
            </Col>
          </Row>
          <Row gutter={16}>
            <Col xs={24} sm={12} md={8} lg={8} xl={8}>
              <Form.InputNumber
                field={'AutoModelScoreDecayMinutes'}
                label={t('评分衰减窗口(分钟)')}
                min={1}
                extraText={t(
                  '默认 30;必须长于抖动冷却窗口(15 分钟),否则冷却到期时评分已归零',
                )}
                onChange={(value) =>
                  setInputs({
                    ...inputs,
                    AutoModelScoreDecayMinutes: String(value),
                  })
                }
              />
            </Col>
          </Row>
          <Row gutter={16}>
            <Col xs={24} sm={16}>
              <Form.TextArea
                field={'AutoModelPermanentKeywords'}
                label={t('确定性失败关键词(补充判据)')}
                placeholder={t('一行一个,不区分大小写')}
                extraText={t(
                  '把日志里看到的永久性报错填进来(例如「function calling is not supported」)。命中后该组合进入长冷却,不再被反复重试。上下文超长永远按抖动处理,不受此处影响。',
                )}
                autosize={{ minRows: 6, maxRows: 12 }}
                onChange={(value) =>
                  setInputs({ ...inputs, AutoModelPermanentKeywords: value })
                }
              />
              <Text type='tertiary'>
                {t(
                  '不建议加入:预扣费额度失败 / insufficient_user_quota(余额问题)、ReasoningEffort invalid(请求参数写错)。',
                )}
              </Text>
            </Col>
          </Row>
          <Row style={{ marginTop: 12 }}>
            <Button size='default' onClick={onSubmit}>
              {t('保存 auto 判据设置')}
            </Button>
          </Row>
        </Form.Section>
      </Form>
    </Spin>
  );
}