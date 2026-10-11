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

import React, { useCallback, useEffect, useState } from 'react';
import { Button, Empty, Modal, Table, Tag, Typography } from '@douyinfe/semi-ui';
import { API, showError, showSuccess } from '../../../helpers';
import { useTranslation } from 'react-i18next';

const { Text } = Typography;

// 冷却剩余时长按人话展示:30 天这种数字直接给秒数没有意义。
// 单位必须走 t(),否则英文界面会显示成 "2天 3小时"。
function formatRemaining(t, seconds) {
  if (!Number.isFinite(seconds) || seconds <= 0) return '0';
  const days = Math.floor(seconds / 86400);
  const hours = Math.floor((seconds % 86400) / 3600);
  const minutes = Math.floor((seconds % 3600) / 60);
  if (days > 0) return t('{{days}} 天 {{hours}} 小时', { days, hours });
  if (hours > 0) return t('{{hours}} 小时 {{minutes}} 分', { hours, minutes });
  return t('{{minutes}} 分', { minutes });
}

/**
 * auto 冷却列表:管理员需要看到 auto 当前挡住了谁、为什么、还要多久。
 *
 * 为什么必须有:冷却阶梯是自我强化的(24h 起、翻倍、封顶 30 天),而唯一的自动
 * 清除条件是"一次 20 秒内的快速成功"——冷却中的组合不会被路由,所以它只能等窗口
 * 自然到期。没有这个面板,加错一个判据就等于给一个组合上了 30 天封条且解不开。
 */
export default function SettingsAutoModelCooldown() {
  const { t } = useTranslation();
  const [loading, setLoading] = useState(false);
  const [rows, setRows] = useState([]);

  const refresh = useCallback(async () => {
    setLoading(true);
    try {
      const res = await API.get('/api/option/auto_model_cooldowns', {
        disableDuplicate: true,
      });
      const { success, message, data } = res.data;
      if (!success) {
        showError(t(message));
        return;
      }
      setRows(Array.isArray(data) ? data : []);
    } catch (e) {
      showError(t('加载 auto 冷却列表失败'));
    } finally {
      setLoading(false);
    }
  }, [t]);

  useEffect(() => {
    refresh();
  }, [refresh]);

  const confirmRemove = (row) => {
    Modal.confirm({
      title: t('确认解除该冷却'),
      content: (
        <div style={{ lineHeight: '1.6' }}>
          <Text>{t('解除后 auto 会重新尝试这个组合。')}</Text>
          <br />
          <Text strong>
            {row.group} / {row.model} / #{row.channel_id}
          </Text>
        </div>
      ),
      onOk: async () => {
        try {
          const res = await API.delete('/api/option/auto_model_cooldowns', {
            params: {
              group: row.group,
              model: row.model,
              channel_id: row.channel_id,
            },
          });
          const { success, message } = res.data;
          if (!success) {
            showError(t(message));
            return;
          }
          showSuccess(t('已解除'));
          await refresh();
        } catch (e) {
          showError(t('解除冷却失败'));
        }
      },
    });
  };

  const columns = [
    {
      title: t('分组'),
      dataIndex: 'group',
    },
    {
      title: t('模型'),
      dataIndex: 'model',
    },
    {
      title: t('渠道'),
      dataIndex: 'channel_id',
      render: (value) => `#${value}`,
    },
    {
      title: t('类型'),
      dataIndex: 'permanent',
      render: (permanent) =>
        permanent ? (
          <Tag color='red'>{t('确定性失败(长冷却)')}</Tag>
        ) : (
          <Tag color='orange'>{t('抖动失败(短冷却)')}</Tag>
        ),
    },
    {
      title: t('剩余时间'),
      dataIndex: 'remaining_seconds',
      render: (value) => formatRemaining(t, value),
    },
    {
      title: t('等级'),
      dataIndex: 'level',
    },
    {
      title: t('原因'),
      dataIndex: 'reason',
      render: (value) => <Text ellipsis={{ showTooltip: true }}>{value}</Text>,
    },
    {
      title: '',
      dataIndex: 'op',
      render: (_, row) => (
        <Button
          size='small'
          type='danger'
          theme='borderless'
          onClick={() => confirmRemove(row)}
        >
          {t('解除')}
        </Button>
      ),
    },
  ];

  return (
    <>
      <Text type='tertiary'>
        {t(
          'auto 当前正在避开的组合。冷却按“模型 + 渠道”记录,不影响渠道的其它模型,也不会禁用渠道。',
        )}
      </Text>
      <div style={{ margin: '12px 0' }}>
        <Button size='default' onClick={refresh} loading={loading}>
          {t('刷新')}
        </Button>
      </div>
      <Table
        columns={columns}
        dataSource={rows}
        rowKey={(row) => `${row.group}/${row.model}/${row.channel_id}`}
        pagination={false}
        empty={<Empty description={t('当前没有组合处于冷却')} />}
      />
    </>
  );
}