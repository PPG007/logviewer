// 后端进度事件 → store。仅在模块首次加载注册一次（StrictMode 双挂载安全）。

import { Events } from '@wailsio/runtime'
import type { IndexProgressEvent, SearchProgressEvent } from './types'
import { ensureFields, useStore } from './store/useStore'

let installed = false

export function installEvents(): void {
  if (installed) return
  installed = true

  Events.On('indexProgress', (e: any) => {
    const p = (e?.data ?? e) as IndexProgressEvent
    if (!p || typeof p.FileID !== 'string' || !p.FileID) return
    useStore.getState().setIndexProgress(p.FileID, p.Percent, p.Done, p.Error ?? '')
    if (p.Done && !p.Error) ensureFields(p.FileID)
  })

  Events.On('searchProgress', (e: any) => {
    const p = (e?.data ?? e) as SearchProgressEvent
    if (!p || !p.TabID) return
    // 未完成时封顶 99，最终 100 由 Search 返回后的 setTab 落定
    const percent = p.Done
      ? 100
      : p.Total > 0
        ? Math.min(99, Math.round((p.Scanned / p.Total) * 100))
        : 0
    useStore.getState().setTab(p.FileID, p.TabID, { searchPercent: percent })
  })
}
