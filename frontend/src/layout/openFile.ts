// 打开文件的共享逻辑（侧栏按钮与空状态按钮共用）。

import { api } from '../api'
import { syncIndex, useStore } from '../store/useStore'

/** 系统对话框选择并打开日志；用户取消返回 false；失败抛出（调用方负责提示）。 */
export async function openViaDialog(): Promise<boolean> {
  const info = await api.openFileDialog()
  if (!info) return false
  useStore.getState().addFile(info)
  // 打开后立即对账一次索引进度：极小文件可能在事件订阅生效前就绪
  syncIndex(info.ID).catch(() => {})
  return true
}
