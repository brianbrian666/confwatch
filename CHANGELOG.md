# Changelog

本项目的所有重要变更记录在此文件中，格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

> **兼容性承诺（自 v1.0.0 起生效）**：`1.x` 系列**不引入破坏性变更**。新增能力以 minor 版本发布，
> 缺陷修复以 patch 版本发布；确需破坏兼容时发布 `v2.0.0`。
> 发布前只有开发期快照（未打 tag），不构成对外承诺。

## [Unreleased]

## [1.1.0] - 2026-10-30

生产级可靠性审计后的第二个发布版本。**无破坏性变更**：没有删除、重命名任何导出标识符，
没有修改任何函数签名，没有删除任何结构体字段。按 minor 发布是因为两处**行为收紧**（依赖旧行为的
代码需要各自检查，详见 Changed）。

### Fixed

- **`watcher/fsnotify`：父目录被删除或改名后监听静默失效（审计 P0）**。fsnotify 会在目录 inode
  消失后从内部表摘掉 watch，此后不再投递任何事件也不报错；而唯一能反映失效的事件 `Name` 是
  目录本身，被针对文件名的过滤丢掉。修复后 `Run` 返回 `fsnotify: watched directory ... is gone`，
  `Watch` 将其归约为 `ErrWatcher` 并写入 `LastError()` 与 `ErrorHook`。
  （修复前已实测：`rm -rf` 目录再重建并写入，2s 内无通知、无错误、`Run` 不返回。）
- **取消与关闭不再计入 `Stats().Failed` 与 `LastError()`**。之前优雅关闭时被打断的那次监听重载
  会留下一条假故障，使基于 `Failed > 0` 或 `LastError != nil` 的告警在每次重启时误报。
  取消/关闭的错误仍照常返回给调用方，也会计入 `Attempts`，但不计入 `Failed`/`Noops`。

### Changed

- **行为收紧 1**：`watcher/fsnotify` 的 `Run` 在父目录消失时会返回错误。若有代码依赖“`Run` 永不返回”
  的旧行为，需要在返回值上做处理（重建 `Watcher` 或向上报错）。
- **行为收紧 2**：`ErrCanceled`/`ErrClosed` 不再写入 `LastError()`、不再计入 `Stats().Failed`。
  监控口径从“含取消的失败数”变为“不含取消的失败数”。
- `watcher/fsnotify` 的 `match` 注释不再声称将 `Chmod` 放行（实现一直是有意排除，现在文档与代码一致）。
- `Close` 与 `LastError` 的文档补充说明：关闭本身既不写入也不清除 `LastError`。

### Docs & Tests

- `doc.go`/README 明确两件事：`Watch` 的 ctx 决定整个监听生命周期（不要把带 `WithTimeout` 的
  启动 ctx 复用给它）；库只对 `Watcher.Run` 与 `ErrorHook` 的 panic 兜底，配置源/解码器/校验器的
  panic 按 Go 默认行为终止进程。
- README 补充：`decode/json` 默认不拒绝未知字段（可用 `DisallowUnknownFields` 收紧）、
  `Stats` 计数器不是原子截面、`poll.Interval` 无下限、`yaml.v3` 上游低维护、
  `fsnotify` 遇卸载/`IN_IGNORED` 无事件可依的残留限制，以及监听器对比表新增“父目录被删除/卸载”一行。
- `examples/watchfile` 在主 `select` 里直接处理监听器退出，不再“带着已停的热更新继续跑”。
- CI 的并发稳定性作业从根包扩到 `./...`，让依赖真实内核事件的用例（目录删除等）也进入重复验证。
- 新增测试：`TestRunFailsWhenWatchedDirGone`、`TestWatchedDirGone`、
  `TestWatchReportsWatcherDeathWhenDirGone`、`TestCanceledReloadIsNotAFailure`、
  `TestWatchStopsWhenDeadlineExpires`、`TestValidatorPanicPropagates`、
  `TestNoopReloadReturnsCurrentSnapshot`；删除 e2e 里用 `time.Sleep(100ms)` 的弱断言，
  改为结构性断言（`Watch` 已 join + `Reload`/`Watch` 返回 `ErrClosed`）。

## [1.0.0] - 2026-10-29

首个公开发布版本，也是 API 兼容性承诺的起点。

### 核心能力

**快照与发布**

- `Manager[T]`：`Open` / `Get` / `Reload` / `TryReload` / `Watch` / `LastError` / `Stats` / `Close`。
- 原子发布：`atomic.Pointer` + 私有 `snapshotBox[T]`，版本号、摘要、发布时间、配置值同属一个对象，
  读者不会读到撕裂状态。
- 写者令牌（容量 1 的 channel，可被 ctx 取消）串行化全部写入：版本号单调、不重复、不覆盖。
- 流水线：读取 → 解码 → 深拷贝 → 默认值 → 全部校验（`errors.Join` 聚合）→ 摘要 → 发布。
- 摘要幂等：内容未变化不发布、不递增版本（`WithNoopSkip(false)` 可关闭）。
- 读路径无锁、无分配：一次原子加载 + 一次结构体拷贝（`TestGetDoesNotAllocate` 断言）。
- 失败隔离：任一阶段失败都不触碰当前快照，只写 `LastError()` 并回调 `ErrorHook`。

**扩展点**

- 一方法接口：`Source`（`Load`/`Describe`）、`Decoder`（`Decode`）、`Validator[T]`、`Watcher`（`Run`）。
- 选项：`WithName` / `WithSource` / `WithDecoder` / `WithValidator` / `WithDefaults` /
  `WithCopyFunc` / `WithNoopSkip` / `WithWatcher` / `WithErrorHook`。

**内置实现**

- `decode/json`：仅标准库；空文档与 `null` 文档显式报错，避免静默发布零值配置。
- `decode/yaml`：`gopkg.in/yaml.v3` → JSON 数据模型，与 JSON 共用同一套 `json` 标签。
- `source/file`：按扩展名推断格式；`NewWithFormat` 支持无扩展名的挂载文件。
- `watcher/poll`：零第三方依赖，按内容摘要判定变化，天然支持原子替换与删除重建。
- `watcher/fsnotify`：监听父目录 + 去抖合并（默认 50ms），毫秒级延迟。

**监听语义**

- 监听信号经容量 1 的合并信箱进入与 `Reload` 完全相同的流水线：监听路径无法绕过校验。
- 启动对齐：监听器注册完成后立即触发一次通知，覆盖 `Open` 读取与监听建立之间的窗口，
  多余触发由摘要幂等吸收。
- 监听器 panic、未报错就返回（静默失效）统一归约为 `ErrWatcher`，不会让 `Watch` 永久阻塞，
  也不会让监听悄悄死掉。
- `Watch` 返回前 join 监听 goroutine；`Close` 与 ctx 取消都能停止监听。

**错误分类**

- `ErrNotConfigured` / `ErrNoDecoder` / `ErrSource` / `ErrDecode` / `ErrValidation` /
  `ErrNotInitialized` / `ErrCanceled` / `ErrClosed` / `ErrReloadInFlight` / `ErrWatcher` /
  `ErrAlreadyWatching`，附带 `Op` / `Name` / `Source` / `Version` / `Attempt` 诊断字段。
- 没有「发布失败」这一类：发布的唯一动作是 `atomic.Pointer.Store`，它不会失败。

### 发布前收口（1.0 自查修复）

- `Close` 改为「置 `closed` → 取消监听 → 等写者令牌」。原顺序会让 `Close` 排在一个正在等待
  ctx 取消的监听重载后面白等。
- `Watch` 增加兜底：监听器 panic 或静默退出不再导致 `Watch` 永久阻塞与 goroutine 泄漏。
  （相对开发期快照，`Watch` 的静默退出语义已收紧；因从未对外发布，不构成破坏性变更。）
- `Reason` 实现 `fmt.Stringer`（`initial` / `file-changed` / `manual: sighup`）。
- `FormatUnknown` 作为 `Format` 的具名零值；`source/file.NewWithFormat` 支持无扩展名文件。
- `Payload.Digest` 与监听器可配置字段（`poll.Interval`、`fsnotify.Debounce`）补充契约文档：
  前者必须随内容变化，后者必须在 `Run` 前设置。

### 质量与验证

- `gofmt -l .` 无输出；`go vet ./...` 无输出；`go test ./...`、`go test -race ./...` 全部通过。
- 覆盖率：根包 93.7%、decode/json 94.1%、source/file 95.7%、watcher/poll 87.2%、
  watcher/fsnotify 80.0%、decode/yaml 78.6%（语句覆盖率）。
- 关键并发证据：并发读写无撕裂（`TestConcurrentSnapshotIntegrity`）、并发 Reload 版本不覆盖
  （`TestConcurrentReloadVersionIntegrity`）、监听启停不泄漏 goroutine
  （`TestWatchDoesNotLeakGoroutines`，20 轮）、读路径零分配（`TestGetDoesNotAllocate`）、
  监听 + 手动 Reload + 高频读取三合一压力（`TestStressWatchReloadRead`）。
- 基准：`BenchmarkGet` / `GetValue` / `GetParallel` / `ReloadNoop` / `ReloadPublish` /
  `ReloadParallel`（数值见 README，标注了机器与测量方式，不构成性能承诺）。

### 依赖

- 核心包（`confwatch`、`decode/json`、`source/file`、`watcher/poll`）零第三方依赖。
- 可选：`decode/yaml` → `gopkg.in/yaml.v3`；`watcher/fsnotify` → `github.com/fsnotify/fsnotify`
  （→ `golang.org/x/sys`，indirect）。
- 最低 Go 版本 1.24（`go.mod` 声明）。

### 兼容性承诺

- **1.x 内不破坏**：`Manager[T]` 的方法集与签名、`Snapshot[T]` 的方法集、`Source`/`Decoder`/
  `Validator`/`Watcher`/`Option` 接口、`With*` 选项、错误哨兵与 `*Error` 字段、`Payload`/`Reason`/
  `Stats` 的字段名。
- **1.x 内只做增量**：变更通知、混合监听器、多层覆盖、历史回滚都可以以 minor 版本加入，
  不需要破坏现有调用方。
- **明确不承诺**：`T` 的深不可变（引用类型字段靠只读约定）、事件必达性（保证收敛，不保证每次写入都
  触发一次）、性能数字。
- 破坏兼容时发布 `v2.0.0`，并在本文件给出迁移说明。

## 开发阶段存档（均未发布，未打 tag）

- **0.2.0**：引入 `Watcher` 接口、`Manager.Watch`、`watcher/poll`、`watcher/fsnotify`、
  启动对齐、`WithErrorHook`、`ErrWatcher`/`ErrAlreadyWatching`/`ReasonFileChanged`；
  `Close` 开始取消内部 `baseCtx`。
- **0.1.0**：项目骨架——快照与原子发布、写者令牌、四类接口、`decode/json`/`decode/yaml`/
  `source/file`、错误分类、并发测试。
