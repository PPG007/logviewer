// 结果表格：行号/时间/级别/消息。点击整行弹出 Modal 展示该行完整内容
//（有效行 = @microlink/react-json-view 树形渲染：语法高亮 + 对象/数组可折叠；
//   无效行 = 原始文本）。级别着色（Tag + 整行淡色）。
// 数据经 GetPage 命中跳转取回，分页条含 pageSize 选择器（默认 20，范围 10~100，随 tab 独立）。

import { useState } from 'react'
import { App, Empty, Modal, Table, Tag, Typography } from 'antd'
import type { TableColumnsType } from 'antd'
import ReactJson from '@microlink/react-json-view'
import dayjs from 'dayjs'
import { errText } from '../api'
import { PAGE_SIZE_OPTIONS, loadSearchPage, useStore, type Tab } from '../store/useStore'
import type { ParsedLine } from '../types'

type Row = ParsedLine

// M8 §1 完整映射：Tag 颜色 + 整行底色档位（default = 无底色）
const LEVEL_COLOR: Record<string, string> = {
  ERROR: 'red',
  FATAL: 'magenta',
  CRITICAL: 'magenta',
  WARN: 'orange',
  WARNING: 'orange',
  INFO: 'blue',
  DEBUG: 'default',
  TRACE: 'default',
}

const LEVEL_TINT: Record<string, string> = {
  ERROR: 'error',
  FATAL: 'error',
  CRITICAL: 'error',
  WARN: 'warn',
  WARNING: 'warn',
  INFO: '',
  DEBUG: 'debug',
  TRACE: '',
}

const rowClass = (row: Row): string => {
  if (!row.Valid || !row.Level) return ''
  const tint = LEVEL_TINT[row.Level.toUpperCase()]
  return tint ? `lv-row lv-${tint}` : ''
}

function TimeCell({ ts }: { ts: number | null }) {
  return ts === null || ts === undefined ? (
    <Typography.Text type="secondary">-</Typography.Text>
  ) : (
    <span>{dayjs(ts).format('YYYY-MM-DD HH:mm:ss.SSS')}</span>
  )
}

function LevelCell({ row }: { row: Row }) {
  if (!row.Valid) {
    return <Tag color="volcano">无效</Tag>
  }
  if (!row.Level) {
    return <Typography.Text type="secondary">-</Typography.Text>
  }
  return <Tag color={LEVEL_COLOR[row.Level.toUpperCase()] ?? 'default'}>{row.Level}</Tag>
}

function MessageCell({ row }: { row: Row }) {
  if (!row.Valid) {
    // 坏行：原始文本兜底 + 标记（docs §5.2）
    return (
      <Typography.Text type="warning" italic style={{ fontSize: 12 }}>
        {row.Raw}
      </Typography.Text>
    )
  }
  return row.Message ? (
    <Typography.Text style={{ fontSize: 13 }}>{row.Message}</Typography.Text>
  ) : (
    <Typography.Text type="secondary">-</Typography.Text>
  )
}

const columns: TableColumnsType<Row> = [
  {
    title: '行号',
    dataIndex: 'LineNo',
    width: 90,
    align: 'right',
    render: (no: number) => <span style={{ color: '#999' }}>{no + 1}</span>,
  },
  {
    title: '时间',
    dataIndex: 'Timestamp',
    width: 200,
    render: (_: unknown, row: Row) => <TimeCell ts={row.Timestamp} />,
  },
  {
    title: '级别',
    dataIndex: 'Level',
    width: 110,
    render: (_: unknown, row: Row) => <LevelCell row={row} />,
  },
  {
    title: '消息',
    dataIndex: 'Message',
    ellipsis: true,
    render: (_: unknown, row: Row) => <MessageCell row={row} />,
  },
]

// 整行点击详情：Modal 展示行号/级别/时间 + 完整内容（JSON 或无效行原始文本）
function RowDetailModal({ row, onClose }: { row: Row | null; onClose: () => void }) {
  const dark = useStore((s) => s.dark)
  if (!row) return null
  const valid = row.Valid
  // 标题 = 消息内容（≤30 字符，超出截断加 …；无效行退化为原始文本）；
  // 换行/连续空白压平为单个空格，避免破坏标题布局；完整内容放 hover 提示
  const full = (valid && row.Message ? row.Message : row.Raw) || '（无消息内容）'
  const flat = full.replace(/\s+/g, ' ').trim()
  const chars = Array.from(flat)
  const head = chars.length > 30 ? `${chars.slice(0, 30).join('')}…` : flat
  return (
    <Modal
      open
      title={<span title={full}>{head}</span>}
      footer={null}
      width={720}
      onCancel={onClose}
      className="lv-json-modal"
    >
      {valid ? (
        // base16 主题：键名/字符串/数字等按类别着色（亮/暗各自的主题，随全局主题切换）；
        // 三角形箭头点开/收起对象与数组。
        // shouldCollapse：namespace ≥ 3 层的子树默认折叠，长文档不一次性展开成大 DOM，
        //   用户仍可手动展开任意层级；超长数组由库内 groupArraysAfterLength=100 分组兜底。
        <div className="lv-json-viewer">
          <ReactJson
            src={row.JSON ?? {}}
            name={false}
            theme={dark ? 'monokai' : 'rjv-default'}
            iconStyle="triangle"
            enableClipboard={false}
            displayObjectSize={false}
            displayDataTypes={false}
            quotesOnKeys={false}
            style={{ fontSize: 12, background: 'transparent' }}
            shouldCollapse={(f) => f.namespace.length >= 3}
          />
        </div>
      ) : (
        <>
          <Typography.Text type="warning" style={{ display: 'block', marginBottom: 8 }}>
            该行无法解析为 JSON，以下为其原始文本：
          </Typography.Text>
          <pre className="lv-json-pre">{row.Raw}</pre>
        </>
      )}
    </Modal>
  )
}

export default function LogTable({ fileId, tab }: { fileId: string; tab: Tab }) {
  const { message } = App.useApp()
  const [detail, setDetail] = useState<Row | null>(null)

  const load = (page: number, pageSize?: number) => {
    // pageSize 变化（选择器）：写入 tab 并回到第 1 页；纯翻页则按传入页码
    const st = useStore.getState()
    const cur = st.files.flatMap((f) => f.tabs).find((t) => t.tabId === tab.tabId)
    if (!cur) return
    setDetail(null) // 翻页/换页大小后旧行详情不再对应本页，关闭弹窗
    if (pageSize !== undefined && pageSize !== cur.pageSize) {
      st.setTab(fileId, tab.tabId, { pageSize, page: 1 })
      loadSearchPage(fileId, tab.tabId, 1).catch((e) => {
        message.error(`翻页失败：${errText(e)}`)
      })
      return
    }
    loadSearchPage(fileId, tab.tabId, page).catch((e) => {
      message.error(`翻页失败：${errText(e)}`)
    })
  }

  const emptyText = tab.searched ? (
    <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="无命中" />
  ) : (
    <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="点击上方「检索」开始查询" />
  )

  return (
    <>
      <Table<Row>
        size="middle"
        className="lv-rows-clickable"
        rowKey={(r) => String(r.LineNo)}
        columns={columns}
        dataSource={tab.rows}
        loading={tab.loading}
        rowClassName={rowClass}
        onRow={(row) => ({ onClick: () => setDetail(row) })}
        pagination={
          tab.total > 0
            ? {
                current: tab.page,
                pageSize: tab.pageSize,
                total: tab.total,
                showSizeChanger: true,
                pageSizeOptions: PAGE_SIZE_OPTIONS,
                showQuickJumper: true,
                showTotal: (t) => `共 ${t.toLocaleString()} 条`,
                onChange: (page, size) => load(page, size),
              }
            : false
        }
        locale={{ emptyText }}
      />
      <RowDetailModal row={detail} onClose={() => setDetail(null)} />
    </>
  )
}
