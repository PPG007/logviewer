# 实施步骤 — Issue 索引

> 每个里程碑一份 issue 文件，含目标、依赖、详细实现、涉及文件、验收标准（DoD）、风险。按编号顺序实施。

| # | 文件 | 主题 | 依赖 | 状态 |
|---|---|---|---|---|
| M0 | [00-m0-environment](./00-m0-environment.md) | 环境就绪 | 无 | 未开始 |
| M1 | [01-m1-logfile-index](./01-m1-logfile-index.md) | 文件会话 + 行偏移索引 | M0 | 未开始 |
| M2 | [02-m2-parse](./02-m2-parse.md) | 行解析 + 字段探测 | M0 | 未开始 |
| M3 | [03-m3-search](./03-m3-search.md) | 检索引擎 | M1、M2 | 未开始 |
| M4 | [04-m4-service](./04-m4-service.md) | Service 绑定层 | M1、M2、M3 | 未开始 |
| M5 | [05-m5-layout](./05-m5-layout.md) | 前端布局 + 状态 | M4 | 未开始 |
| M6 | [06-m6-open-display](./06-m6-open-display.md) | 打开文件 + 分页展示 | M5、M4 | 未开始 |
| M7 | [07-m7-search-tabs](./07-m7-search-tabs.md) | 检索 tab | M6、M4 | 未开始 |
| M8 | [08-m8-polish-bench](./08-m8-polish-bench.md) | 打磨 + 压测 | M7 | 未开始 |

## 关联

- [功能设计](../functional-design.md) — 需求、数据模型、DSL、Binding API、性能目标
- [实施步骤总览](../implementation-plan.md) — 里程碑总览、目录结构、测试策略、风险
