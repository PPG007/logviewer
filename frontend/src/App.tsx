// 顶层布局（docs/issues/05 §4）：左「文件列表」+ 右「文件/检索 Tabs」。

import { Layout } from 'antd'
import FileSidebar from './layout/FileSidebar'
import MainPane from './layout/MainPane'

export default function App() {
  return (
    <Layout style={{ height: '100vh' }}>
      <FileSidebar />
      <MainPane />
    </Layout>
  )
}
