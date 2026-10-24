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
import {
  API,
  isRoot,
  showError,
  showSuccess,
  showWarning,
} from '../../helpers';
import { useTranslation } from 'react-i18next';

const { Text } = Typography;

const DEFAULT_REFRESH_SECONDS = 60;

function formatPercent(value) {
  if (value === undefined || value === null || Number.isNaN(Number(value))) {
    return '-';
  }
  return `${(Number(value) * 100).toFixed(1)}%`;
}

function formatSamples(value) {
  const count = Number(value);
  if (!Number.isFinite(count) || count <= 0) return '0';
  return count >= 10 ? String(Math.round(count)) : count.toFixed(1);
}

function formatLatency(latency, samples) {
  if (!samples || !latency) return '-';
  return `${Math.round(Number(latency))}ms · ${formatSamples(samples)}`;
}

function formatCount(value) {
  if (value === undefined || value === null) return '-';
  const count = Number(value);
  if (!Number.isFinite(count)) return '-';
  if (count <= 0) return '0';
  return count >= 10 ? String(Math.round(count)) : count.toFixed(1);
}

function categoryColor(category) {
  switch (category) {
    case 'main':
    case 'reliable':
      return 'green';
    case 'backup':
    case 'successful_trial':
      return 'blue';
    case 'pending_recovery':
      return 'orange';
    case 'cooling':
    case 'unreliable':
      return 'red';
    default:
      return 'grey';
  }
}

function formatTime(unixSeconds) {
  if (!unixSeconds) return '-';
  const date = new Date(Number(unixSeconds) * 1000);
  if (Number.isNaN(date.getTime())) return '-';
  return date.toLocaleString();
}

function scoreRowKey(record) {
  return `${record.group}\u0000${record.model}\u0000${record.channel_id}`;
}

const ModelScores = () => {
  const { t } = useTranslation();
  const [loading, setLoading] = useState(true);
  const [rows, setRows] = useState([]);
  const [refreshSeconds, setRefreshSeconds] = useState(DEFAULT_REFRESH_SECONDS);
  const [keyword, setKeyword] = useState('');
  const [group, setGroup] = useState('');
  const [channel, setChannel] = useState('');
  const [streamMode, setStreamMode] = useState(false);
  const [pageSize, setPageSize] = useState(20);
  const [currentPage, setCurrentPage] = useState(1);
  const [selectedRowKeys, setSelectedRowKeys] = useState([]);
  const [batchClearing, setBatchClearing] = useState(false);
  const timerRef = useRef();
  const loadSeq = useRef(0);
  // clear callbacks capture an old render's load closure. Read the current mode
  // from a ref so "clear then switch mode" refreshes the new mode, and a late
  // response for the old mode is discarded.
  const streamModeRef = useRef(streamMode);
  streamModeRef.current = streamMode;

  const load = async (showSpinner = true) => {
    const seq = ++loadSeq.current;
    const requestedStream = streamModeRef.current;
    const isCurrent = () =>
      seq === loadSeq.current && requestedStream === streamModeRef.current;
    if (showSpinner) setLoading(true);
    try {
      const res = await API.get('/api/auto_model_scores', {
        params: { stream: requestedStream ? 'true' : 'false' },
      });
      // 切换流式/非流式预览时，旧模式的迟到响应必须丢弃，不能覆盖新模式的表。
      if (!isCurrent()) return;
      const { success, data, refresh_seconds, message } = res.data;
      if (!success) {
        showError(message || t('加载路由状态失败'));
        return;
      }
      setRows(Array.isArray(data) ? data : []);
      const seconds = Number(refresh_seconds);
      if (Number.isInteger(seconds) && seconds >= 1) {
        setRefreshSeconds(seconds);
      }
    } catch (error) {
      if (isCurrent()) showError(t('加载路由状态失败'));
    } finally {
      if (isCurrent()) setLoading(false);
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
  }, [refreshSeconds, streamMode]);

  useEffect(() => {
    load(false);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [streamMode]);

  // 一键清除某个 (分组, 模型, 渠道) 组合的负面效果:清空证据并解除冷却。
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
      const { success, message, data } = res.data;
      if (!success) {
        if (data?.persistence_pending) {
          showWarning(message || t('内存证据已清空，数据库写入待重试'));
          await load(false);
        } else {
          showError(message || t('清除失败'));
        }
        return;
      }
      showSuccess(t('证据已清空，恢复为待观察'));
      await load(false);
    } catch (error) {
      showError(t('清除失败'));
    }
  };

  const groupOptions = useMemo(() => {
    const set = new Set(rows.map((row) => row.group).filter(Boolean));
    return Array.from(set).map((value) => ({ value, label: value }));
  }, [rows]);

  const channelOptions = useMemo(() => {
    const ids = new Set();
    rows.forEach((row) => {
      const id = Number(row.channel_id);
      if (Number.isInteger(id) && id > 0) ids.add(id);
    });
    return Array.from(ids)
      .sort((a, b) => a - b)
      .map((id) => ({ value: String(id), label: `#${id}` }));
  }, [rows]);

  const filteredRows = useMemo(() => {
    const kw = keyword.trim().toLowerCase();
    return rows.filter((row) => {
      if (group && row.group !== group) return false;
      if (channel && String(row.channel_id) !== String(channel)) return false;
      if (!kw) return true;
      return String(row.model || '')
        .toLowerCase()
        .includes(kw);
    });
  }, [rows, keyword, group, channel]);

  useEffect(() => {
    setCurrentPage(1);
    setSelectedRowKeys([]);
  }, [keyword, group, channel, streamMode]);

  const pageRows = useMemo(() => {
    const start = (currentPage - 1) * pageSize;
    return filteredRows.slice(start, start + pageSize);
  }, [filteredRows, currentPage, pageSize]);

  useEffect(() => {
    const maxPage = Math.max(1, Math.ceil(filteredRows.length / pageSize));
    if (currentPage > maxPage) setCurrentPage(maxPage);
  }, [filteredRows.length, pageSize, currentPage]);

  const selectedRecords = useMemo(() => {
    const byKey = new Map(rows.map((row) => [scoreRowKey(row), row]));
    return selectedRowKeys.map((key) => byKey.get(key)).filter(Boolean);
  }, [rows, selectedRowKeys]);

  useEffect(() => {
    const valid = new Set(rows.map(scoreRowKey));
    setSelectedRowKeys((previous) => {
      const next = previous.filter((key) => valid.has(key));
      return next.length === previous.length ? previous : next;
    });
  }, [rows]);

  const selectCurrentPage = () => {
    const keys = pageRows.map(scoreRowKey);
    setSelectedRowKeys(Array.from(new Set([...selectedRowKeys, ...keys])));
  };

  const clearSelectedNegative = async () => {
    if (!selectedRecords.length) return;
    setBatchClearing(true);
    try {
      let cleared = 0;
      let skipped = 0;
      let pending = 0;
      let pendingMessage = '';
      // The backend accepts up to 1000 rows; chunk anyway so a future select-all
      // over a large table cannot turn into one oversized request.
      for (let start = 0; start < selectedRecords.length; start += 200) {
        const items = selectedRecords.slice(start, start + 200).map((row) => ({
          group: row.group,
          model: row.model,
          channel_id: Number(row.channel_id),
        }));
        const res = await API.post('/api/option/auto_model_scores/batch', {
          items,
        });
        const { success, message, data } = res.data;
        const pendingCount = Number(data?.persistence_pending_count) || 0;
        cleared += Number(data?.cleared_count) || 0;
        skipped += Number(data?.skipped_count) || 0;
        pending += pendingCount;
        if (!success && pendingCount === 0) {
          showError(message || t('批量清除失败'));
          return;
        }
        if (pendingCount > 0 && !pendingMessage) {
          pendingMessage = message || '';
        }
      }
      if (pending > 0) {
        showWarning(
          pendingMessage || t('部分清除已在内存生效，数据库写入待重试'),
        );
      } else {
        showSuccess(
          t('已清除 {{cleared}} 条，跳过 {{skipped}} 条', {
            cleared,
            skipped,
          }),
        );
      }
      setSelectedRowKeys([]);
      await load(false);
    } catch (error) {
      showError(t('批量清除失败'));
    } finally {
      setBatchClearing(false);
    }
  };

  const columns = [
    { title: t('分组'), dataIndex: 'group', width: 110 },
    {
      title: t('模型'),
      dataIndex: 'model',
      render: (value) => <span className='font-medium'>{value || '-'}</span>,
    },
    { title: t('渠道'), dataIndex: 'channel_id', width: 80 },
    {
      title: t('优先级'),
      dataIndex: 'channel_priority',
      width: 80,
      render: (value) =>
        value === undefined || value === null ? '-' : Number(value),
    },
    {
      title: t('可靠性'),
      dataIndex: 'reliability',
      width: 100,
      render: (value, record) =>
        record.reliability_known ? (
          <Text
            style={{
              color: ['main', 'backup', 'reliable'].includes(record.category)
                ? 'var(--semi-color-success)'
                : 'var(--semi-color-danger)',
              fontWeight: 600,
            }}
          >
            {formatPercent(value)}
          </Text>
        ) : (
          '-'
        ),
    },
    {
      title: t('业务样本'),
      dataIndex: 'observations',
      width: 90,
      render: (value) => formatCount(value),
    },
    {
      title: t('流式延迟 / 有效样本'),
      dataIndex: 'stream_latency_ms',
      width: 170,
      render: (value, record) => (
        <div>
          <span>{formatLatency(value, record.stream_latency_samples)}</span>
          {Number(record.stream_latency_samples) > 0 &&
            !record.stream_latency_known && (
              <Tag color='orange' style={{ marginLeft: 6 }}>
                {record.stream_latency_fresh ? t('样本不足') : t('证据陈旧')}
              </Tag>
            )}
        </div>
      ),
    },
    {
      title: t('非流式耗时 / 有效样本'),
      dataIndex: 'non_stream_latency_ms',
      width: 180,
      render: (value, record) => (
        <div>
          <span>{formatLatency(value, record.non_stream_latency_samples)}</span>
          {Number(record.non_stream_latency_samples) > 0 &&
            !record.non_stream_latency_known && (
              <Tag color='orange' style={{ marginLeft: 6 }}>
                {record.non_stream_latency_fresh
                  ? t('样本不足')
                  : t('证据陈旧')}
              </Tag>
            )}
        </div>
      ),
    },
    {
      title: t('最后观测'),
      dataIndex: 'last_observed_at',
      width: 170,
      render: (value) => formatTime(value),
    },
    {
      title: t('路由状态'),
      dataIndex: 'category',
      width: 180,
      render: (value, record) => (
        <div>
          <Tag color={categoryColor(value)}>
            {record.reason || value || '-'}
          </Tag>
          {record.pending_recovery && value !== 'pending_recovery' && (
            <Tag color='orange' style={{ marginLeft: 6 }}>
              {t('待恢复')}
            </Tag>
          )}
          {record.cooling && (
            <Tag color='red' style={{ marginLeft: 6 }}>
              {t('冷却 Lv.{{level}}', { level: record.cooldown_level })}
            </Tag>
          )}
        </div>
      ),
    },
  ];

  if (isRoot()) {
    columns.push({
      title: t('操作'),
      dataIndex: 'op',
      width: 140,
      render: (_, record) => (
        <Popconfirm
          title={t('确定清空该组合的业务证据？')}
          content={t('证据将清空并恢复为待观察，不会生成成功样本')}
          onConfirm={() => clearNegative(record)}
        >
          <Button size='small'>{t('清空证据')}</Button>
        </Popconfirm>
      ),
    });
  }

  const rowSelection = isRoot()
    ? {
        selectedRowKeys,
        onChange: (keys) => setSelectedRowKeys(keys),
      }
    : undefined;

  return (
    <div className='mt-[60px] px-2'>
      <Card
        title={t('auto 路由状态')}
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
            alignItems: 'center',
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
          <Select
            value={channel}
            onChange={setChannel}
            placeholder={t('按渠道筛选')}
            optionList={channelOptions}
            style={{ width: 150 }}
            showClear
          />
          <Select
            value={streamMode ? 'stream' : 'non_stream'}
            onChange={(value) => {
              const next = value === 'stream';
              streamModeRef.current = next;
              setStreamMode(next);
            }}
            optionList={[
              { value: 'non_stream', label: t('非流式预览') },
              { value: 'stream', label: t('流式预览') },
            ]}
            style={{ width: 150 }}
          />
          {isRoot() && (
            <>
              <Button
                onClick={selectCurrentPage}
                disabled={pageRows.length === 0}
              >
                {t('全选本页')}
              </Button>
              <Button
                onClick={() => setSelectedRowKeys([])}
                disabled={selectedRowKeys.length === 0}
              >
                {t('清空选择')}
              </Button>
              <Popconfirm
                title={t('确定清空所选组合的业务证据？')}
                content={t('共 {{count}} 条，证据将清空并恢复为待观察', {
                  count: selectedRecords.length,
                })}
                onConfirm={clearSelectedNegative}
              >
                <Button
                  type='danger'
                  loading={batchClearing}
                  disabled={batchClearing || selectedRecords.length === 0}
                >
                  {selectedRecords.length > 0
                    ? t('批量清空证据 ({{count}})', {
                        count: selectedRecords.length,
                      })
                    : t('批量清空证据')}
                </Button>
              </Popconfirm>
            </>
          )}
        </div>
        <Spin spinning={loading}>
          <Table
            columns={columns}
            dataSource={filteredRows}
            rowSelection={rowSelection}
            pagination={{
              currentPage,
              pageSize,
              pageSizeOpts: [10, 20, 50, 100],
              showSizeChanger: true,
              onChange: (page, size) => {
                setCurrentPage(page);
                setPageSize(size);
              },
              onPageSizeChange: (size) => {
                setCurrentPage(1);
                setPageSize(size);
              },
            }}
            rowKey={scoreRowKey}
            empty={t('暂无路由状态数据')}
          />
        </Spin>
        <Text
          type='tertiary'
          size='small'
          style={{ display: 'block', marginTop: 8 }}
        >
          {t(
            '这里展示的是近期业务证据，不是模型能力排行；延迟样本与业务样本均为按衰减窗口折算后的有效值。auto 在既有授权和优先级内，先检查可靠性，再在成功延迟接近的候选池中按渠道/模型权重分流，并用少量真实业务请求补充其他候选的证据。页面不会发起上游请求，也不会占用探索或试用名额。',
          )}
        </Text>
      </Card>
    </div>
  );
};

export default ModelScores;
