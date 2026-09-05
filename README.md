# logviewer — 结构化日志查看器

基于 Wails v3 的桌面端结构化日志（JSONL）查看与检索工具。

## 项目文档

- 📐 [功能设计](./docs/functional-design.md) — 需求、数据模型、查询 DSL、Binding API
- 🛠 [实施步骤](./docs/implementation-plan.md) — 里程碑 M0–M8 与验收标准

> 下文为 Wails v3 模板自带的快速上手说明，保留供参考。

## Getting Started

1. Navigate to your project directory in the terminal.

2. To run your application in development mode, use the following command:

   ```
   wails3 dev
   ```

   This will start your application and enable hot-reloading for both frontend and backend changes.

3. To build your application for production, use:

   ```
   wails3 build
   ```

   This will create a production-ready executable in the `build` directory.

## Exploring Wails3 Features

Now that you have your project set up, it's time to explore the features that Wails3 offers:

1. **Check out the examples**: The best way to learn is by example. Visit the `examples` directory in the `v3/examples` directory to see various sample applications.

2. **Run an example**: To run any of the examples, navigate to the example's directory and use:

   ```
   go run .
   ```

   Note: Some examples may be under development during the alpha phase.

3. **Explore the documentation**: Visit the [Wails3 documentation](https://v3.wails.io/) for in-depth guides and API references.

4. **Join the community**: Have questions or want to share your progress? Join the [Wails Discord](https://discord.gg/JDdSxwjhGf) or visit the [Wails discussions on GitHub](https://github.com/wailsapp/wails/discussions).

## Project Structure

Take a moment to familiarize yourself with your project structure:

- `frontend/`: 前端代码（React + TypeScript + Vite）
- `main.go`: Go 后端入口（注册 Service 与事件）
- `greetservice.go`: 模板示例 Service（M4 阶段替换为 `internal/service`）
- `build/`、`Taskfile.yml`: Wails v3 构建配置
- `docs/`: 功能设计与实施步骤文档

## Next Steps

1. Modify the frontend in the `frontend/` directory to create your desired UI.
2. Add backend functionality in `main.go`.
3. Use `wails3 dev` to see your changes in real-time.
4. When ready, build your application with `wails3 build`.

Happy coding with Wails3! If you encounter any issues or have questions, don't hesitate to consult the documentation or reach out to the Wails community.
