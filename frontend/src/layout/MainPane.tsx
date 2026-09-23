// 右侧主区（docs/issues/05 §4）：文件切换在左侧边栏，这里只渲染当前激活
// 文件（含其内检索级 Tabs）；空状态：未打开文件时提示，索引中显示进度。

import { useEffect, useState } from 'react'
import { Alert, App, Button, Empty, Layout, Progress, Space, Tabs, Tooltip, Typography } from 'antd'
import { ExportOutlined, LoadingOutlined, ReloadOutlined } from '@ant-design/icons'
import { api, errText } from '../api'
import { findFile, reloadFile, remoteCacheKey, runSearch, useStore, type FileView } from '../store/useStore'
import LogTable from '../table/LogTable'
import QueryBuilder from '../search/QueryBuilder'
import { openViaDialog } from './openFile'

function IndexingPane({ file }: { file: FileView }) {
  const remote = file.info.Kind === 'remote'
  // 命中本地缓存时不必再走网络：提示里说清楚，免得用户以为又要等一遍传输
  const cacheInfo = useStore((s) => s.cacheInfo)
  const cached =
    remote &&
    (cacheInfo?.Entries ?? []).some(
      (e) => e.SourceKey === remoteCacheKey(file.info.Remote, file.info.Path),
    )
  return (
    <div className="main-center">
      <Space orientation="vertical" size={16} align="center">
        <Progress type="circle" percent={Math.floor(file.indexPercent)} status="active" size={120} />
        <Typography.Text type="secondary" style={{ textAlign: 'center' }}>
          {file.reloading ? (
            <>
              文件已变化，正在重新加载
              {file.indexPercent > 0 ? `（${Math.floor(file.indexPercent)}%）` : ''}
              ，完成后会按当前条件重新检索
            </>
          ) : cached ? (
            <>
              正在从本地缓存重建索引
              {file.indexPercent > 0 ? `（${Math.floor(file.indexPercent)}%）` : ''}，不走网络
            </>
          ) : remote ? (
            <>
              正在从 {file.info.Remote} 读取并缓存到本机
              {file.indexPercent > 0 ? `（${Math.floor(file.indexPercent)}%）` : ''}，
              首次打开需要完整读一遍（传输时间取决于链路带宽），之后打开与翻页直接读本地缓存
            </>
          ) : (
            <>
              正在建立行索引
              {file.indexPercent > 0 ? `（${Math.floor(file.indexPercent)}%）` : ''}
              ，大文件首次打开需要几秒，完成后即可浏览与检索
            </>
          )}
        </Typography.Text>
      </Space>
    </div>
  )
}

function ErrorPane({ file }: { file: FileView }) {
  return (
    <div className="main-center">
      <Alert
        type="error"
        showIcon
        style={{ maxWidth: 520 }}
        message="索引失败"
        description={file.indexError || '未知错误'}
        action={
          <Button danger size="small" onClick={() => useStore.getState().removeFile(file.fileId)}>
            关闭此文件
          </Button>
        }
      />
    </div>
  )
}

function FilePane({ fileId }: { fileId: string }) {
  const { message } = App.useApp()
  const file = useStore((s) => findFile(s, fileId))
  const [exportingTab, setExportingTab] = useState<string | null>(null)
  const [reloading, setReloading] = useState(false)

  // 初始「全部」tab：索引就绪后自动跑一次空条件检索（全部行）。
  // autoRun 在触发前即被消费，取消/失败都不会造成自动重跑循环。
  // 注意：所有 hook（含本 effect）必须在下方条件 return 之前声明，顺序不可变。
  useEffect(() => {
    if (!file?.indexDone || file.indexError) return
    const st = useStore.getState()
    const t = findFile(st, fileId)?.tabs.find((x) => x.autoRun)
    if (!t || t.searching || t.searched) return
    st.setTab(fileId, t.tabId, { autoRun: false })
    runSearch(fileId, t.tabId).catch((e) => {
      message.error(`检索失败：${errText(e)}`)
    })
  }, [fileId, file?.indexDone, file?.indexError, message])

  if (!file) return null

  if (file.indexError) return <ErrorPane file={file} />
  if (!file.indexDone) return <IndexingPane file={file} />

  // 重新加载：文件被追加/轮转后重建索引（后缀加 loading 由后端进度驱动）
  const handleReload = async () => {
    setReloading(true)
    try {
      await reloadFile(fileId)
      message.success('已重新加载')
    } catch (e) {
      message.error(`重新加载失败：${errText(e)}`)
    } finally {
      setReloading(false)
    }
  }

  // 导出该 tab 的全部命中（后端 SaveFile 对话框 + 逐段流式写出）
  const handleExport = async (tab: FileView['tabs'][number]) => {
    setExportingTab(tab.tabId)
    try {
      const path = await api.exportMatches(fileId, tab.tabId)
      if (path) message.success(`已导出 ${tab.total.toLocaleString()} 条 → ${path}`)
    } catch (e) {
      message.error(`导出失败：${errText(e)}`)
    } finally {
      setExportingTab(null)
    }
  }

  const items = file.tabs.map((tab, i) => ({
    key: tab.tabId,
    label: (
      // 标题固定为序号（index），不随查询变化；查询条件摘要放 hover 提示里
      <span title={tab.title}>
        {i + 1}
        {tab.searching && <LoadingOutlined style={{ marginLeft: 6, color: '#1677ff' }} spin />}
      </span>
    ),
    // 每个文件至少保留一个 tab；关闭由 removeTab 内部兜底
    closable: file.tabs.length > 1,
    // 固定区（查询条件/导出）在上，表格区自占剩余高度并自行滚动（见 app.css .tab-pane）
    children: (
      <div className="tab-pane">
        <QueryBuilder fileId={fileId} tab={tab} fields={file.fields ?? []} />
        <div className="tab-actions">
          {/* 日志被追加/轮转后重新读取：会话与检索条件保留，完成后自动重跑当前 tab */}
          <Tooltip title="重新读取该文件并重建索引（日志被追加或轮转后用）">
            <Button
              size="small"
              icon={<ReloadOutlined />}
              loading={reloading}
              onClick={handleReload}
            >
              重新加载
            </Button>
          </Tooltip>
          {tab.searched && tab.total > 0 && (
            <Button
              size="small"
              icon={<ExportOutlined />}
              loading={exportingTab === tab.tabId}
              onClick={() => handleExport(tab)}
            >
              导出全部命中（{tab.total.toLocaleString()} 条）
            </Button>
          )}
        </div>
        <LogTable fileId={fileId} tab={tab} />
      </div>
    ),
  }))

  const onEdit = (targetKey: unknown, action: 'add' | 'remove') => {
    if (action === 'add') {
      // + = 复制当前激活 tab 的过滤条件到新 tab 并执行查询
      useStore
        .getState()
        .addSearchTab(fileId)
        .catch((e) => {
          message.error(`检索失败：${errText(e)}`)
        })
      return
    }
    const tabId = String(targetKey)
    const st = useStore.getState()
    const f = findFile(st, fileId)
    if (!f || f.tabs.length <= 1) return
    const tab = f.tabs.find((t) => t.tabId === tabId)
    if (!tab) return
    // 关闭 tab：内部已取消扫描并释放后端命中缓存
    try {
      st.removeTab(fileId, tabId)
    } catch (e) {
      message.error(`关闭 tab 失败：${errText(e)}`)
    }
  }

  return (
    <Tabs
      type="editable-card"
      size="small"
      items={items}
      activeKey={file.activeTabId}
      onChange={(key) => useStore.getState().setActiveTab(fileId, key)}
      onEdit={onEdit}
      className="search-tabs"
    />
  )
}

export default function MainPane() {
  const { message } = App.useApp()
  const activeFileId = useStore((s) => s.activeFileId)

  const handleOpen = async () => {
    try {
      await openViaDialog()
    } catch (e) {
      message.error(`打开文件失败：${errText(e)}`)
    }
  }

  // 文件切换在左侧边栏（FileSidebar），右侧只渲染当前文件（含其检索 Tabs）
  return (
    <Layout.Content className="right-pane">
      {activeFileId ? (
        <FilePane fileId={activeFileId} />
      ) : (
        <div className="main-center">
          <Empty description="尚未打开任何日志文件">
            <Button type="primary" onClick={handleOpen}>
              打开文件
            </Button>
          </Empty>
        </div>
      )}
    </Layout.Content>
  )
}
