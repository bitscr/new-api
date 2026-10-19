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

import React, { useEffect, useMemo, useRef, useState } from 'react';
import {
  Button,
  Card,
  Input,
  Popconfirm,
  Select,
  Spin,
  Table,
  Tag,
  Typography,
} from '@douyinfe/semi-ui';
import { API, isRoot, showError, showSuccess } from '../../helpers';
import { useTranslation } from 'react-i18next';

const { Text } = Typography;

const DEFAULT_REFRESH_SECONDS = 60;

function formatScore(score) {
  if (score === undefined || score === null || Number.isNaN(Number(score))) {
    return '-';
  }
  return Number(score).toFixed(4);
}

function scoreColor(score) {
  const value = Number(score);
  if (Number.isNaN(value)) return undefined;
  if (value >= 0.7) return 'var(--semi-color-success)';
  if (value <= 0.3) return 'var(--semi-color-danger)';
  return 'var(--semi-color-warning)';
}

function formatTime(unixSeconds) {
  if (!unixSeconds) return '-';
  const date = new Date(Number(unixSeconds) * 1000);
  if (Number.isNaN(date.getTime())) return '-';
  return date.toLocaleString();
}

const ModelScores = () => {
  const { t } = useTranslation();
  const [loading, setLoading] = useState(true);
  const [rows, setRows] = useState([]);
  const [refreshSeconds, setRefreshSeconds] = useState(DEFAULT_REFRESH_SECONDS);
  const [keyword, setKeyword] = useState('');
  const [group, setGroup] = useState('');
  const [pageSize, setPageSize] = useState(20);
  const timerRef = useRef();

  const load = async (showSpinner = true) => {
    if (showSpinner) setLoading(true);
    try {
      const res = await API.get('/api/auto_model_scores');
      const { success, data, refresh_seconds, message } = res.data;
      if (!success) {
        showError(message || t('加载模型得分失败'));
        return;
      }
      setRows(Array.isArray(data) ? data : []);
      const seconds = Number(refresh_seconds);
      if (Number.isInteger(seconds) && seconds >= 1) {
        setRefreshSeconds(seconds);
      }
    } catch (error) {
      showError(t('加载模型得分失败'));
    } finally {
      if (showSpinner) setLoading(false);
    }
  };

  useEffect(() => {
    load(true);
    // 首次拿到 refresh_seconds 后按该间隔轮询;后续以最近一次返回值为准。
    return () => {
      if (timerRef.current) clearInterval(timerRef.current);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    if (timerRef.current) clearInterval(timerRef.current);
    timerRef.current = setInterval(() => load(false), refreshSeconds * 1000);
    return () => {
      if (timerRef.current) clearInterval(timerRef.current);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [refreshSeconds]);

  // 一键清除某个 (分组, 模型, 渠道) 组合的负面效果:得分恢复中立 0.5,冷却解除。
  // 仅 root 可用:接口挂在 /api/option/ 的 RootAuth 路由组下。
  const clearNegative = async (record) => {
    try {
      const res = await API.delete('/api/option/auto_model_scores', {
        params: {
          group: record.group,
          model: record.model,
          channel_id: record.channel_id,
        },
      });
      const { success, message } = res.data;
      if (!success) {
        showError(message || t('清除失败'));
        return;
      }
      showSuccess(t('已恢复中立得分'));
      await load(false);
    } catch (error) {
      showError(t('清除失败'));
    }
  };

  const groupOptions = useMemo(() => {
    const set = new Set(rows.map((row) => row.group).filter(Boolean));
    return Array.from(set).map((value) => ({ value, label: value }));
  }, [rows]);

  const filteredRows = useMemo(() => {
    const kw = keyword.trim().toLowerCase();
    return rows.filter((row) => {
      if (group && row.group !== group) return false;
      if (!kw) return true;
      return String(row.model || '')
        .toLowerCase()
        .includes(kw);
    });
  }, [rows, keyword, group]);

  const columns = [
    {
      title: t('得分'),
      dataIndex: 'score',
      sorter: (a, b) => (Number(a.score) || 0) - (Number(b.score) || 0),
      defaultSortOrder: 'descend',
      render: (value) => (
        <Text style={{ color: scoreColor(value), fontWeight: 600 }}>
          {formatScore(value)}
        </Text>
      ),
    },
    {
      title: t('模型'),
      dataIndex: 'model',
      render: (value) => <span className='font-medium'>{value || '-'}</span>,
    },
    {
      title: t('分组'),
      dataIndex: 'group',
    },
    {
      title: t('渠道'),
      dataIndex: 'channel_id',
      width: 90,
    },
    {
      title: t('延迟(ms)'),
      dataIndex: 'latency_ms',
      sorter: (a, b) =>
        (Number(a.latency_ms) || 0) - (Number(b.latency_ms) || 0),
      render: (value) =>
        value === undefined || value === null ? '-' : Math.round(Number(value)),
    },
    {
      title: t('观察次数'),
      dataIndex: 'observations',
      sorter: (a, b) =>
        (Number(a.observations) || 0) - (Number(b.observations) || 0),
      render: (value) =>
        value === undefined || value === null ? '-' : Math.round(Number(value)),
    },
    {
      title: t('冷却'),
      dataIndex: 'cooling',
      render: (value, record) => {
        if (!value) return <Tag color='grey'>{t('正常')}</Tag>;
        const text = t('冷却 Lv.{{level}}', { level: record.cooldown_level });
        return <Tag color='orange'>{text}</Tag>;
      },
    },
    {
      title: t('更新时间'),
      dataIndex: 'updated_at',
      render: (value) => formatTime(value),
    },
  ];

  // 只有 root 能清除负面效果;普通用户看不到这一列。
  if (isRoot()) {
    columns.push({
      title: t('操作'),
      dataIndex: 'op',
      render: (_, record) => (
        <Popconfirm
          title={t('确定清除该组合的负面效果？')}
          content={t('得分将恢复中立 0.5,并解除冷却')}
          onConfirm={() => clearNegative(record)}
        >
          <Button size='small'>{t('清除负面效果')}</Button>
        </Popconfirm>
      ),
    });
  }

  return (
    <div className='mt-[60px] px-2'>
      <Card
        title={t('模型得分排序')}
        headerExtraContent={
          <Text type='tertiary'>
            {t('每 {{seconds}} 秒自动刷新', { seconds: refreshSeconds })}
          </Text>
        }
      >
        <div
          style={{
            display: 'flex',
            gap: 12,
            marginBottom: 16,
            flexWrap: 'wrap',
          }}
        >
          <Input
            value={keyword}
            onChange={setKeyword}
            placeholder={t('搜索模型名')}
            style={{ width: 240 }}
            showClear
          />
          <Select
            value={group}
            onChange={setGroup}
            placeholder={t('按分组筛选')}
            optionList={groupOptions}
            style={{ width: 200 }}
            showClear
          />
        </div>
        <Spin spinning={loading}>
          <Table
            columns={columns}
            dataSource={filteredRows}
            pagination={{
              pageSize,
              pageSizeOpts: [10, 20, 50, 100],
              showSizeChanger: true,
              onPageSizeChange: setPageSize,
            }}
            rowKey={(record) =>
              `${record.group}\u0000${record.model}\u0000${record.channel_id}`
            }
            empty={t('暂无模型得分数据')}
          />
        </Spin>
      </Card>
    </div>
  );
};

export default ModelScores;
