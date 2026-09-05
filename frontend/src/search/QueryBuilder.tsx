// 查询构建区（docs/issues/07 §2）：字段条件（可多条，AND）+ 关键字 + 时间范围。
// 状态全部驱动 store（单一来源），本组件只做编辑动作与提交/取消。
// 值单元格：string 字段且探测到完整取值枚举（level 等）→ 下拉点选防误输；
//   无取值/取值过多截断/非 string 字段 → 自由输入（见 FetchValue 决策）。

import { useEffect, useState } from 'react'
import { App, Button, DatePicker, Input, Progress, Select, Space } from 'antd'
import {
  MinusCircleOutlined,
  PlusOutlined,
  SearchOutlined,
  StopOutlined,
} from '@ant-design/icons'
import dayjs, { type Dayjs } from 'dayjs'
import { errText } from '../api'
import { cancelTabSearch, fetchFieldValues, runSearch, useStore, type Tab } from '../store/useStore'
import type { Condition, FieldInfo, FieldValues, TimeRange } from '../types'

const OP_OPTIONS = [
  { value: '=', label: '=' },
  { value: '!=', label: '≠' },
  { value: 'contains', label: '包含' },
  { value: '>', label: '>' },
  { value: '<', label: '<' },
]

interface Props {
  fileId: string
  tab: Tab
  fields: FieldInfo[]
}

// 值单元格：字段切到 string 且该文件取值枚举完整（未截断）时换 Select。
// 枚举数据在索引期已备好、此处按文件缓存拉取（store 命中即同步返回），
// 拉取完成前暂以输入框形态渲染，避免无谓的形态闪烁。
function ValueCell({
  fileId,
  field,
  fieldType,
  value,
  onChange,
  onPressEnter,
}: {
  fileId: string
  field: string
  fieldType?: string
  value: string
  onChange: (v: string) => void
  onPressEnter: () => void
}) {
  const [fv, setFv] = useState<FieldValues | null>(null)
  useEffect(() => {
    let alive = true
    setFv(null) // 字段切换后旧取值枚举作废，等新结果
    if (!field || fieldType !== 'string') return
    fetchFieldValues(fileId, field).then((v) => {
      if (alive) setFv(v)
    })
    return () => {
      alive = false
    }
  }, [fileId, field, fieldType])
  if (fv && fv.Values.length > 0) {
    return (
      <Select
        style={{ width: 240 }}
        placeholder="值"
        allowClear
        showSearch
        value={value || undefined}
        onChange={(v) => onChange(v ?? '')}
        options={fv.Values.map((s) => ({ value: s, label: s }))}
      />
    )
  }
  return (
    <Input
      style={{ width: 240 }}
      placeholder={fv?.Truncated ? '取值过多，可手动输入' : '值'}
      value={value}
      onChange={(e) => onChange(e.target.value)}
      onPressEnter={onPressEnter}
    />
  )
}

export default function QueryBuilder({ fileId, tab, fields }: Props) {
  const { message } = App.useApp()
  const query = tab.query

  const updateCond = (i: number, patch: Partial<Condition>) => {
    const next = query.Conditions.map((c, idx) => (idx === i ? { ...c, ...patch } : c))
    useStore.getState().setTab(fileId, tab.tabId, { query: { ...query, Conditions: next } })
  }
  const addCond = () => {
    useStore.getState().setTab(fileId, tab.tabId, {
      query: { ...query, Conditions: [...query.Conditions, { Field: '', Op: '=', Value: '' }] },
    })
  }
  const removeCond = (i: number) => {
    const next = query.Conditions.filter((_, idx) => idx !== i)
    useStore.getState().setTab(fileId, tab.tabId, { query: { ...query, Conditions: next } })
  }

  const setRange = (range: [Dayjs | null, Dayjs | null] | null) => {
    const tr: TimeRange | null =
      range && (range[0] || range[1])
        ? {
            From: range[0] ? range[0].valueOf() : null,
            To: range[1] ? range[1].valueOf() : null,
          }
        : null
    useStore.getState().setTab(fileId, tab.tabId, { query: { ...query, TimeRange: tr } })
  }

  const doSearch = () => {
    runSearch(fileId, tab.tabId).catch((e) => {
      message.error(`检索失败：${errText(e)}`)
    })
  }
  const doCancel = () => {
    cancelTabSearch(fileId, tab.tabId).catch(() => {})
  }

  const fieldOptions = fields.map((f) => ({ value: f.Name, label: `${f.Name}（${f.Type}）` }))
  const rangeValue = query.TimeRange
    ? ([
        query.TimeRange.From !== null ? dayjs(query.TimeRange.From) : null,
        query.TimeRange.To !== null ? dayjs(query.TimeRange.To) : null,
      ] as [Dayjs | null, Dayjs | null])
    : null

  return (
    <div className="query-builder">
      <Space orientation="vertical" size={4} style={{ width: '100%' }}>
        {query.Conditions.map((c, i) => (
          <Space key={i} wrap size={6}>
            <Select
              style={{ width: 200 }}
              placeholder="字段名（可搜索）"
              value={c.Field || undefined}
              onChange={(v) => updateCond(i, { Field: v ?? '' })}
              options={fieldOptions}
              showSearch
              allowClear
              notFoundContent="字段列表为空：可自由输入字段名"
            />
            <Select
              style={{ width: 92 }}
              value={c.Op}
              onChange={(v) => updateCond(i, { Op: v })}
              options={OP_OPTIONS}
            />
            <ValueCell
              fileId={fileId}
              field={c.Field}
              fieldType={fields.find((f) => f.Name === c.Field)?.Type}
              value={c.Value}
              onChange={(v) => updateCond(i, { Value: v })}
              onPressEnter={doSearch}
            />
            <Button
              type="text"
              icon={<MinusCircleOutlined />}
              onClick={() => removeCond(i)}
              aria-label="删除条件"
            />
          </Space>
        ))}
        <Space wrap size={8}>
          <Button icon={<PlusOutlined />} onClick={addCond} disabled={tab.searching}>
            条件
          </Button>
          <Input
            style={{ width: 260 }}
            placeholder="关键字（整行文本匹配）"
            value={query.Keyword}
            allowClear
            onChange={(e) =>
              useStore.getState().setTab(fileId, tab.tabId, {
                query: { ...query, Keyword: e.target.value },
              })
            }
            onPressEnter={doSearch}
          />
          <DatePicker.RangePicker
            showTime
            value={rangeValue}
            onChange={(r) => setRange(r)}
            placeholder={['起始时间', '结束时间']}
            style={{ width: 360 }}
          />
          {tab.searching ? (
            <Button danger icon={<StopOutlined />} onClick={doCancel}>
              取消检索
            </Button>
          ) : (
            <Button type="primary" icon={<SearchOutlined />} onClick={doSearch}>
              检索
            </Button>
          )}
        </Space>
      </Space>
      {tab.searching && (
        <Progress
          percent={tab.searchPercent}
          size="small"
          status="active"
          style={{ maxWidth: 420, marginTop: 6 }}
        />
      )}
    </div>
  )
}
