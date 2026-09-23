import React from 'react'
import ReactDOM from 'react-dom/client'
import { App as AntdApp, ConfigProvider, theme as antdTheme } from 'antd'
import zhCN from 'antd/locale/zh_CN'
import dayjs from 'dayjs'
import 'dayjs/locale/zh-cn'
// 让生成的绑定把 indexProgress/searchProgress 载荷重建为强类型事件（configure 副作用）
import '../bindings/github.com/wailsapp/wails/v3/internal/eventcreate.js'
import './app.css'
import App from './App'
import { installEvents } from './events'
import { refreshRecent, useStore } from './store/useStore'

dayjs.locale('zh-cn')

// 订阅后端进度事件（模块级注册一次）
installEvents()

// 启动即拉取历史文件记录（SQLite 持久化）：侧栏「最近打开」在重启后仍可见
refreshRecent().catch(() => {})

// 主题入口：dark 状态驱动 ConfigProvider algorithm（antd 组件配色），
// 并同步 <html data-theme>（自写 CSS 变量，见 app.css）。useLayoutEffect 确保
// 首帧渲染前属性已就位，避免暗色启动时闪白。
function ThemedRoot() {
  const dark = useStore((s) => s.dark)
  React.useLayoutEffect(() => {
    document.documentElement.dataset.theme = dark ? 'dark' : 'light'
  }, [dark])
  return (
    <ConfigProvider
      locale={zhCN}
      theme={{
        algorithm: dark ? antdTheme.darkAlgorithm : antdTheme.defaultAlgorithm,
        token: { colorPrimary: '#1677ff', borderRadius: 4 },
      }}
    >
      <AntdApp>
        <App />
      </AntdApp>
    </ConfigProvider>
  )
}

ReactDOM.createRoot(document.getElementById('root') as HTMLElement).render(
  <React.StrictMode>
    <ThemedRoot />
  </React.StrictMode>,
)
