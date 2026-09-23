# M9 · 远端日志直连（SSH/SFTP）

- **状态**：已完成（2026-09-23）
- **依赖**：M8
- **关联文档**：[功能设计](../functional-design.md) §5.6（远端文件）、§8.1（Binding API）、§10（边界）、§11 决策 10

## 目标

从远端 Ubuntu 主机直接查看日志：输入连接信息（主机/端口/用户/认证）→ 浏览该路径下的目录 → 选中文件直接索引查看，**不下载到本地**。检索/分页/导出等既有能力与本地文件完全一致。

已确认的四项决策：

| 项 | 结论 |
|---|---|
| 认证方式 | 密码 + 私钥文件（私钥支持口令）；不做 ssh-agent、不做 keyboard-interactive/2FA |
| 主机指纹 | TOFU + 复用 `~/.ssh/known_hosts`；指纹变化拒绝连接 |
| 大文件策略 | 全量流式索引（与本地同语义），带进度与取消 |
| 口令存储 | 不保存。数据库里不出现任何密钥材料（**已于同日修订为明文保存，见下**） |

> 后续变更：
> 1. M10 加入了**本地内容缓存**（默认开启），因此本文件与界面文案里「不下载到本地」的说法已不再成立，改由 [M10](./10-m10-remote-cache.md) 描述。M9 当时的实现确实不落盘，此处保留历史结论。
> 2. 口令存储决策同日后修订为**明文保存在本机数据库**（连接成功后写入、可显式清除），见 [功能设计 §11 决策 12](../functional-design.md)。本文件其他「口令不落盘」的表述均以该修订为准。

## 详细实现

### 1. 字节来源抽象（`internal/logfile`）

原实现把 `*os.File` 写死在 `FileSession` 里，但真正依赖的只有三个原语：`Stat()`（大小/修改时间）、`ReadAt()`（翻页）、`Seek(0)`+顺序读（建索引/检索）。`*sftp.File` 恰好同形，因此加一个接口即可，索引/检索/分页/**导出的算法一行未改**：

```go
type Source interface {
    io.ReaderAt
    io.Reader
    io.Seeker
    Stat() (os.FileInfo, error)
    io.Closer
}

func Open(path string, onLine LineFunc) (*FileSession, error)              // 本地，签名不变
func OpenSource(src Source, displayPath, name string, onLine LineFunc) (*FileSession, error)
```

顺带补了一个缺口：原 `Close()` 不通知索引 goroutine，长任务无法中断。现在索引在每 1MB 的进度检查点检查会话是否已关闭，**远端索引可以在用户关闭文件时立刻停止拉取**（否则关一个 GB 级文件要等它传完）。

### 2. 传输层（`internal/sshconn`）

- **连接复用**：按 `user@host:port` 复用一条 SSH 连接（`Manager` 持有连接表），多个文件会话共享；30s keepalive 探测死连接，断开/删除主机时关闭。
- **认证**：`ssh.Password` 与 `ssh.PublicKeys`；私钥支持口令（`ParsePrivateKeyWithPassphrase`），路径留空时按 `~/.ssh/id_ed25519`、`id_rsa`、`id_ecdsa` 探测。
- **主机指纹**：先查本程序自己的 known_hosts（配置目录）与 `~/.ssh/known_hosts`（只读）；未记录的主机按 TOFU 记入前者（0600），**指纹变化一律拒绝**并给出两侧指纹。
- **顺序读走并发快路径**：`File.WriteTo` 写入 `io.Pipe`，由库内部按文件大小开并发工作池分块取数——吞吐取决于链路带宽而不是单包 RTT（serial 32KB/RTT 在跨地域链路上慢到不可用）。
- **随机读用独立句柄**：`sftp.File` 的 `WriteTo` 会全程持有该文件对象的写锁，同一句柄上的 `ReadAt` 会被整趟传输阻塞，且 `Close` 与该句柄的读写并非并发安全 —— 因此每次泵送新开句柄、用完即弃。
- **关闭不被链路阻塞**：关掉管道读端能让 `WriteTo` 立即停止写，但它返回前还要等已发出的 SFTP 读请求回来（在途窗口约 2MB）。慢链路上这几 MB 可能要好几秒，而「关闭文件」是点了就要有反馈的操作，故只等 500ms 就返回，残留 goroutine 自行收尾。

### 3. 持久化（`internal/store`）

`path_key` 原本是全局唯一索引，两台主机上的 `/var/log/app.log` 会撞成一条记录。**没有新增复合索引**（`AutoMigrate` 不会删旧索引，需要手写迁移），而是让 key 自带来源：

```
本地：PathKey(Normalize(path))          —— 与既有记录逐字节一致，无需迁移
远端："sftp://" + lower(user@host:port) + path
```

`File` 增列 `Kind` / `ConnID` / `Remote`；远端路径用 `path.Base`/`path.Dir` 切分（不能用 Windows 的 `filepath`）。新增 `Connection` 模型（主机/端口/用户/认证方式/私钥路径/上次浏览目录），删除主机时在事务里连带删除其文件记录。

### 4. 服务层（`internal/service`）

`openSession` 改为接受 `store.Source` + 一个「怎么拿到字节来源」的闭包，本地与远端共用同一条打开链路（字段收集、取值收集、进度事件全部复用）。新增方法：`ListConnections` / `SaveConnection` / `DeleteConnection` / `ConnectRemote` / `DisconnectRemote` / `ListRemoteDir` / `OpenRemoteFile` / `PickKeyFile`。

「最近打开」的远端条目**不在列表阶段探测存在性**（否则侧栏会被网络拖住）：有活动连接时标 `Connected`，未连接时前端显示「未连接」而不是「已丢失」；真实存在性在用户点击时（连接后 stat）判定。

### 5. 内容更新（重新加载）

「已打开就复用会话」会让远端文件在服务端被追加/轮转后**怎么点都不再拉取**。修正为：

- `logfile.FileSession.Reload(src, onLine)`：用新的字节来源重建索引，**会话 id 不变**（前端 tab 与检索条件保留）；期间状态回到 indexing，`WaitReady` 重新阻塞。取写锁时会一并等到进行中的 `Scan`/`ReadLines` 结束——不能在扫描脚下换索引；索引进行中调用则直接拒绝。完成信号 `done` 通道每轮新建，`WaitReady` 取通道改为持锁读（原先无锁读，替换通道即为数据竞争）。
- 服务层 `reuseOrReload`：重新打开同一文件时先比对大小与修改时间，**变了才重新读取**（远端不重复拉取 GB 级文件），没变直接复用；`ReloadFile` 供前端强制刷新。
- 前端：检测到「已就绪 → 又开始索引」即认定重新加载，清空各 tab 失效的行号缓存，完成后按当前条件重跑激活 tab；工具栏常驻「重新加载」按钮。

### 6. 前端

- 侧栏新增「远程主机」区（连接状态点 + 名称 + `user@host:port` + 浏览/编辑/断开/删除）与「连接远程主机」入口；已打开文件与历史记录对远端用 `CloudServerOutlined` 区分，并显示主机前缀。
- `ConnectModal`：主机/端口/用户名/认证方式/私钥路径（可浏览选择）/口令（标注「仅本次连接使用，不会保存」）。**认证失败时回滚新建的主机记录**，避免连不上的主机在侧栏堆积。
- `BrowseModal`：面包屑 + 可编辑路径 + 上级 + 快捷跳转（家目录/`/var/log`/`/tmp`/根）+「只显示日志文件」过滤（默认开，并提示隐藏了多少项，避免误以为目录为空）+ 表格；双击目录进入、双击文件打开。
- 索引中若为远端文件，提示语说明「远端文件不落盘，建索引需要完整读一遍」，把传输成本说在前面。

## 涉及文件

| 文件 | 操作 |
|---|---|
| `internal/logfile/index.go` | 改：`Source` 接口 + `OpenSource` + `Reload` + 关闭时中断索引 |
| `internal/logfile/reader.go` | 改：底层句柄类型改为 `Source`（算法不变） |
| `internal/sshconn/{conn,auth,knownhosts,browse,source}.go` | 新建 |
| `internal/testssh/sshd.go` | 新建：内嵌 sshd + sftp 子系统（`os.Root` 限定根目录，支持限速模拟慢链路） |
| `internal/sshconn/*_test.go` | 新建 |
| `internal/store/store.go` | 改：`Source`、`File` 增列、`Connection` 模型与 CRUD、`ConfigDir` |
| `internal/service/logservice.go` | 改：`openSession` 重构、DTO 增字段、远端历史记录分支 |
| `internal/service/remote.go` | 新建：远端 DTO 与方法 |
| `internal/service/remote_test.go` | 新建 |
| `frontend/src/remote/{ConnectModal,BrowseModal}.tsx` | 新建 |
| `frontend/src/{types,api}.ts`、`store/useStore.ts`、`main.tsx`、`layout/FileSidebar.tsx`、`layout/MainPane.tsx`、`app.css` | 改 |
| `frontend/bindings/**` | 重新生成 |
| `go.mod` / `go.sum` | 改：`golang.org/x/crypto`、`github.com/pkg/sftp`（均纯 Go，`CGO_ENABLED=0` 不受影响） |

## 验收标准（DoD）

- [x] 能保存主机配置、连接、浏览目录、选中文件打开并正常检索/翻页/导出
- [x] 同名但不同主机的文件是两条互不干扰的历史记录（打开次数、行数不串）
- [x] 本地历史记录的键格式不变（升级后旧记录仍可打开）
- [x] 主机指纹 TOFU：首次记录、二次直连、指纹变化被拒
- [x] ~~口令不落盘~~ → 已修订为明文落库（见开头「后续变更」；前端仍不持有口令）
- [x] 密码与私钥（含带口令私钥）两种认证都能连上；错误凭据给出可读提示
- [x] 索引中途关闭文件能立刻停止拉取，且不会把行数写成 0
- [x] 远端文件被追加/轮转后重新打开：自动重新拉取（会话 id 不变、新行可检索），未变化时不重复拉取
- [x] 「重新加载」可强制刷新（大小与修改时间都没变的同长度改写也能刷新）
- [x] 删除主机连带删除其历史文件记录并关闭相关文件会话
- [x] `go test ./internal/...`、`go vet`、`npx tsc --noEmit`、`npm run build` 全部通过
- [x] 单元测试覆盖连接/认证/指纹/浏览/打开/索引/检索/取消（内嵌 sshd，无外部依赖）
- [ ] 真机（真实 Ubuntu 主机）验证 — 待人工执行

## 风险与注意

1. **索引 GB 级远端文件需要完整传输一遍**。已按决策接受；用并发预取把吞吐拉到链路上限，并提供进度与取消。后续由 M10 用本地内容缓存把「第二次打开」的成本降到零。
2. 断线不做透明重连：连接断开后文件会话报错，由用户重新连接；已打开的文件会话在断开/删除主机时一并关闭。
3. 远端文件在索引期间增长：沿用本地既有的「打开时刻大小快照」语义，行为与本地一致。
4. `path` 与 `filepath` 混用是这类功能最容易出的错：所有远端路径一律走 `path` 包，代码审查时重点看这部分。
5. 测试里的 sshd 是**只读**的（`Filecmd`/`Filewrite` 直接返回不支持），且 SFTP 根目录被 `os.Root` 限定，不会暴露宿主机路径。
