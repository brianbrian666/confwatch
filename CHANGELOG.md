# Changelog

本项目的所有重要变更记录在此文件中，格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

> **兼容性说明**：v1.0 之前，minor 版本（`0.x`）可能包含不兼容变更；每一个不兼容变更都会在
> 对应版本下以 **Breaking** 明确标出，并给出迁移方式。patch 版本只做兼容性修复。

## [Unreleased]

### Added

- `source/file.NewWithFormat(path, format)`：为没有扩展名的文件（容器挂载点常见）显式指定格式。
- `confwatch.FormatUnknown`：`Format` 的具名零值，便于诊断「未指定格式」。
- 测试：`TestWatchRecoversWatcherPanic`、`TestWatchTreatsSilentWatcherExitAsFailure`、
  `TestGetDoesNotAllocate`、`TestWatchDoesNotLeakGoroutines`、`TestStressWatchReloadRead`。
- 基准：`BenchmarkGet`、`BenchmarkGetValue`、`BenchmarkGetParallel`、`BenchmarkReloadNoop`、
  `BenchmarkReloadPublish`、`BenchmarkReloadParallel`。
- 开源交付件：`LICENSE`（MIT）、`CONTRIBUTING.md`、`SECURITY.md`、本文件、
  `.github/workflows/ci.yml`、`examples/watchfile`。

### Changed

- `Watch` 现在把「监听器未报错且未取消就返回」视为故障，返回包装了 `ErrWatcher` 的错误，
  而不再当作正常停止。监听能力静默失效必须能被观察到。
- 监听器 panic 会被 recover 并转为 `ErrWatcher`，不再导致 `Watch` 永久阻塞（goroutine 泄漏）。
- `Payload.Digest` 文档明确：摘要必须随内容变化，返回常量摘要会让真实更新被静默跳过。

### Fixed

- 监听器 panic 或静默退出时 `Watch` 永久阻塞、承载 goroutine 泄漏。

## [0.2.0] - 未发布（开发阶段）

### Added

- 文件变化监听：`Watcher` 接口 + `WithWatcher` + `Manager.Watch(ctx)`。
- `watcher/poll`：轮询实现，零第三方依赖，按内容摘要判定变化，天然支持原子替换与删除重建。
- `watcher/fsnotify`：事件实现，监听父目录、按去抖合并事件（默认 50ms）。
- **启动对齐**：两种监听器在注册完成后立即触发一次通知，覆盖 `Manager.Open` 读取与监听建立
  之间的窗口；多余触发由摘要幂等吸收。
- `WithErrorHook`：观察调用方无法收到返回值的后台失败（监听触发的重载失败、监听器致命故障）；
  同步调用，panic 会被 recover。
- `ErrWatcher`、`ErrAlreadyWatching`、`ReasonFileChanged`。
- 测试：监听触发的发布、坏内容、校验失败、文件删除、原子替换、连续修改、与手动 Reload 并发、
  停止后不再更新、监听器故障、错误钩子 panic、可重启、事件合并（容量 1 信箱）、真实文件端到端
  （fsnotify + poll）。

### Changed

- `Manager.Close` 现在会取消内部 `baseCtx`，从而终止正在运行的 `Watch`。

## [0.1.0] - 未发布（开发阶段）

### Added

- `Manager[T]`：`Open` / `Get` / `Reload` / `TryReload` / `LastError` / `Stats` / `Close`。
- 原子发布：`atomic.Pointer` + 私有 `snapshotBox[T]`，版本号在写者临界区内分配。
- 写者令牌（容量 1 的 channel，可被 ctx 取消）串行化全部写入，无版本覆盖。
- 流水线：读取 → 解码 → 深拷贝 → 默认值 → 全部校验（`errors.Join`）→ 摘要 → 发布。
- 摘要幂等：内容未变化不发布、不递增版本（可用 `WithNoopSkip(false)` 关闭）。
- 接口：`Source`、`Decoder`、`Validator[T]`、`Option`；选项 `WithName` / `WithSource` /
  `WithDecoder` / `WithValidator` / `WithDefaults` / `WithCopyFunc` / `WithNoopSkip`。
- `decode/json`（仅 stdlib，空文档与 `null` 文档显式报错）、`decode/yaml`（yaml.v3 → JSON 数据模型）、
  `source/file`（按扩展名推断格式）。
- 错误分类：`ErrNotConfigured` / `ErrNoDecoder` / `ErrSource` / `ErrDecode` / `ErrValidation` /
  `ErrNotInitialized` / `ErrCanceled` / `ErrClosed` / `ErrReloadInFlight`，附带 `*Error` 诊断字段。
- 并发测试：并发读取无撕裂、并发 Reload 版本不覆盖不重复、旧快照不被复用。
