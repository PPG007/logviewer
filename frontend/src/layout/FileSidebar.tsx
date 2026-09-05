// 左侧文件列表（docs/functional-design.md §7）：每个打开的文件一项，
// 显示文件名 + 索引进度/状态；底部「打开文件」+「创建临时日志」入口；hover 关闭。

import { useState } from 'react'
import {
  App,
  Button,
  Input,
  Layout,
  Modal,
  Popconfirm,
  Progress,
  Tooltip,
  Typography,
} from 'antd'
import {
  CloseOutlined,
  FileAddOutlined,
  FileTextOutlined,
  FolderOpenOutlined,
  MenuFoldOutlined,
  MenuUnfoldOutlined,
  MoonOutlined,
  SunOutlined,
} from '@ant-design/icons'
import { api, errText, TEMP_LOG_MAX_BYTES, TEMP_LOG_MAX_LINES } from '../api'
import { findFile, syncIndex, useStore } from '../store/useStore'
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

export default function FileSidebar() {
  const { message, modal } = App.useApp()
  const files = useStore((s) => s.files)
  const activeFileId = useStore((s) => s.activeFileId)
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
      content: '将释放该文件的行索引与全部检索结果，确定关闭？',
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
              return (
                <div
                  key={f.fileId}
                  className={`file-item${active ? ' file-item-active' : ''}`}
                  onClick={() => useStore.getState().setActiveFile(f.fileId)}
                >
                  <div className="file-item-row">
                    <FileTextOutlined style={{ marginRight: 8, color: '#1677ff' }} />
                    <Tooltip title={f.info.Path} placement="right">
                      <span className="file-item-name" title={f.info.Name}>
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
                  </div>
                </div>
              )
            })
          )}
        </div>
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
