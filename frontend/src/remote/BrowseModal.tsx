// 远端目录浏览器（M9/M10）：面包屑 + 路径输入 + 目录列表，选中文件后打开。
// 已在本机有内容缓存的文件会标出来（打开与后续翻页都不再走网络）。
//
// 交互：双击目录进入、双击文件打开；单击文件选中后按「打开」。
// 目录项由后端排序（目录优先、同级按名称），前端不再重排，避免两处规则漂移。

import { useCallback, useEffect, useMemo, useState } from 'react'
import {
  Alert,
  App,
  Breadcrumb,
  Button,
  Empty,
  Input,
  Modal,
  Space,
  Switch,
  Table,
  Tag,
  Tooltip,
  Typography,
} from 'antd'
import { FileOutlined, FileTextOutlined, FolderOutlined, ReloadOutlined, UpOutlined } from '@ant-design/icons'
import dayjs from 'dayjs'
import { api, errText } from '../api'
import { openRemoteFile, refreshCacheInfo, refreshConnections, remoteCacheKey, useStore } from '../store/useStore'
import type { Connection, RemoteEntry, RemoteListing } from '../types'

/** 日志文件扩展名（「只显示日志文件」过滤用）。 */
const LOG_EXTS = ['.log', '.jsonl', '.txt', '.json', '.out', '.err', '.trace']
/** 快捷跳转目录。 */
const QUICK_DIRS = ['/var/log', '/tmp']

function isLogFile(name: string): boolean {
  const lower = name.toLowerCase()
  return LOG_EXTS.some((e) => lower.endsWith(e))
}

/** 人类可读的体积。 */
function humanSize(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  if (n < 1024 * 1024 * 1024) return `${(n / 1024 / 1024).toFixed(1)} MB`
  return `${(n / 1024 / 1024 / 1024).toFixed(2)} GB`
}

/** 面包屑分段：/a/b/c → [{title:'/', path:'/'}, {title:'a', path:'/a'}, …] */
function crumbs(p: string): { title: string; path: string }[] {
  const parts = p.split('/').filter(Boolean)
  const out = [{ title: '/', path: '/' }]
  let acc = ''
  for (const s of parts) {
    acc += `/${s}`
    out.push({ title: s, path: acc })
  }
  return out
}

interface Props {
  open: boolean
  connection: Connection | null
  onClose: () => void
  /** 成功打开文件后回调（父组件据此刷新列表/收起弹窗）。 */
  onOpened: () => void
}

export default function BrowseModal({ open, connection, onClose, onOpened }: Props) {
  // 本机缓存的来源键用 user@host:port + 路径拼成，与后端 Source.Key() 同构
  const connLabel = connection ? `${connection.User}@${connection.Host}:${connection.Port}` : "" 
  const { message } = App.useApp()
  const cacheInfo = useStore((s) => s.cacheInfo)
  // 已缓存的来源键集合（判断某个远端文件本机是否已有内容缓存）
  const cachedKeys = useMemo(
    () => new Set((cacheInfo?.Entries ?? []).map((e) => e.SourceKey)),
    [cacheInfo],
  )
  const [listing, setListing] = useState<RemoteListing | null>(null)
  const [loading, setLoading] = useState(false)
  const [pathInput, setPathInput] = useState('')
  const [onlyLogs, setOnlyLogs] = useState(true)
  const [selected, setSelected] = useState<RemoteEntry | null>(null)
  const [opening, setOpening] = useState(false)

  // 打开失败的目录（权限不足、路径不存在等）：列表保留在原来的目录，
  // 失败信息显示在列表上方——用户既知道没进去，也知道自己还在哪。
  const [error, setError] = useState('')

  const load = useCallback(
    async (dir: string) => {
      if (!connection) return
      setLoading(true)
      try {
        const res = await api.listRemoteDir(connection.ID, dir)
        setListing(res)
        setPathInput(res.Path)
        setSelected(null)
        setError('')
      } catch (e) {
        setError(`无法打开 ${dir || '家目录'}：${errText(e)}`)
      } finally {
        setLoading(false)
      }
    },
    [connection],
  )

  // 打开时定位到上次浏览的目录（后端也会兜底）
  useEffect(() => {
    if (open && connection) {
      setListing(null)
      load(connection.LastDir || '').catch(() => {})
    }
  }, [open, connection, load])

  const visible = useMemo(() => {
    const all = listing?.Entries ?? []
    if (!onlyLogs) return all
    return all.filter((e) => e.IsDir || isLogFile(e.Name))
  }, [listing, onlyLogs])

  const hidden = (listing?.Entries.length ?? 0) - visible.length

  const doOpen = async (entry: RemoteEntry) => {
    if (!connection) return
    if (entry.IsDir) {
      await load(entry.Path)
      return
    }
    setOpening(true)
    try {
      const info = await openRemoteFile(connection.ID, entry.Path)
      await refreshConnections()
      await refreshCacheInfo() // 打开会写入/更新缓存，刷新标记与占用
      // 已打开过的文件：服务端内容有变化时会自动重新拉取（而不是沿用旧索引）
      message.success(
        info.Status === 'indexing'
          ? `文件有变化，正在重新拉取 ${entry.Name} 并重建索引…`
          : `已打开 ${entry.Name}`,
      )
      onOpened()
    } catch (e) {
      message.error(`打开失败：${errText(e)}`)
    } finally {
      setOpening(false)
    }
  }

  const go = (dir: string) => {
    if (!dir.trim()) {
      message.error('请输入要跳转的路径')
      return
    }
    load(dir.trim()).catch(() => {})
  }

  return (
    <Modal
      title={
        <Space>
          <span>浏览远端文件</span>
          {connection && (
            <Typography.Text type="secondary" style={{ fontSize: 12, fontWeight: 'normal' }}>
              {connection.User}@{connection.Host}:{connection.Port}
            </Typography.Text>
          )}
        </Space>
      }
      open={open}
      width={900}
      footer={null}
      onCancel={onClose}
      styles={{ body: { paddingTop: 8 } }}
    >
      <Space orientation="vertical" size={8} style={{ width: '100%' }}>
        <Space.Compact style={{ width: '100%' }}>
          <Button
            icon={<UpOutlined />}
            disabled={!listing?.Parent}
            onClick={() => listing?.Parent && load(listing.Parent)}
          >
            上级
          </Button>
          <Input
            value={pathInput}
            placeholder="远端绝对路径，如 /var/log"
            onChange={(e) => setPathInput(e.target.value)}
            onPressEnter={() => go(pathInput)}
          />
          <Button icon={<ReloadOutlined />} loading={loading} onClick={() => go(pathInput)}>
            转到
          </Button>
        </Space.Compact>

        <Space size={4} wrap>
          {listing?.Home && (
            <Button size="small" onClick={() => load(listing.Home)}>
              家目录
            </Button>
          )}
          {QUICK_DIRS.map((d) => (
            <Button key={d} size="small" onClick={() => load(d)}>
              {d}
            </Button>
          ))}
          <Button size="small" onClick={() => load('/')}>
            根目录
          </Button>
          <Tooltip title="隐藏非日志文件（.log/.jsonl/.txt/.json/.out/.err/.trace）">
            <Space size={4} style={{ marginLeft: 8 }}>
              <Switch size="small" checked={onlyLogs} onChange={setOnlyLogs} />
              <Typography.Text style={{ fontSize: 12 }}>只显示日志文件</Typography.Text>
            </Space>
          </Tooltip>
        </Space>

        {listing && listing.Path !== '/' && (
          <Breadcrumb
            items={crumbs(listing.Path).map((c) => ({
              title: (
                <a onClick={() => load(c.path)} key={c.path}>
                  {c.title}
                </a>
              ),
            }))}
          />
        )}

        {error && (
          <Alert
            type="warning"
            showIcon
            closable
            message={error}
            description={
              listing
                ? `当前仍停留在 ${listing.Path}；如需读取受限目录，请用有权限的账号（如 root 或加入 adm/syslog 组）连接。`
                : undefined
            }
            onClose={() => setError('')}
          />
        )}

        {hidden > 0 && (
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            已按扩展名过滤，隐藏 {hidden} 项（关闭上方开关可全部显示）
          </Typography.Text>
        )}

        <Table<RemoteEntry>
          size="small"
          rowKey="Path"
          loading={loading}
          dataSource={visible}
          pagination={false}
          scroll={{ y: 380 }}
          locale={{
            emptyText: listing ? (
              <Empty
                image={Empty.PRESENTED_IMAGE_SIMPLE}
                description={hidden > 0 ? '该目录下没有日志文件' : '该目录为空'}
              />
            ) : (
              <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="加载中…" />
            ),
          }}
          onRow={(entry) => ({
            onClick: () => setSelected(entry),
            onDoubleClick: () => doOpen(entry),
            style: { cursor: 'pointer' },
            className: selected?.Path === entry.Path ? 'lv-row-selected' : '',
          })}
          columns={[
            {
              title: '名称',
              dataIndex: 'Name',
              render: (_, e) => (
                <Space size={6}>
                  {e.IsDir ? (
                    <FolderOutlined style={{ color: '#faad14' }} />
                  ) : isLogFile(e.Name) ? (
                    <FileTextOutlined style={{ color: '#1677ff' }} />
                  ) : (
                    <FileOutlined style={{ color: 'var(--lv-close)' }} />
                  )}
                  <Typography.Text style={{ fontSize: 13 }}>{e.Name}</Typography.Text>
                  {!e.IsDir && cachedKeys.has(remoteCacheKey(connLabel, e.Path)) && (
                    <Tooltip title="本机已有内容缓存：打开与翻页都不再走网络">
                      <Tag color="purple" style={{ marginInlineEnd: 0, fontSize: 11, lineHeight: '16px' }}>
                        已缓存
                      </Tag>
                    </Tooltip>
                  )}
                </Space>
              ),
            },
            {
              title: '大小',
              dataIndex: 'Size',
              width: 110,
              align: 'right',
              render: (v: number, e) =>
                e.IsDir ? (
                  <Typography.Text type="secondary">—</Typography.Text>
                ) : (
                  <Typography.Text style={{ fontSize: 12 }}>{humanSize(v)}</Typography.Text>
                ),
            },
            {
              title: '修改时间',
              dataIndex: 'ModTime',
              width: 180,
              render: (v: number) => (
                <Typography.Text style={{ fontSize: 12 }}>
                  {dayjs(v).format('YYYY-MM-DD HH:mm:ss')}
                </Typography.Text>
              ),
            },
            {
              title: '权限',
              dataIndex: 'Mode',
              width: 120,
              render: (v: string) => (
                <Typography.Text type="secondary" style={{ fontSize: 12, fontFamily: 'monospace' }}>
                  {v}
                </Typography.Text>
              ),
            },
          ]}
        />

        <Space style={{ width: '100%', justifyContent: 'space-between' }}>
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            双击目录进入、双击文件打开；首次打开会边读边缓存到本机，之后打开与翻页直接读本地缓存
          </Typography.Text>
          <Space>
            <Button onClick={onClose}>取消</Button>
            <Button
              type="primary"
              loading={opening}
              disabled={!selected || selected.IsDir}
              onClick={() => selected && doOpen(selected)}
            >
              打开
            </Button>
          </Space>
        </Space>
      </Space>
    </Modal>
  )
}
