// 左侧文件列表（docs/functional-design.md §7）：上段是本次已打开的文件，
// 下段是「最近打开」（SQLite 持久化，重启后仍在，含磁盘存在性标记）；
// 底部「打开文件」+「创建临时日志」入口；hover 关闭。
// 同名不同路径的文件靠目录行区分。

import { useState } from 'react'
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
  ClockCircleOutlined,
  CloseOutlined,
  DeleteOutlined,
  FileAddOutlined,
  FileTextOutlined,
  FolderOpenOutlined,
  MenuFoldOutlined,
  MenuUnfoldOutlined,
  MoonOutlined,
  SunOutlined,
} from '@ant-design/icons'
import { api, errText, TEMP_LOG_MAX_BYTES, TEMP_LOG_MAX_LINES } from '../api'
import {
  clearRecentFiles,
  findFile,
  openRecentFile,
  refreshRecent,
  removeRecentFile,
  syncIndex,
  useStore,
} from '../store/useStore'
import type { RecentFile } from '../types'
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

export default function FileSidebar() {
  const { message, modal } = App.useApp()
  const files = useStore((s) => s.files)
  const activeFileId = useStore((s) => s.activeFileId)
  const recent = useStore((s) => s.recent)
  const dark = useStore((s) => s.dark)
  // 收起 = Sider 折到 48px 窄栏（只留顶部工具按钮，含展开入口）
  const [collapsed, setCollapsed] = useState(false)
  const [opening, setOpening] = useState(false)
  const [tempOpen, setTempOpen] = useState(false)
  const [tempText, setTempText] = useState('')
  const [creating, setCreating] = useState(false)

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

  // 历史记录中的文件：打开（重建索引）或提示已丢失
  const handleOpenRecent = async (rec: RecentFile) => {
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

  // 「最近打开」中排除已在「已打开」里的文件（同一文件不重复出现）
  const openKeys = new Set(files.map((f) => pathKey(f.info.Path)))
  const recentList = recent.filter((r) => !openKeys.has(pathKey(r.Path)))

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
              const dir = shortDir(f.info.Path)
              return (
                <div
                  key={f.fileId}
                  className={`file-item${active ? ' file-item-active' : ''}`}
                  onClick={() => useStore.getState().setActiveFile(f.fileId)}
                >
                  <div className="file-item-row">
                    <FileTextOutlined style={{ marginRight: 8, color: '#1677ff' }} />
                    <Tooltip title={f.info.Path} placement="right">
                      <span className="file-item-name" title={f.info.Path}>
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
                      <Tooltip title={f.info.Path} placement="right">
                        <div className="file-item-dir">{dir}</div>
                      </Tooltip>
                    )}
                  </div>
                </div>
              )
            })
          )}
        </div>
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
            {recentList.map((r) => (
              <div key={r.ID} className="file-item file-item-recent" onClick={() => handleOpenRecent(r)}>
                <div className="file-item-row">
                  <ClockCircleOutlined
                    style={{ marginRight: 8, color: r.Exists ? 'var(--lv-close)' : '#ff4d4f' }}
                  />
                  <Tooltip title={r.Path} placement="right">
                    <span className="file-item-name" title={r.Path}>
                      {r.Name}
                    </span>
                  </Tooltip>
                  {!r.Exists && (
                    <Tag color="error" style={{ marginInlineEnd: 4, fontSize: 11, lineHeight: '16px' }}>
                      已丢失
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
                  <Tooltip title={r.Path} placement="right">
                    <div
                      className="file-item-dir"
                      style={r.Exists ? undefined : { color: '#ff4d4f' }}
                    >
                      {shortDir(r.Path) || r.Path}
                      {r.TotalLines > 0 ? ` · ${r.TotalLines.toLocaleString()} 行` : ''}
                    </div>
                  </Tooltip>
                </div>
              </div>
            ))}
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
    </>
  )
}
