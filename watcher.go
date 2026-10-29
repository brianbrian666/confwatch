package confwatch

import "context"

// Watcher 检测配置是否可能发生变化。
//
// Run 在 ctx 结束或自身遇到不可恢复的错误时返回：
//   - ctx 取消或 Manager 关闭时返回 nil（属于正常停止）；
//   - 不可恢复的错误返回非 nil，Manager.Watch 会把它包装成 *Error 返回，并写入
//     LastError 与 ErrorHook。
//
// 每次"可能发生变化"时调用一次 notify。对同一次变化重复调用 notify 是允许的：
// Manager 会合并重复触发，并用载荷摘要判断内容是否真的变了。notify 必须快速返回，
// 不得阻塞，也不得在其中递归触发。
//
// 实现必须自己处理文件系统事件的不确定性：连续多次写入、临时文件加原子重命名、
// 删除后重建、事件合并或丢失、监听器自身故障。watcher/poll 与 watcher/fsnotify
// 是两个参考实现。
//
// 实现还应在“注册完成”后立即调用一次 notify（启动对齐）：从 Manager 首次读取到
// 监听真正建立之间可能已经发生了变化，而这段窗口内不会产生任何事件。多触发一次
// 不会造成多余发布，Manager 会用摘要判幂等。
type Watcher interface {
	Run(ctx context.Context, notify func()) error
}
