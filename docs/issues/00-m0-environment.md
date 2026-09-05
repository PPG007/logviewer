# M0 · 环境就绪

- **状态**：未开始
- **依赖**：无
- **关联文档**：[功能设计](../functional-design.md) §3 技术栈

## 目标

把 Wails v3 模板跑通，并补齐前端依赖（antd），得到一个能启动的空白应用，为后续里程碑提供可运行的基础。

## 前置条件

已确认本机工具链：

| 工具 | 版本要求 | 当前 |
|---|---|---|
| Go | ≥ 1.25 | 1.26.5 ✅ |
| Wails v3 CLI | 3.0.0-beta.x | 3.0.0-beta.3 ✅ |
| Node | ≥ 18 | 24.16.0 ✅ |
| npm | ≥ 9 | 11.13.0 ✅ |

> Windows 上还需系统已安装 **WebView2 运行时**（Win11 默认自带）。

## 任务清单

- [ ] 安装前端基础依赖（`npm install`）
- [ ] 安装 antd 与图标库
- [ ] 生成 Wails bindings
- [ ] `wails3 dev` 启动验证

## 详细步骤

### 1. 安装前端依赖

```bash
cd frontend
npm install
npm install antd @ant-design/icons
```

说明：
- `antd` v5 采用 CSS-in-JS，**无需**全局引入 CSS 文件，直接 `import { Button } from 'antd'` 即可。
- `@ant-design/icons` 提供图标（文件列表、关闭 tab、搜索等图标后续会用到）。

### 2. 生成 bindings

```bash
# 回到项目根目录
wails3 generate bindings
```

生成产物位于 `frontend/bindings/logviewer/`（目录名为 Go module 名），会被前端 `import { LogService } from "../bindings/logviewer"` 引用。**此目录为生成物，禁止手改。**

### 3. 启动验证

```bash
wails3 dev
```

预期：弹出应用窗口，模板页面（Wails + React 欢迎页）正常渲染，页脚时钟每秒跳动（说明事件链路 `app.Event.Emit("time")` → 前端 `Events.On` 已通）。

## 涉及文件

| 文件 | 操作 |
|---|---|
| `frontend/package.json` | 新增 `antd`、`@ant-design/icons` 依赖 |
| `frontend/bindings/` | 生成（勿手改） |

## 验收标准（DoD）

- [ ] `wails3 dev` 能启动窗口且无报错
- [ ] `frontend/package.json` 中 `antd`、`@ant-design/icons` 已在 dependencies
- [ ] 前端能成功 `import` 生成的 bindings（可临时在 App.tsx 加一行验证后删除）

## 风险与注意

1. **Wails v3 是 beta**：锁定 `v3.0.0-beta.3`，升级需评估 breaking change。
2. **npm 网络**：若 `npm install` 慢/失败，可配置镜像（如 `npm config set registry https://registry.npmmirror.com`）。
3. **bindings 未生成时前端 import 会报错**：必须先跑一次 `wails3 generate bindings`（`wails3 dev` 也会自动触发）。
