// 左侧文件列表（docs/functional-design.md §7）：上段是本次已打开的文件，
// 中间是「远程主机」（SQLite 持久化，点击浏览远端目录），
// 下段是「最近打开」（本地与远端文件混排，远端未连接时标「未连接」）；
// 底部「打开文件」+「新建连接」+「创建临时日志」入口；hover 关闭。
// 同名不同路径靠目录行区分，同名不同主机再靠 user@host 前缀区分。

import { useRef, useState } from 'react'
import {
  App,
  Button,
  Input,
  Layout,
  Modal,
  Popconfirm,
  Progress,
  Tag,
  Tooltip,
  Typography,
} from 'antd'
import {
  ApiOutlined,
  ClockCircleOutlined,
  CloseOutlined,
  CloudServerOutlined,
  DatabaseOutlined,
  DeleteOutlined,
  DisconnectOutlined,
  EditOutlined,
  FileAddOutlined,
  FileTextOutlined,
  FolderOpenOutlined,
  MenuFoldOutlined,
  MenuUnfoldOutlined,
  MoonOutlined,
  PlusOutlined,
  SunOutlined,
} from '@ant-design/icons'
import { api, errText, TEMP_LOG_MAX_BYTES, TEMP_LOG_MAX_LINES } from '../api'
import {
  clearRecentFiles,
  deleteConnection,
  findFile,
  openRecentFile,
  openRemoteFile,
  refreshConnections,
  refreshRecent,
  removeRecentFile,
  syncIndex,
  useStore,
} from '../store/useStore'
import type { Connection, RecentFile } from '../types'
import BrowseModal from '../remote/BrowseModal'
import ConnectModal from '../remote/ConnectModal'
import CacheModal from '../cache/CacheModal'
import { openViaDialog } from './openFile'

// 与后端 countLines 同口径（\n 计行，无换行结尾的末行 +1）：预检提示用。
function countTempLines(s: string): number {
  if (!s) return 0
  let n = 0
  for (let i = 0; i < s.length; i++) {
    if (s.charCodeAt(i) === 10) n++
  }
  return s.endsWith('\n') ? n : n + 1
}

/** 路径拆分（后端返回平台原生路径，Windows 用 \）：文件名 + 目录段。 */
function pathParts(p: string): { name: string; dirs: string[]; sep: string } {
  const sep = p.includes('\\') ? '\\' : '/'
  const parts = p.split(/[\\/]/).filter((x) => x !== '')
  const name = parts[parts.length - 1] ?? p
  return { name, dirs: parts.slice(0, -1), sep }
}

/**
 * 目录缩写：只保留末尾 keep 段，前面用 … 省略。
 * 两份同名文件（…\service-a\app.log 与 …\service-b\app.log）靠这段区分。
 */
function shortDir(p: string, keep = 2): string {
  const { dirs, sep } = pathParts(p)
  if (!dirs.length) return ''
  const tail = dirs.slice(Math.max(0, dirs.length - keep))
  return (dirs.length > keep ? `…${sep}` : '') + tail.join(sep)
}

/** 路径比较键（判断历史记录是否已在「已打开」中）：分隔符归一，Windows 风格忽略大小写。 */
function pathKey(p: string): string {
  const s = p.replace(/\//g, '\\')
  return /^[A-Za-z]:\\/.test(s) || s.includes('\\') ? s.toLowerCase() : s
}

/** 人类可读的体积（缓存占用展示）。 */
function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  if (n < 1024 * 1024 * 1024) return `${(n / 1024 / 1024).toFixed(1)} MB`
  return `${(n / 1024 / 1024 / 1024).toFixed(2)} GB`
}

/** 完整定位串：远端加 user@host 前缀（同名文件跨主机可区分）。 */
function fullPathOf(path: string, remote: string): string {
  return remote ? `${remote}:${path}` : path
}

/**
 * 来源比较键（与后端 store.Source.Key() 同构）：远端并入主机维度，
 * 否则「本地 /var/log/syslog」与「远端 /var/log/syslog」会互相把对方从列表里隐去。
 */
function sourceKey(kind: string, remote: string, p: string): string {
  return kind === 'remote' ? `sftp://${remote.toLowerCase()}${p}` : pathKey(p)
}

export default function FileSidebar() {
  const { message, modal } = App.useApp()
  const files = useStore((s) => s.files)
  const activeFileId = useStore((s) => s.activeFileId)
  const recent = useStore((s) => s.recent)
  const connections = useStore((s) => s.connections)
  const cacheInfo = useStore((s) => s.cacheInfo)
  const dark = useStore((s) => s.dark)
  // 收起 = Sider 折到 48px 窄栏（只留顶部工具按钮，含展开入口）
  const [collapsed, setCollapsed] = useState(false)
  const [opening, setOpening] = useState(false)
  const [tempOpen, setTempOpen] = useState(false)
  const [tempText, setTempText] = useState('')
  const [creating, setCreating] = useState(false)
  // 连接表单 / 目录浏览器；conn 为 null 表示新建
  const [connModal, setConnModal] = useState<{ open: boolean; conn: Connection | null }>({
    open: false,
    conn: null,
  })
  const [browseModal, setBrowseModal] = useState<{ open: boolean; conn: Connection | null }>({
    open: false,
    conn: null,
  })
  const [cacheOpen, setCacheOpen] = useState(false)
  // 正在用已保存的口令重连的主机 id（行内「连接」按钮的 loading）
  const [connecting, setConnecting] = useState<number | null>(null)
  // 从「最近打开」进来的远端文件：连接成功后直接打开它，而不是弹目录浏览器
  const pendingRemote = useRef<string | null>(null)

  const handleOpen = async () => {
    setOpening(true)
    try {
      await openViaDialog()
    } catch (e) {
      message.error(`打开文件失败：${errText(e)}`)
    } finally {
      setOpening(false)
    }
  }

  // 创建临时日志：先本地预检（与后端常量同口径），通过后提交后端落盘，
  // 再走与「打开文件」完全一致的 addFile + syncIndex 流程。
  const handleCreateTemp = async () => {
    const text = tempText
    if (!text.trim()) {
      message.error('内容为空：请输入至少一行日志')
      return
    }
    const lines = countTempLines(text)
    if (lines > TEMP_LOG_MAX_LINES) {
      message.error(`行数超过上限：最多 ${TEMP_LOG_MAX_LINES.toLocaleString()} 行（当前 ${lines.toLocaleString()} 行）`)
      return
    }
    if (new TextEncoder().encode(text).length > TEMP_LOG_MAX_BYTES) {
      message.error(`体积超过上限：最多 ${(TEMP_LOG_MAX_BYTES >> 20).toLocaleString()} MB`)
      return
    }
    setCreating(true)
    try {
      const info = await api.openTempLog(text)
      useStore.getState().addFile(info)
      syncIndex(info.ID).catch(() => {}) // 事件推送为主，此调用兜底对账
      setTempOpen(false)
      setTempText('')
      message.success(`已创建临时日志（${lines.toLocaleString()} 行）`)
    } catch (e) {
      message.error(`创建临时日志失败：${errText(e)}`)
    } finally {
      setCreating(false)
    }
  }

  const handleRemove = (fileId: string) => {
    modal.confirm({
      title: '关闭文件',
      content: '将释放该文件的行索引与全部检索结果，确定关闭？（历史记录保留，可随时重开）',
      okText: '关闭',
      okButtonProps: { danger: true },
      cancelText: '取消',
      onOk: () => {
        const f = findFile(useStore.getState(), fileId)
        if (f) useStore.getState().removeFile(fileId)
        message.success(`已关闭 ${f?.info.Name ?? ''}`)
      },
    })
  }

  // 用已保存的口令直接连上（口令留空 = 后端取用已保存的），连上后开目录浏览器。
  // 这是「断开之后想再连」的主路径：一次点击即可，不必再开表单输口令。
  const connectSaved = async (c: Connection): Promise<boolean> => {
    setConnecting(c.ID)
    try {
      await api.connectRemote(c.ID, { Password: '', Passphrase: '' })
      await refreshConnections()
      message.success(`已连接到 ${c.Name}`)
      return true
    } catch (e) {
      message.error(`连接失败：${errText(e)}`)
      refreshConnections().catch(() => {})
      // 口令可能已失效：转到表单让用户重新输入
      setConnModal({ open: true, conn: c })
      return false
    } finally {
      setConnecting(null)
    }
  }

  // 主机：已连接 → 浏览目录；未连接但存过口令 → 直接连上再浏览；否则去表单输入
  const handleBrowse = async (c: Connection) => {
    if (c.Connected) {
      setBrowseModal({ open: true, conn: c })
      return
    }
    if (c.HasPassword) {
      if (await connectSaved(c)) {
        setBrowseModal({ open: true, conn: { ...c, Connected: true } })
      }
      return
    }
    setConnModal({ open: true, conn: c })
  }

  const handleConnected = async (c: Connection) => {
    setConnModal({ open: false, conn: null })
    await refreshConnections().catch(() => {})
    const pending = pendingRemote.current
    pendingRemote.current = null
    if (pending) {
      // 从历史记录点进来的：连上后直接把那个文件打开
      try {
        await openRemoteFile(c.ID, pending)
      } catch (e) {
        message.error(`打开失败：${errText(e)}`)
        refreshRecent().catch(() => {})
      }
      return
    }
    setBrowseModal({ open: true, conn: c })
  }

  const handleDisconnect = async (c: Connection) => {
    try {
      await api.disconnectRemote(c.ID)
      await refreshConnections()
      await refreshRecent()
      message.success(`已断开 ${c.Name}`)
    } catch (e) {
      message.error(`断开失败：${errText(e)}`)
    }
  }

  const handleDeleteConn = (c: Connection) => {
    modal.confirm({
      title: `删除主机 ${c.Name}？`,
      content: '将同时删除该主机的全部历史文件记录（不影响远端磁盘上的文件）。',
      okText: '删除',
      okButtonProps: { danger: true },
      cancelText: '取消',
      onOk: async () => {
        try {
          await deleteConnection(c.ID)
          message.success('已删除')
        } catch (e) {
          message.error(`删除失败：${errText(e)}`)
        }
      },
    })
  }

  // 历史记录中的文件：打开（重建索引）或提示已丢失
  const handleOpenRecent = async (rec: RecentFile) => {
    if (rec.Kind === 'remote') {
      if (rec.Connected) {
        try {
          await openRecentFile(rec.ID)
        } catch (e) {
          message.error(`打开失败：${errText(e)}`)
          refreshRecent().catch(() => {})
        }
        return
      }
      // 未连接：存过口令就直接连上并打开该文件，否则去表单输入
      const conn = connections.find((c) => c.ID === rec.ConnID)
      if (!conn) {
        message.error('该文件所属的主机已被删除，请移除这条记录')
        return
      }
      if (conn.HasPassword) {
        if (await connectSaved(conn)) {
          try {
            await openRemoteFile(conn.ID, rec.Path)
          } catch (e) {
            message.error(`打开失败：${errText(e)}`)
            refreshRecent().catch(() => {})
          }
        }
        return
      }
      pendingRemote.current = rec.Path
      message.info(`请先连接 ${conn.Name}，连接后将自动打开该文件`)
      setConnModal({ open: true, conn })
      return
    }
    if (!rec.Exists) {
      modal.confirm({
        title: '文件不存在',
        width: 520,
        content: (
          <div>
            <div style={{ wordBreak: 'break-all' }}>{rec.Path}</div>
            <div style={{ marginTop: 8, color: 'rgba(128, 128, 128, 1)' }}>
              该文件可能已被移动、重命名或删除，无法打开。是否从历史记录中移除？
            </div>
          </div>
        ),
        okText: '移除记录',
        okButtonProps: { danger: true },
        cancelText: '保留',
        onOk: async () => {
          try {
            await removeRecentFile(rec.ID)
            message.success('已移除记录')
          } catch (e) {
            message.error(`移除记录失败：${errText(e)}`)
          }
        },
      })
      return
    }
    try {
      await openRecentFile(rec.ID)
    } catch (e) {
      message.error(`打开失败：${errText(e)}`)
      refreshRecent().catch(() => {}) // 可能刚被删除：刷新存在性标记
    }
  }

  // 「最近打开」中排除已在「已打开」里的文件（同一来源不重复出现）
  const openKeys = new Set(files.map((f) => sourceKey(f.info.Kind, f.info.Remote, f.info.Path)))
  const recentList = recent.filter((r) => !openKeys.has(sourceKey(r.Kind, r.Remote, r.Path)))

  return (
    <>
      <Layout.Sider
        width={270}
        collapsedWidth={48}
        collapsible
        collapsed={collapsed}
        trigger={null}
        theme="light"
        style={{ borderRight: '1px solid var(--lv-border)' }}
      >
      <div className="file-sidebar">
        <div className={`file-sidebar-tools${collapsed ? ' file-sidebar-tools-collapsed' : ''}`}>
          <Tooltip title={dark ? '切换到亮色模式' : '切换到暗色模式'}>
            <Button
              type="text"
              shape="circle"
              icon={dark ? <SunOutlined /> : <MoonOutlined />}
              onClick={() => useStore.getState().toggleTheme()}
            />
          </Tooltip>
          <Tooltip title={collapsed ? '展开侧栏' : '收起侧栏'}>
            <Button
              type="text"
              shape="circle"
              icon={collapsed ? <MenuUnfoldOutlined /> : <MenuFoldOutlined />}
              onClick={() => setCollapsed(!collapsed)}
            />
          </Tooltip>
        </div>
        {!collapsed && (
          <>
        <div className="file-sidebar-header">
          <Typography.Text strong>文件</Typography.Text>
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            {files.length ? `${files.length} 个已打开` : ''}
          </Typography.Text>
        </div>
        <div className="file-sidebar-list">
          {files.length === 0 ? (
            <div className="file-sidebar-empty">
              <Typography.Text type="secondary" style={{ fontSize: 13 }}>
                尚未打开文件
                <br />
                点击下方「打开文件」开始
              </Typography.Text>
            </div>
          ) : (
            files.map((f) => {
              const active = f.fileId === activeFileId
              const remote = f.info.Kind === 'remote'
              const dir = shortDir(f.info.Path)
              return (
                <div
                  key={f.fileId}
                  className={`file-item${active ? ' file-item-active' : ''}`}
                  onClick={() => useStore.getState().setActiveFile(f.fileId)}
                >
                  <div className="file-item-row">
                    {remote ? (
                      <CloudServerOutlined style={{ marginRight: 8, color: '#722ed1' }} />
                    ) : (
                      <FileTextOutlined style={{ marginRight: 8, color: '#1677ff' }} />
                    )}
                    <Tooltip title={fullPathOf(f.info.Path, f.info.Remote)} placement="right">
                      <span
                        className="file-item-name"
                        title={fullPathOf(f.info.Path, f.info.Remote)}
                      >
                        {f.info.Name}
                      </span>
                    </Tooltip>
                    <Popconfirm
                      title="关闭此文件？"
                      description="将释放索引与全部检索结果"
                      okText="关闭"
                      okButtonProps={{ danger: true }}
                      cancelText="取消"
                      onConfirm={(e) => {
                        e?.stopPropagation?.()
                        handleRemove(f.fileId)
                      }}
                    >
                      <CloseOutlined
                        className="file-item-close"
                        onClick={(e) => e.stopPropagation()}
                      />
                    </Popconfirm>
                  </div>
                  <div className="file-item-meta">
                    {f.indexError ? (
                      <Typography.Text type="danger" style={{ fontSize: 12 }}>
                        索引失败：{f.indexError}
                      </Typography.Text>
                    ) : !f.indexDone ? (
                      <Progress
                        percent={Math.round(f.indexPercent)}
                        size="small"
                        status="active"
                        style={{ margin: 0 }}
                      />
                    ) : (
                      <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                        {f.info.TotalLines.toLocaleString()} 行 · 就绪
                      </Typography.Text>
                    )}
                    {dir && (
                      <Tooltip
                        title={fullPathOf(f.info.Path, f.info.Remote)}
                        placement="right"
                      >
                        <div className={`file-item-dir${remote ? ' file-item-dir-remote' : ''}`}>
                          {remote && <span className="file-item-host">{f.info.Remote}</span>}
                          {dir}
                        </div>
                      </Tooltip>
                    )}
                  </div>
                </div>
              )
            })
          )}
        </div>
        {connections.length > 0 && (
          <div className="file-sidebar-conns">
            <div className="file-sidebar-recent-header">
              <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                远程主机
              </Typography.Text>
              <Tooltip title="新建连接">
                <PlusOutlined
                  className="file-sidebar-recent-clear"
                  onClick={() => setConnModal({ open: true, conn: null })}
                />
              </Tooltip>
            </div>
            {connections.map((c) => (
              <div key={c.ID} className="file-item file-item-conn" onClick={() => handleBrowse(c)}>
                <div className="file-item-row">
                  <Tooltip title={c.Connected ? '已连接' : '未连接'}>
                    <span
                      className={`conn-dot${c.Connected ? ' conn-dot-on' : ''}`}
                      style={{ marginRight: 8 }}
                    />
                  </Tooltip>
                  <Tooltip title={`${c.User}@${c.Host}:${c.Port}`} placement="right">
                    <span className="file-item-name">{c.Name}</span>
                  </Tooltip>
                  {!c.Connected && (
                    <Button
                      size="small"
                      type="link"
                      style={{ padding: 0, height: 20, fontSize: 12 }}
                      loading={connecting === c.ID}
                      onClick={(e) => {
                        e.stopPropagation()
                        handleBrowse(c).catch(() => {})
                      }}
                    >
                      连接
                    </Button>
                  )}
                  <Tooltip title="编辑">
                    <EditOutlined
                      className="file-item-close"
                      onClick={(e) => {
                        e.stopPropagation()
                        setConnModal({ open: true, conn: c })
                      }}
                    />
                  </Tooltip>
                  {c.Connected && (
                    <Tooltip title="断开">
                      <DisconnectOutlined
                        className="file-item-close"
                        onClick={(e) => {
                          e.stopPropagation()
                          handleDisconnect(c).catch(() => {})
                        }}
                      />
                    </Tooltip>
                  )}
                  <Popconfirm
                    title={`删除主机 ${c.Name}？`}
                    description="同时删除该主机的历史文件记录"
                    okText="删除"
                    okButtonProps={{ danger: true }}
                    cancelText="取消"
                    onConfirm={(e) => {
                      e?.stopPropagation?.()
                      handleDeleteConn(c)
                    }}
                  >
                    <DeleteOutlined
                      className="file-item-close"
                      onClick={(e) => e.stopPropagation()}
                    />
                  </Popconfirm>
                </div>
                <div className="file-item-meta">
                  <div className="file-item-dir">
                    <span className="file-item-host">
                      {c.User}@{c.Host}:{c.Port}
                    </span>
                    {c.Connected ? ' · 已连接' : c.HasPassword ? ' · 已保存口令，可直接连接' : ' · 需输入口令'}
                  </div>
                </div>
              </div>
            ))}
            <div className="file-item file-item-conn" onClick={() => setCacheOpen(true)}>
              <div className="file-item-row">
                <DatabaseOutlined style={{ marginRight: 8, color: 'var(--lv-close)' }} />
                <span className="file-item-name">本地缓存</span>
              </div>
              <div className="file-item-meta">
                <div className="file-item-dir">
                  {cacheInfo?.Available
                    ? cacheInfo.Enabled
                      ? `${(cacheInfo.Entries ?? []).length} 项 · ${formatBytes(cacheInfo.TotalBytes)}`
                      : '已关闭'
                    : '不可用'}
                </div>
              </div>
            </div>
          </div>
        )}
        {recentList.length > 0 && (
          <div className="file-sidebar-recent">
            <div className="file-sidebar-recent-header">
              <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                最近打开
              </Typography.Text>
              <Popconfirm
                title="清空历史记录？"
                description="仅删除记录，不影响磁盘上的文件"
                okText="清空"
                okButtonProps={{ danger: true }}
                cancelText="取消"
                onConfirm={async () => {
                  try {
                    await clearRecentFiles()
                    message.success('已清空历史记录')
                  } catch (e) {
                    message.error(`清空失败：${errText(e)}`)
                  }
                }}
              >
                <Tooltip title="清空历史记录">
                  <DeleteOutlined className="file-sidebar-recent-clear" />
                </Tooltip>
              </Popconfirm>
            </div>
            {recentList.map((r) => {
              const remote = r.Kind === 'remote'
              // 远端只在「未连接」时打标：不连接就无从判断文件是否还在，
              // 显示「已丢失」会误导（真实判断发生在连接后的 stat）。
              const tag = remote
                ? !r.Connected && { color: 'default', text: '未连接' }
                : !r.Exists && { color: 'error', text: '已丢失' }
              const full = fullPathOf(r.Path, r.Remote)
              return (
              <div key={r.ID} className="file-item file-item-recent" onClick={() => handleOpenRecent(r)}>
                <div className="file-item-row">
                  {remote ? (
                    <CloudServerOutlined
                      style={{ marginRight: 8, color: r.Connected ? '#722ed1' : 'var(--lv-close)' }}
                    />
                  ) : (
                    <ClockCircleOutlined
                      style={{ marginRight: 8, color: r.Exists ? 'var(--lv-close)' : '#ff4d4f' }}
                    />
                  )}
                  <Tooltip title={full} placement="right">
                    <span className="file-item-name" title={full}>
                      {r.Name}
                    </span>
                  </Tooltip>
                  {tag && (
                    <Tag
                      color={tag.color}
                      style={{ marginInlineEnd: 4, fontSize: 11, lineHeight: '16px' }}
                    >
                      {tag.text}
                    </Tag>
                  )}
                  <Popconfirm
                    title="移除这条历史记录？"
                    description="仅删除记录，不影响磁盘上的文件"
                    okText="移除"
                    okButtonProps={{ danger: true }}
                    cancelText="取消"
                    onConfirm={async (e) => {
                      e?.stopPropagation?.()
                      try {
                        await removeRecentFile(r.ID)
                      } catch (err) {
                        message.error(`移除记录失败：${errText(err)}`)
                      }
                    }}
                  >
                    <CloseOutlined
                      className="file-item-close"
                      onClick={(e) => e.stopPropagation()}
                    />
                  </Popconfirm>
                </div>
                <div className="file-item-meta">
                  <Tooltip title={full} placement="right">
                    <div
                      className={`file-item-dir${remote ? ' file-item-dir-remote' : ''}`}
                      style={!remote && !r.Exists ? { color: '#ff4d4f' } : undefined}
                    >
                      {remote && <span className="file-item-host">{r.Remote}</span>}
                      {shortDir(r.Path) || r.Path}
                      {r.TotalLines > 0 ? ` · ${r.TotalLines.toLocaleString()} 行` : ''}
                    </div>
                  </Tooltip>
                </div>
              </div>
              )
            })}
          </div>
        )}
        <div className="file-sidebar-footer">
          <Button
            type="primary"
            block
            icon={<FolderOpenOutlined />}
            loading={opening}
            onClick={handleOpen}
          >
            打开文件
          </Button>
          <Button
            block
            icon={<ApiOutlined />}
            onClick={() => setConnModal({ open: true, conn: null })}
          >
            连接远程主机
          </Button>
          <Button
            block
            icon={<FileAddOutlined />}
            disabled={creating}
            onClick={() => setTempOpen(true)}
          >
            创建临时日志
          </Button>
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            支持 .log / .jsonl / .txt / .json
          </Typography.Text>
        </div>
          </>
          )}
      </div>
    </Layout.Sider>
    <Modal
      title="创建临时日志"
      open={tempOpen}
      width={600}
      okText="创建"
      cancelText="取消"
      confirmLoading={creating}
      onOk={handleCreateTemp}
      onCancel={() => {
        if (!creating) setTempOpen(false)
      }}
    >
      <Input.TextArea
        value={tempText}
        onChange={(e) => setTempText(e.target.value)}
        autoSize={{ minRows: 10, maxRows: 18 }}
        placeholder={'每行一条 JSON 日志，例如：\n{"time": 1720000000000, "level": "INFO", "msg": "hello"}'}
        style={{ fontFamily: 'Consolas, Menlo, monospace', fontSize: 12 }}
      />
      <div
        style={{
          display: 'flex',
          justifyContent: 'space-between',
          marginTop: 8,
          fontSize: 12,
          color: 'rgba(0, 0, 0, 0.45)',
        }}
      >
        <span>
          最多 {TEMP_LOG_MAX_LINES.toLocaleString()} 行 /{' '}
          {(TEMP_LOG_MAX_BYTES >> 20).toLocaleString()} MB
        </span>
        {countTempLines(tempText) > 0 && (
          <span>当前 {countTempLines(tempText).toLocaleString()} 行</span>
        )}
      </div>
    </Modal>
    <ConnectModal
      open={connModal.open}
      connection={connModal.conn}
      onClose={() => {
        pendingRemote.current = null
        setConnModal({ open: false, conn: null })
      }}
      onConnected={(c) => {
        handleConnected(c).catch(() => {})
      }}
    />
    <CacheModal open={cacheOpen} onClose={() => setCacheOpen(false)} />
    <BrowseModal
      open={browseModal.open}
      connection={browseModal.conn}
      onClose={() => setBrowseModal({ open: false, conn: null })}
      onOpened={() => setBrowseModal({ open: false, conn: null })}
    />
    </>
  )
}
