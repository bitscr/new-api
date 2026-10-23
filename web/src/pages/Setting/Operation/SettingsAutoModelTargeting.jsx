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
  Switch,
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

const OPTION_KEYS = [
  'AutoModelEnabled',
  'AutoModelCandidates',
  'AutoModelWeights',
  'AutoModelReliabilityThreshold',
  'AutoModelMinObservations',
  'AutoModelLatencyToleranceRatio',
  'AutoModelLatencyToleranceMs',
  'AutoModelExploreRatio',
  'AutoModelExploreMaxRatio',
];

/**
 * auto 的候选范围、组内权重与渠道排序。
 *
 * 前两个值决定 auto「可以选谁」,比冷却和重试预算更靠前:配错就是直接改变选路结果。
 * 所以这里不提供可视化点选,而是 JSON 文本 + 保存前逐条校验,与分组倍率设置的手动
 * 编辑保持一致——半成品的选择器比一个校验良好的文本框更容易出错。
 *
 * 最需要小心的一点(界面上直接标出来):
 *   分组名**不出现**在 JSON 里 = 该分组不限制,用它的全部启用模型;
 *   分组名出现但值是空数组 = 该分组一个模型都不选,auto 在该分组下无路可走。
 * 两者在原始 JSON 里看起来很像,后果完全相反。
 *
 * 路由在渠道 priority 硬边界内执行:近期可靠性达到门槛、成功延迟样本充足的路线
 * 进入主流量;成功延迟接近的路线共同竞争并按渠道/模型权重随机;可靠性不足、样本
 * 不足、证据陈旧、待恢复或明显较慢的路线进入试用/探索范围。探索只使用真实业务
 * 请求,不会主动测活。「模型得分」页已改为 auto 路由状态,展示这些证据和原因。
 */
export default function SettingsAutoModelTargeting(props) {
  const { t } = useTranslation();
  const [loading, setLoading] = useState(false);
  const [inputs, setInputs] = useState({
    AutoModelEnabled: 'true',
    AutoModelCandidates: '',
    AutoModelWeights: '',
    AutoModelReliabilityThreshold: '0.95',
    AutoModelMinObservations: '5',
    AutoModelLatencyToleranceRatio: '0.20',
    AutoModelLatencyToleranceMs: '250',
    AutoModelExploreRatio: '0.05',
    AutoModelExploreMaxRatio: '0.10',
  });
  const refForm = useRef();
  const [inputsRow, setInputsRow] = useState(inputs);

  // 与后端 controller/option.go 的校验保持一致:这里提前报错只是为了少一次往返,
  // 真正的权威检查仍在服务端。
  const parseCandidates = (value, key) => {
    if (!value || !String(value).trim()) return null; // 空 = 清空配置,全部模型回退默认
    const parsed = JSON.parse(value);
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
      throw new Error(t('必须是一个 JSON 对象:键为分组名,值为模型名数组'));
    }
    for (const [group, names] of Object.entries(parsed)) {
      if (!String(group).trim()) throw new Error(t('分组名不能为空'));
      if (
        !Array.isArray(names) ||
        names.some((name) => typeof name !== 'string')
      ) {
        throw new Error(t('分组「{{group}}」的值必须是字符串数组', { group }));
      }
      if (names.some((name) => !String(name).trim())) {
        throw new Error(t('分组「{{group}}」里有空的模型名', { group }));
      }
      if (names.includes('auto')) {
        throw new Error(t('候选列表不能包含 auto 本身'));
      }
    }
    return parsed;
  };

  const parseWeights = (value, key) => {
    if (!value || !String(value).trim()) return null;
    const parsed = JSON.parse(value);
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
      throw new Error(
        t('必须是一个 JSON 对象,例如 {"default": {"gpt-4o": 2}}'),
      );
    }
    for (const [group, models] of Object.entries(parsed)) {
      if (!String(group).trim()) throw new Error(t('分组名不能为空'));
      if (!models || typeof models !== 'object' || Array.isArray(models)) {
        throw new Error(
          t('分组「{{group}}」的值必须是「模型名: 权重」对象', { group }),
        );
      }
      for (const [model, weight] of Object.entries(models)) {
        if (!String(model).trim()) throw new Error(t('模型名不能为空'));
        if (
          typeof weight !== 'number' ||
          !Number.isFinite(weight) ||
          weight < 0
        ) {
          throw new Error(
            t(
              '分组「{{group}}」的模型「{{model}}」权重必须是不小于 0 的有限数字',
              {
                group,
                model,
              },
            ),
          );
        }
      }
    }
    return parsed;
  };

  const parseNumber = (value, { min, max, label }) => {
    const raw = String(value ?? '').trim();
    if (!raw) throw new Error(t('不能为空'));
    const parsed = Number(raw);
    if (!Number.isFinite(parsed)) throw new Error(t('必须是数字'));
    if (parsed < min || (max !== undefined && parsed > max)) {
      throw new Error(label);
    }
    return parsed;
  };

  const validators = {
    AutoModelCandidates: parseCandidates,
    AutoModelWeights: parseWeights,
    AutoModelReliabilityThreshold: (value) =>
      parseNumber(value, {
        min: 0,
        max: 1,
        label: t('可靠性门槛必须在 0～1 之间'),
      }),
    AutoModelMinObservations: (value) => {
      const parsed = parseNumber(value, {
        min: 1,
        max: 70,
        label: t('最低业务观测数必须是 1～70 的整数'),
      });
      if (!Number.isInteger(parsed)) throw new Error(t('必须是整数'));
      return parsed;
    },
    AutoModelLatencyToleranceRatio: (value) =>
      parseNumber(value, {
        min: 0,
        label: t('延迟相对容差必须是非负有限数字'),
      }),
    AutoModelLatencyToleranceMs: (value) => {
      const parsed = parseNumber(value, {
        min: 0,
        label: t('延迟绝对容差不能为负数'),
      });
      if (!Number.isInteger(parsed)) throw new Error(t('必须是整数'));
      return parsed;
    },
    AutoModelExploreRatio: (value) =>
      parseNumber(value, {
        min: 0,
        max: 0.1,
        label: t('探索比例必须满足 0 ≤ 基础比例 ≤ 最高比例 ≤ 0.10'),
      }),
    AutoModelExploreMaxRatio: (value) =>
      parseNumber(value, {
        min: 0,
        max: 0.1,
        label: t('探索比例必须满足 0 ≤ 基础比例 ≤ 最高比例 ≤ 0.10'),
      }),
  };

  const validate = (key, value) => {
    // 没有校验器的键(如 AutoModelEnabled 这种开关)一律放行。
    // 曾经这里无条件调用 validators[key],于是单改开关会抛
    // "validators[key] is not a function" 并把保存挡下来。
    const validator = validators[key];
    if (!validator) return '';
    try {
      validator(value, key);
      if (
        key === 'AutoModelExploreRatio' ||
        key === 'AutoModelExploreMaxRatio'
      ) {
        const base = Number(
          key === 'AutoModelExploreRatio'
            ? value
            : inputs.AutoModelExploreRatio,
        );
        const maximum = Number(
          key === 'AutoModelExploreMaxRatio'
            ? value
            : inputs.AutoModelExploreMaxRatio,
        );
        if (base > maximum) {
          return t('探索比例必须满足 0 ≤ 基础比例 ≤ 最高比例 ≤ 0.10');
        }
      }
      return '';
    } catch (e) {
      return e instanceof SyntaxError ? t('不是合法的 JSON 字符串') : e.message;
    }
  };

  const onSubmit = () => {
    const updateArray = compareObjects(inputs, inputsRow);
    if (!updateArray.length) return showWarning(t('你似乎并没有修改什么'));

    for (const item of updateArray) {
      const message = validate(item.key, inputs[item.key]);
      if (message) return showError(`${item.key}: ${message}`);
    }

    const exploreKeys = ['AutoModelExploreRatio', 'AutoModelExploreMaxRatio'];
    const exploreChanged = updateArray.some((item) =>
      exploreKeys.includes(item.key),
    );
    // 两个探索比例相互约束：无论改了哪一个，都把当前组合作为一次请求提交，
    // 后端在同一事务里校验并保存。拆成两个单键请求会因先后顺序被拒绝。
    const requestQueue = updateArray
      .filter((item) => !exploreKeys.includes(item.key))
      .map((item) =>
        API.put('/api/option/', {
          key: item.key,
          value: String(inputs[item.key] ?? ''),
        }),
      );
    if (exploreChanged) {
      requestQueue.push(
        API.put('/api/option/auto_model_explore_ratios', {
          base: String(inputs.AutoModelExploreRatio ?? ''),
          max: String(inputs.AutoModelExploreMaxRatio ?? ''),
        }),
      );
    }
    setLoading(true);
    Promise.all(requestQueue)
      .then((res) => {
        // 后端校验失败是 HTTP 200 + success:false,不是 HTTP 错误。
        const rejected = res.find(
          (item) => !item || !item.data || item.data.success === false,
        );
        if (rejected) {
          return showError(
            rejected && rejected.data && rejected.data.message
              ? rejected.data.message
              : t('保存失败，请重试'),
          );
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
    for (const key of OPTION_KEYS) {
      currentInputs[key] =
        props.options && props.options[key] !== undefined
          ? String(props.options[key])
          : inputs[key];
    }
    setInputs(currentInputs);
    setInputsRow(structuredClone(currentInputs));
    if (refForm.current) refForm.current.setValues(currentInputs);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [props.options]);

  // 空数组是一个容易被误读的状态,单独提示出来。
  const emptyGroups = (() => {
    try {
      const parsed = parseCandidates(
        inputs.AutoModelCandidates,
        'AutoModelCandidates',
      );
      return parsed
        ? Object.entries(parsed)
            .filter(([, names]) => names.length === 0)
            .map(([group]) => group)
        : [];
    } catch {
      return [];
    }
  })();

  return (
    <Spin spinning={loading}>
      <Form
        values={inputs}
        getFormApi={(formAPI) => (refForm.current = formAPI)}
        style={{ marginBottom: 15 }}
      >
        <Form.Section text={t('auto 候选范围、权重与渠道排序')}>
          <Banner
            type='warning'
            description={t(
              '这里每一项都会直接改变 auto 的选路结果。改错会立刻生效,请先确认分组与模型名确实存在。',
            )}
            style={{ marginBottom: 16 }}
          />
          <Row gutter={16}>
            <Col xs={24} sm={16}>
              <Form.TextArea
                field={'AutoModelCandidates'}
                label={t('候选模型白名单(按分组)')}
                placeholder={
                  '{"default": ["gpt-4o", "claude-sonnet-4"], "vip": ["gpt-4o"]}'
                }
                extraText={t(
                  '留空 = 不限制,每个分组都用它全部启用的模型(受计费配置过滤)。分组名不出现 = 该分组不限制;分组名出现但数组为空 = 该分组一个模型都不选。',
                )}
                autosize={{ minRows: 6, maxRows: 14 }}
                trigger='blur'
                rules={[
                  {
                    validator: (rule, value) =>
                      !validate('AutoModelCandidates', value),
                  },
                ]}
                onChange={(value) =>
                  setInputs({ ...inputs, AutoModelCandidates: value })
                }
              />
            </Col>
          </Row>
          {emptyGroups.length > 0 && (
            <Banner
              type='danger'
              description={t(
                '这些分组的候选列表是空的,auto 在它们下面将无路可走:{{groups}}',
                { groups: emptyGroups.join(', ') },
              )}
              style={{ marginBottom: 16 }}
            />
          )}
          <Row gutter={16}>
            <Col xs={24} sm={12}>
              <Form.Slot label={t('启用 auto 虚拟模型')}>
                <Switch
                  checked={inputs.AutoModelEnabled !== 'false'}
                  checkedText='|'
                  uncheckedText='〇'
                  onChange={(value) =>
                    setInputs({
                      ...inputs,
                      AutoModelEnabled: value ? 'true' : 'false',
                    })
                  }
                />
                <Text
                  type='tertiary'
                  size='small'
                  style={{ display: 'block', marginTop: 4 }}
                >
                  {t('关闭后 model=auto 不再自动选路,命名模型的请求不受影响。')}
                </Text>
              </Form.Slot>
            </Col>
          </Row>
          <Row gutter={16} style={{ marginTop: 16 }}>
            <Col xs={24} sm={16}>
              <Form.TextArea
                field={'AutoModelWeights'}
                label={t('组内模型权重(按分组)')}
                placeholder={'{"default": {"gpt-4o": 2, "claude-sonnet-4": 1}}'}
                extraText={t(
                  '留空或未列出的模型 = 权重 1。权重 0 表示该模型不参与 auto(等价于从候选里去掉),数值越大越容易被选中。只影响同一渠道内的模型选择,不改变渠道权重。',
                )}
                autosize={{ minRows: 6, maxRows: 14 }}
                trigger='blur'
                rules={[
                  {
                    validator: (rule, value) =>
                      !validate('AutoModelWeights', value),
                  },
                ]}
                onChange={(value) =>
                  setInputs({ ...inputs, AutoModelWeights: value })
                }
              />
              <Text type='tertiary'>
                {t(
                  '示例:{"default": {"gpt-4o": 2}} 让 default 分组里 gpt-4o 的选中概率是其它同级模型的两倍。',
                )}
              </Text>
            </Col>
          </Row>
          <Row gutter={16} style={{ marginTop: 16 }}>
            <Col xs={24} sm={12} lg={8}>
              <Form.Input
                field={'AutoModelReliabilityThreshold'}
                label={t('可靠性门槛')}
                extraText={t(
                  '默认 0.95。近期成功率低于它的路线不进入主流量，只作为试用/后备。',
                )}
                onChange={(value) =>
                  setInputs({ ...inputs, AutoModelReliabilityThreshold: value })
                }
              />
            </Col>
            <Col xs={24} sm={12} lg={8}>
              <Form.Input
                field={'AutoModelMinObservations'}
                label={t('最低业务观测数')}
                extraText={t(
                  '默认 5，可设 1～70。少于该数量的路线只进入试用，不进入主流量。',
                )}
                onChange={(value) =>
                  setInputs({ ...inputs, AutoModelMinObservations: value })
                }
              />
            </Col>
            <Col xs={24} sm={12} lg={8}>
              <Form.Input
                field={'AutoModelLatencyToleranceRatio'}
                label={t('延迟相对容差')}
                extraText={t(
                  '默认 0.20。候选池宽度取 max(绝对容差, 最低延迟 × 该比例)。',
                )}
                onChange={(value) =>
                  setInputs({
                    ...inputs,
                    AutoModelLatencyToleranceRatio: value,
                  })
                }
              />
            </Col>
          </Row>
          <Row gutter={16} style={{ marginTop: 16 }}>
            <Col xs={24} sm={12} lg={8}>
              <Form.Input
                field={'AutoModelLatencyToleranceMs'}
                label={t('延迟绝对容差(ms)')}
                extraText={t(
                  '默认 250ms。成功延迟相差不超过该值的路线共同竞争。',
                )}
                onChange={(value) =>
                  setInputs({ ...inputs, AutoModelLatencyToleranceMs: value })
                }
              />
            </Col>
            <Col xs={24} sm={12} lg={8}>
              <Form.Input
                field={'AutoModelExploreRatio'}
                label={t('基础探索比例')}
                extraText={t(
                  '默认 0.05。用于给主池之外的候选补充真实业务证据。',
                )}
                onChange={(value) =>
                  setInputs({ ...inputs, AutoModelExploreRatio: value })
                }
              />
            </Col>
            <Col xs={24} sm={12} lg={8}>
              <Form.Input
                field={'AutoModelExploreMaxRatio'}
                label={t('最高探索比例')}
                extraText={t(
                  '默认 0.10。待补充证据候选越多，探索比例越接近该值。',
                )}
                onChange={(value) =>
                  setInputs({ ...inputs, AutoModelExploreMaxRatio: value })
                }
              />
            </Col>
          </Row>
          <Text
            type='tertiary'
            size='small'
            style={{ display: 'block', marginTop: 12 }}
          >
            {t(
              '主流量顺序：渠道 priority 降序 → 可靠性准入 → 成功延迟接近的候选池 → 池内渠道/模型权重随机。可靠性不足、样本不足、证据陈旧、待恢复或明显较慢的路线进入试用/探索范围；探索只使用真实业务请求，不会主动测活。',
            )}
          </Text>
          <Row style={{ marginTop: 12 }}>
            <Button size='default' onClick={onSubmit}>
              {t('保存 auto 选路设置')}
            </Button>
          </Row>
        </Form.Section>
      </Form>
    </Spin>
  );
}
