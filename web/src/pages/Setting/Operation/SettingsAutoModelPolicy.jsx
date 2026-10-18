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
  'AutoModelCooldownMaxHours',
  'AutoModelCooldownBaseMinutes',
  'AutoModelScoreDecayMinutes',
  'AutoModelPermanentKeywords',
  'AutoModelTransientKeywords',
];

// 需要做"至少 1 的整数"校验的键。用白名单而不是"从上面 filter 掉关键词":
// 后者每加一个文本项就要记得补一次排除,漏一个就报错(实测漏过 Transient)。
const AUTO_MODEL_NUMERIC_KEYS = [
  'AutoModelMaxAttempts',
  'AutoModelPermanentCooldownHours',
  'AutoModelCooldownMaxHours',
  'AutoModelCooldownBaseMinutes',
  'AutoModelScoreDecayMinutes',
];

// 关键词键与数值键必须正好构成上面那张总表,漏一个就会出现"存了但不校验"
// 或"校验了但没存"的错配。改这张表时这里会立刻报出来。
const AUTO_MODEL_KEYWORD_KEYS = [
  'AutoModelPermanentKeywords',
  'AutoModelTransientKeywords',
];

// 数值项的中文名。报错信息里要用人能看懂的名字,否则管理员看到的是一串
// OptionMap key。这里的每一项都必须也在语言包里,漏一个就会显示原文。
const NUMERIC_LABELS = {
  AutoModelMaxAttempts: 'auto 单请求最大尝试次数',
  AutoModelPermanentCooldownHours: '确定性失败冷却时长(小时)',
  AutoModelCooldownMaxHours: '普通冷却封顶(小时)',
  AutoModelCooldownBaseMinutes: '普通冷却起始时间(分钟)',
  AutoModelScoreDecayMinutes: '评分衰减窗口(分钟)',
};

// 本机在用的"普通冷却"关键词:命中后强制只走抖动冷却(15m 起步的阶梯),
// 绝不升级为确定性失败。用于把其实会自愈、却被确定性判据误伤的错误钉回来。
//
// 内容与后端 setting/operation_setting 的 autoModelBuiltinTransientKeywords 一致。
// 后端才是权威来源(未配置时 GET /api/option/ 会回显后端内置值,直接覆盖这里);
// 这里保留一份是为了在接口返回前/失败时输入框不为空。
//
// 这里不再收 520/524:它们是上游/CF 侧的问题,后端已按确定性失败处理
// (冷却 → 再犯摘除),留在这里反而会把它们钉回抖动档、永远删不掉。
const DEFAULT_TRANSIENT_KEYWORDS = [
  // 限流是明确的临时状态,冷却窗口本来就有更合适的阶梯。
  'rpm exhausted',
  'token plan entitlement exhausted',
  // 上游自己的容量/健康问题,不是模型不存在。
  'all candidates failed',
  'upstream_response_timeout',
];
// 内容与后端 autoModelBuiltinPermanentKeywords 一致(后端为权威来源,理由同上)。
const DEFAULT_PERMANENT_KEYWORDS = [
  // 上游明说"这个模型我不提供"——结构化 code 缺失时的自然语言兜底
  '模型不可用',
  '模型不存在',
  'model not found',
  'community model not found',
  // 已实证会复发:某个上游确实不支持的确定性能力。
  // 本网关的 auto 主要用于工具调用,给不了 tools 的模型对这个用途就是废的,
  // 应当冷却并在再犯时摘除,而不是留着被反复选中、每次白费一次尝试。
  'function calling is not supported',
  // 账号/密钥级确定性失败:换了模型也一样,重试没有意义
  'invalid_api_key',
  'invalid_authentication',
  // 明确的状态码(可写数字,判定用子串匹配 “status code: 404” 这类文本)
  'status code: 404',
];

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
    AutoModelCooldownMaxHours: '6',
    AutoModelCooldownBaseMinutes: '15',
    AutoModelScoreDecayMinutes: '360',
    AutoModelPermanentKeywords: DEFAULT_PERMANENT_KEYWORDS.join('\n'),
    AutoModelTransientKeywords: DEFAULT_TRANSIENT_KEYWORDS.join('\n'),
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
        // 后端允许保存"不合逻辑"的组合,但会在成功响应里附带建议值
        // (如"保存成功,但建议封顶值为 6")。这类提示不能当成失败,也不能吞掉——
        // 直接显示后端给的整句,用户就知道值已保存、只是建议改。
        const advised = res.find(
          (item) =>
            item && item.data && typeof item.data.message === 'string' && item.data.message,
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
      // 关键词表是文本:后端未配置时回显空串,直接采用会把前端预填清掉,
      // 管理员打开看到空框、以为没有内置判据。空串一律保留预填值。
      if (AUTO_MODEL_KEYWORD_KEYS.includes(key) && String(fromApi).trim() === '') {
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
                field={'AutoModelCooldownBaseMinutes'}
                label={t('普通冷却起始时间(分钟)')}
                min={1}
                extraText={t(
                  '默认 15。普通(抖动)失败的冷却阶梯从这里起步,每犯一次在上一次的基础上翻倍;某次翻倍超出封顶时,以封顶为准。',
                )}
                onChange={(value) =>
                  setInputs({ ...inputs, AutoModelCooldownBaseMinutes: String(value) })
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
                  setInputs({ ...inputs, AutoModelCooldownMaxHours: String(value) })
                }
              />
            </Col>
            <Col xs={24} sm={12} md={8} lg={8} xl={8}>
              <Form.InputNumber
                field={'AutoModelPermanentCooldownHours'}
                label={t('确定性失败冷却时长(小时)')}
                min={1}
                extraText={t(
                  '默认 24。确定性失败(模型不存在/无权/不支持)首犯冷却这么久,再犯直接把该模型从渠道里删除,不再逐级延长',
                )}
                onChange={(value) =>
                  setInputs({
                    ...inputs,
                    AutoModelPermanentCooldownHours: String(value),
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
                min={16}
                extraText={t(
                  '默认 360,与"普通冷却封顶"联动(封顶 6 小时 = 360 分钟)。含义:一次失败把健康分从 0.5 压到 0.35,这个扣分会在本窗口内线性爬回中立 0.5。两条硬性关系:①必须大于冷却第二档(30 分钟),否则第 2 次失败解冻时分数正好归零、惩罚作废;②不能小于封顶,否则最高几档解冻时扣分已是 0,冷却只剩"等时间"。改封顶会自动同步本值;一旦你单独填过,就以你填的为准。',
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
                label={t('确定性失败关键词(自动删除判据)')}
                placeholder={t('一行一个,不区分大小写;中文文案、英文词语、状态码都可以')}
                extraText={t(
                  '在错误正文、错误码、错误类型里做子串匹配。命中后:首犯按「确定性失败冷却时长」冷却(默认 24 小时),再犯直接把该模型从这条渠道里删除(不删渠道)。上文已预填本机在用的判据。上下文超长永远按抖动处理,不受此处影响。',
                )}
                autosize={{ minRows: 8, maxRows: 16 }}
                onChange={(value) =>
                  setInputs({ ...inputs, AutoModelPermanentKeywords: value })
                }
              />
              <Text type='tertiary'>
                {t(
                  '三种写法都支持:中文文案(模型不可用)、英文词语(model not found)、状态码(status code: 404)。不建议加入:insufficient_user_quota(余额问题)、ReasoningEffort invalid(请求参数写错,不是模型不可用)。',
                )}
              </Text>
            </Col>
          </Row>
          <Row gutter={16}>
            <Col xs={24} sm={16}>
              <Form.TextArea
                field={'AutoModelTransientKeywords'}
                label={t('普通冷却关键词(强制降级为抖动失败)')}
                placeholder={t('一行一个,不区分大小写;中文文案、英文词语、状态码都可以')}
                extraText={t(
                  '优先级最高:命中后这次失败只按普通抖动冷却处理(以「普通冷却起始时间」起步的阶梯),绝不升级成确定性冷却档、也不会删除模型。用来把其实会自愈的错误钉在这一档,防止被上面的确定性判据误伤。上文已预填本机在用的判据。',
                )}
                autosize={{ minRows: 6, maxRows: 14 }}
                onChange={(value) =>
                  setInputs({ ...inputs, AutoModelTransientKeywords: value })
                }
              />
              <Text type='tertiary'>
                {t(
                  '本机预填:rpm exhausted、token plan entitlement exhausted、all candidates failed、upstream_response_timeout。',
                )}
              </Text>
              <Text type='tertiary' size='small' style={{ display: 'block', marginTop: 4 }}>
                {t(
                  '520/524 等上游网关故障已由后端按状态码直接判为确定性失败(冷却 → 再犯摘除),不要写在这里:那会把它们钉回抖动档,永远删不掉。',
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