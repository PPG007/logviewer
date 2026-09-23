// 本地缓存管理（M10）：查看占用与条目、逐条删除、清空、全局开关与上限。
//
// 缓存是加速手段，不是数据源：这里的所有操作都不影响已打开文件的浏览与检索，
// 清空后下次打开远端文件会重新拉取。

import { useCallback, useEffect, useState } from 'react'
import { App, Button, Empty, InputNumber, Modal, Popconfirm, Space, Switch, Table, Tooltip, Typography } from 'antd'
import { DeleteOutlined, ReloadOutlined } from '@ant-design/icons'
import dayjs from 'dayjs'
import { api, errText } from '../api'
import { refreshCacheInfo, useStore } from '../store/useStore'
import type { CacheEntryInfo } from '../types'

/** 人类可读的体积。 */
function humanSize(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  if (n < 1024 * 1024 * 1024) return `${(n / 1024 / 1024).toFixed(1)} MB`
  return `${(n / 1024 / 1024 / 1024).toFixed(2)} GB`
}

interface Props {
  open: boolean
  onClose: () => void
}

export default function CacheModal({ open, onClose }: Props) {
  const { message } = App.useApp()
  const info = useStore((s) => s.cacheInfo)
  const [loading, setLoading] = useState(false)
  const [busy, setBusy] = useState(false)
  // 上限以 MB 编辑（后端按字节存）
  const [limitMB, setLimitMB] = useState<number | null>(null)

  const load = useCallback(async () => {
    setLoading(true)
    try {
      await refreshCacheInfo()
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    if (!open) return
    load().catch(() => {})
  }, [open, load])

  useEffect(() => {
    if (info) setLimitMB(Math.round(info.Limit / 1024 / 1024))
  }, [info])

  const toggleEnabled = async (enabled: boolean) => {
    setBusy(true)
    try {
      await api.setCacheEnabled(enabled)
      await refreshCacheInfo()
      message.success(enabled ? '已启用本地缓存' : '已关闭本地缓存（已缓存的内容保留，可在此清空）')
    } catch (e) {
      message.error(`切换失败：${errText(e)}`)
    } finally {
      setBusy(false)
    }
  }

  const applyLimit = async () => {
    if (limitMB == null || limitMB < 0) {
      message.error('请输入不小于 0 的整数（0 表示不限制）')
      return
    }
    setBusy(true)
    try {
      await api.setCacheLimit(Math.round(limitMB) * 1024 * 1024)
      await refreshCacheInfo()
      message.success('已更新上限')
    } catch (e) {
      message.error(`更新上限失败：${errText(e)}`)
    } finally {
      setBusy(false)
    }
  }

  const removeOne = async (key: string) => {
    try {
      await api.removeCacheEntry(key)
      await refreshCacheInfo()
    } catch (e) {
      message.error(`删除失败：${errText(e)}`)
    }
  }

  const clearAll = async () => {
    setBusy(true)
    try {
      await api.clearCache()
      await refreshCacheInfo()
      message.success('已清空本地缓存')
    } catch (e) {
      message.error(`清空失败：${errText(e)}`)
    } finally {
      setBusy(false)
    }
  }

  const entries = info?.Entries ?? []
  const inUse = entries.filter((e) => e.InUse).length

  return (
    <Modal
      title="本地缓存"
      open={open}
      width={880}
      footer={null}
      onCancel={onClose}
      styles={{ body: { paddingTop: 8 } }}
    >
      <Space orientation="vertical" size={10} style={{ width: '100%' }}>
        <Space size={16} wrap>
          <Space size={6}>
            <Switch
              checked={info?.Enabled ?? false}
              disabled={busy || !info?.Available}
              onChange={(v) => toggleEnabled(v).catch(() => {})}
            />
            <Typography.Text>启用本地缓存</Typography.Text>
          </Space>
          <Space size={6}>
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              上限（MB，0 = 不限制）
            </Typography.Text>
            <InputNumber
              size="small"
              style={{ width: 100 }}
              min={0}
              value={limitMB ?? 0}
              onChange={(v) => setLimitMB(v == null ? null : Number(v))}
            />
            <Button size="small" disabled={busy} onClick={() => applyLimit().catch(() => {})}>
              应用
            </Button>
          </Space>
          <Button size="small" icon={<ReloadOutlined />} loading={loading} onClick={() => load()}>
            刷新
          </Button>
          <Popconfirm
            title="清空本地缓存？"
            description="仅删除本机缓存，不影响远端文件；下次打开会重新拉取"
            okText="清空"
            okButtonProps={{ danger: true }}
            cancelText="取消"
            onConfirm={() => clearAll().catch(() => {})}
          >
            <Button size="small" danger icon={<DeleteOutlined />} disabled={busy || entries.length === 0}>
              清空
            </Button>
          </Popconfirm>
        </Space>

        <Typography.Text type="secondary" style={{ fontSize: 12 }}>
          {info?.Available
            ? `共 ${entries.length} 项 · 占用 ${humanSize(info?.TotalBytes ?? 0)}` +
              (inUse > 0 ? ` · ${inUse} 项正在使用（清空时跳过）` : '')
            : '缓存不可用（历史记录数据库未能初始化）'}
          {info?.Dir ? ` · 目录：${info.Dir}` : ''}
        </Typography.Text>

        <Typography.Text type="secondary" style={{ fontSize: 12 }}>
          远端文件第一次打开时会边读边缓存到本机；之后重新打开与翻页都直接读本地，
          不再走网络。远端内容变化（大小或修改时间）时整份重新拉取；日志内容可能含敏感信息，
          缓存目录在本机，可随时在此清空。
        </Typography.Text>

        <Table<CacheEntryInfo>
          size="small"
          rowKey="SourceKey"
          loading={loading}
          dataSource={entries}
          pagination={entries.length > 12 ? { pageSize: 12, size: 'small' } : false}
          locale={{ emptyText: <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="暂无缓存" /> }}
          columns={[
            {
              title: '文件',
              dataIndex: 'Name',
              render: (_, e) => (
                <Tooltip title={`${e.Remote}:${e.Path}`}>
                  <Space size={6}>
                    <Typography.Text style={{ fontSize: 13 }}>{e.Name}</Typography.Text>
                    {e.InUse && (
                      <Typography.Text type="secondary" style={{ fontSize: 11 }}>
                        使用中
                      </Typography.Text>
                    )}
                  </Space>
                </Tooltip>
              ),
            },
            {
              title: '主机',
              dataIndex: 'Remote',
              width: 190,
              render: (v: string) => (
                <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                  {v}
                </Typography.Text>
              ),
            },
            {
              title: '远端路径',
              dataIndex: 'Path',
              render: (v: string) => (
                <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                  {v}
                </Typography.Text>
              ),
            },
            {
              title: '大小',
              dataIndex: 'Size',
              width: 100,
              align: 'right',
              render: (v: number) => <Typography.Text style={{ fontSize: 12 }}>{humanSize(v)}</Typography.Text>,
            },
            {
              title: '最近使用',
              dataIndex: 'LastUsedAt',
              width: 160,
              render: (v: number) => (
                <Typography.Text style={{ fontSize: 12 }}>
                  {dayjs(v).format('YYYY-MM-DD HH:mm')}
                </Typography.Text>
              ),
            },
            {
              title: '',
              width: 48,
              render: (_, e) => (
                <Popconfirm
                  title="删除这条缓存？"
                  description="仅删除本机缓存，不影响远端文件"
                  okText="删除"
                  okButtonProps={{ danger: true }}
                  cancelText="取消"
                  onConfirm={() => removeOne(e.SourceKey).catch(() => {})}
                >
                  <Button type="text" size="small" danger icon={<DeleteOutlined />} />
                </Popconfirm>
              ),
            },
          ]}
        />

        <Space style={{ width: '100%', justifyContent: 'flex-end' }}>
          <Button onClick={onClose}>关闭</Button>
        </Space>
      </Space>
    </Modal>
  )
}
