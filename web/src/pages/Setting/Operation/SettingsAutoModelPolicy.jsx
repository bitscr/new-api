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
import {
  API,
  compareObjects,
  showError,
  showSuccess,
  showWarning,
} from '../../../helpers';
import { useTranslation } from 'react-i18next';

const { Text } = Typography;

const AUTO_MODEL_OPTION_KEYS = [
  'AutoModelMaxAttempts',
  'AutoModelCooldownMaxHours',
  'AutoModelCooldownBaseMinutes',
  'AutoModelScoreDecayMinutes',
  'AutoModelScoreRefreshSeconds',
  'AutoModelPermanentKeywords',
  'AutoModelTransientKeywords',
];

// 需要做"至少 1 的整数"校验的键。用白名单而不是"从上面 filter 掉关键词":
// 后者每加一个文本项就要记得补一次排除,漏一个就报错(实测漏过 Transient)。
const AUTO_MODEL_NUMERIC_KEYS = [
  'AutoModelMaxAttempts',
  'AutoModelCooldownMaxHours',
  'AutoModelCooldownBaseMinutes',
  'AutoModelScoreDecayMinutes',
  'AutoModelScoreRefreshSeconds',
];

// 数值项的中文名。报错信息里要用人能看懂的名字,否则管理员看到的是一串
// OptionMap key。这里的每一项都必须也在语言包里,漏一个就会显示原文。
const NUMERIC_LABELS = {
  AutoModelMaxAttempts: 'auto 单请求最大尝试次数',
  AutoModelCooldownMaxHours: '普通冷却封顶(小时)',
  AutoModelCooldownBaseMinutes: '普通冷却起始时间(分钟)',
  AutoModelScoreDecayMinutes: '证据衰减窗口(分钟)',
  AutoModelScoreRefreshSeconds: '路由状态页面刷新间隔(秒)',
};

// 判据(两份关键词表)完全以数据库里的值为准:前端不做任何预填,
// 后端 GET /api/option/ 回显什么,输入框里就是什么。
// 命中"确定性失败关键词"的组合会被直接从渠道里删除(不再有冷却等待);
// 命中"普通冷却关键词"的失败强制走抖动冷却阶梯。

/**
 * auto 的失败判据与时长。
 *
 * 判据硬编码认不出上游冒出的新错误串,那些失败只会拿 15 分钟冷却然后被反复重试。
 * 这里让管理员把日志里看到的永久性报错补成关键词,以及调整几个时长/次数上限。
 *
 * 红线:关键词只对真实业务请求的失败生效,作用域仍是单个 (模型, 渠道) 组合——
 * 不会禁用渠道(渠道被删空时才会自动禁用),也不引入任何主动探测。
 */
export default function SettingsAutoModelPolicy(props) {
  const { t } = useTranslation();
  const [loading, setLoading] = useState(false);
  const [inputs, setInputs] = useState({
    AutoModelMaxAttempts: '4',
    AutoModelCooldownMaxHours: '6',
    AutoModelCooldownBaseMinutes: '15',
    AutoModelScoreDecayMinutes: '360',
    AutoModelScoreRefreshSeconds: '60',
    AutoModelPermanentKeywords: '',
    AutoModelTransientKeywords: '',
  });
  const refForm = useRef();
  const [inputsRow, setInputsRow] = useState(inputs);

  const onSubmit = () => {
    const updateArray = compareObjects(inputs, inputsRow);
    if (!updateArray.length) return showWarning(t('你似乎并没有修改什么'));

    // 只有数值项才做"至少 1 的整数"校验:两张关键词表是自由文本,
    // Number('模型不可用') === NaN,漏排除它们就会在改任何数值时报
    // "AutoModelTransientKeywords 必须是大于等于 1 的整数"。
    for (const key of AUTO_MODEL_NUMERIC_KEYS) {
      const raw = String(inputs[key] ?? '').trim();
      const value = Number(raw);
      if (!Number.isInteger(value) || value < 1) {
        // 曾经这里是 t(`${key} 必须是大于等于 1 的整数`),模板串永远对不上语言包,
        // 英文界面会直接显示中文。用人能看懂的名字,整句走语言包。
        return showError(
          t('{{name}} 必须是大于等于 1 的整数', {
            name: t(NUMERIC_LABELS[key] || key),
          }),
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
        // 后端允许保存"不合逻辑"的组合,但会在成功响应里附带建议值
        // (如"保存成功,但建议封顶值为 6")。这类提示不能当成失败,也不能吞掉——
        // 直接显示后端给的整句,用户就知道值已保存、只是建议改。
        const advised = res.find(
          (item) =>
            item &&
            item.data &&
            typeof item.data.message === 'string' &&
            item.data.message,
        );
        if (advised) {
          showWarning(advised.data.message);
        } else {
          showSuccess(t('保存成功'));
        }
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
      const fromApi = props.options ? props.options[key] : undefined;
      if (fromApi === undefined) {
        currentInputs[key] = inputs[key];
        continue;
      }
      currentInputs[key] = String(fromApi);
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
              '确定性关键词命中后会把该模型从这条渠道里直接删除(渠道被删空时自动禁用);其余失败按普通冷却阶梯处理。作用域是「模型 + 渠道」组合,不会禁用渠道本身。',
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
                field={'AutoModelCooldownBaseMinutes'}
                label={t('普通冷却起始时间(分钟)')}
                min={1}
                extraText={t(
                  '默认 15。普通(抖动)失败的冷却阶梯从这里起步,每犯一次在上一次的基础上翻倍;某次翻倍超出封顶时,以封顶为准。',
                )}
                onChange={(value) =>
                  setInputs({
                    ...inputs,
                    AutoModelCooldownBaseMinutes: String(value),
                  })
                }
              />
            </Col>
            <Col xs={24} sm={12} md={8} lg={8} xl={8}>
              <Form.InputNumber
                field={'AutoModelCooldownMaxHours'}
                label={t('普通冷却封顶(小时)')}
                min={1}
                extraText={t(
                  '默认 6。普通(抖动)失败的冷却阶梯从「普通冷却起始时间」起步、每犯一次翻倍,翻倍超出封顶时以封顶为准。这里改的就是封顶。抬得太低会让反复出错的组合很快解冻、又回候选池;太高则会长期放弃一条只是暂时抖动的渠道。',
                )}
                onChange={(value) =>
                  setInputs({
                    ...inputs,
                    AutoModelCooldownMaxHours: String(value),
                  })
                }
              />
            </Col>
          </Row>
          <Row gutter={16}>
            <Col xs={24} sm={12} md={8} lg={8} xl={8}>
              <Form.InputNumber
                field={'AutoModelScoreDecayMinutes'}
                label={t('证据衰减窗口(分钟)')}
                min={16}
                extraText={t(
                  '默认 360,与"普通冷却封顶"联动(封顶 6 小时 = 360 分钟)。含义:成功/失败计数和两类延迟样本按本窗口线性衰减;冷却到期时证据仍保留衰减余量,惩罚不会提前归零。延迟数值本身不随空闲下降,证据陈旧时按未知处理。两条硬性关系:①必须大于冷却第二档(30 分钟),否则第 2 次失败解冻时证据已基本归零、惩罚作废;②不能小于封顶,否则最高几档解冻时证据已经失效,冷却只剩"等时间"。改封顶会自动同步本值;一旦你单独填过,就以你填的为准。',
                )}
                onChange={(value) =>
                  setInputs({
                    ...inputs,
                    AutoModelScoreDecayMinutes: String(value),
                  })
                }
              />
            </Col>
            <Col xs={24} sm={12} md={8} lg={8} xl={8}>
              <Form.InputNumber
                field={'AutoModelScoreRefreshSeconds'}
                label={t('路由状态页面刷新间隔(秒)')}
                min={1}
                extraText={t(
                  '默认 60。「auto 路由状态」页面按此间隔自动刷新,登录用户顶部栏「路由状态」入口进入后生效。',
                )}
                onChange={(value) =>
                  setInputs({
                    ...inputs,
                    AutoModelScoreRefreshSeconds: String(value),
                  })
                }
              />
            </Col>
          </Row>
          <Row gutter={16}>
            <Col xs={24} sm={16}>
              <Form.TextArea
                field={'AutoModelPermanentKeywords'}
                label={t('确定性失败关键词(自动删除判据)')}
                placeholder={t(
                  '一行一个,不区分大小写;中文文案、英文词语、状态码都可以',
                )}
                extraText={t(
                  '在错误正文、错误码、错误类型里做大小写不敏感的子串匹配。命中后:直接把该模型从这条渠道里删除(不删渠道),不再有冷却等待。上下文超长永远按抖动处理,不受此处影响。',
                )}
                autosize={{ minRows: 8, maxRows: 16 }}
                onChange={(value) =>
                  setInputs({ ...inputs, AutoModelPermanentKeywords: value })
                }
              />
              <Text type='tertiary'>
                {t(
                  '三种写法都支持:中文文案(模型不可用)、英文词语(model not found)、状态码(status code 524)。判据只以这里保存的值为准,源码不内置任何条目。',
                )}
              </Text>
            </Col>
          </Row>
          <Row gutter={16}>
            <Col xs={24} sm={16}>
              <Form.TextArea
                field={'AutoModelTransientKeywords'}
                label={t('普通冷却关键词(强制降级为抖动失败)')}
                placeholder={t(
                  '一行一个,不区分大小写;中文文案、英文词语、状态码都可以',
                )}
                extraText={t(
                  '优先级最高:命中后这次失败只按普通抖动冷却处理(以「普通冷却起始时间」起步的阶梯),绝不会命中确定性关键词、也不会删除模型。用来把其实会自愈的错误钉在这一档,防止被上面的确定性关键词误伤。',
                )}
                autosize={{ minRows: 6, maxRows: 14 }}
                onChange={(value) =>
                  setInputs({ ...inputs, AutoModelTransientKeywords: value })
                }
              />
              <Text
                type='tertiary'
                size='small'
                style={{ display: 'block', marginTop: 4 }}
              >
                {t(
                  '上游网关类故障(如 520/524)不再由后端状态码内置判定;若想让某个失败只走普通冷却,把它的文案写在这里即可。',
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
