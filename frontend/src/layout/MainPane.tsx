// 右侧主区（docs/issues/05 §4）：文件切换在左侧边栏，这里只渲染当前激活
// 文件（含其内检索级 Tabs）；空状态：未打开文件时提示，索引中显示进度。

import { useEffect, useState } from 'react'
import { Alert, App, Button, Empty, Layout, Progress, Space, Tabs, Typography } from 'antd'
import { ExportOutlined, LoadingOutlined } from '@ant-design/icons'
import { api, errText } from '../api'
import { findFile, runSearch, useStore, type FileView } from '../store/useStore'
import LogTable from '../table/LogTable'
import QueryBuilder from '../search/QueryBuilder'
import { openViaDialog } from './openFile'

function IndexingPane({ file }: { file: FileView }) {
  return (
    <div className="main-center">
      <Space orientation="vertical" size={16} align="center">
        <Progress type="circle" percent={Math.floor(file.indexPercent)} status="active" size={120} />
        <Typography.Text type="secondary">
          正在建立行索引
          {file.indexPercent > 0 ? `（${Math.floor(file.indexPercent)}%）` : ''}，大文件首次打开需要几秒，
          完成后即可浏览与检索
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
    children: (
      <Space orientation="vertical" size={4} style={{ width: '100%', alignItems: 'stretch' }}>
        <QueryBuilder fileId={fileId} tab={tab} fields={file.fields ?? []} />
        {tab.searched && tab.total > 0 && (
          <div style={{ display: 'flex', justifyContent: 'flex-end', padding: '2px 4px' }}>
            <Button
              size="small"
              icon={<ExportOutlined />}
              loading={exportingTab === tab.tabId}
              onClick={() => handleExport(tab)}
            >
              导出全部命中（{tab.total.toLocaleString()} 条）
            </Button>
          </div>
        )}
        <LogTable fileId={fileId} tab={tab} />
      </Space>
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
    <Layout.Content className="right-scroll">
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
