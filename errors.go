package confwatch

import (
	"errors"
	"fmt"
	"strings"
)

// 错误分类。用 errors.Is 判断类别，用 errors.As 取出 *Error 查看诊断字段。
//
// 有意不提供"发布失败"这一类错误：发布的唯一动作是 atomic.Pointer.Store，
// 它不会失败。所有真实的失败都在发布之前发生（读取、解码、校验、选项配置），
// 因此都为它们提供了独立的哨兵错误。
var (
	// ErrNotConfigured 表示 Open 的选项不完整或不合法，例如缺少配置源、
	// 缺少解码器、类型不匹配的选项、nil context。
	ErrNotConfigured = errors.New("invalid configuration")

	// ErrNoDecoder 表示配置源产出的载荷格式没有注册对应解码器。
	ErrNoDecoder = errors.New("no decoder for payload format")

	// ErrSource 表示从配置源读取失败。
	ErrSource = errors.New("source read failed")

	// ErrDecode 表示解析失败，例如语法错误、类型不匹配、空文档。
	ErrDecode = errors.New("decode failed")

	// ErrValidation 表示校验失败。多个校验器的错误会被 errors.Join 聚合。
	ErrValidation = errors.New("validation failed")

	// ErrNotInitialized 表示在零值 Manager（未经 Open 构造）上调用了写入方法。
	ErrNotInitialized = errors.New("manager not initialized")

	// ErrCanceled 表示加载因 context 被取消或超时而中止。返回的错误同时包装
	// context 的错误，因此 errors.Is(err, context.Canceled) 同样成立。
	ErrCanceled = errors.New("reload canceled")

	// ErrClosed 表示 Manager 已关闭，不再接受配置变更。
	ErrClosed = errors.New("manager closed")

	// ErrReloadInFlight 表示 TryReload 时已有其他发布正在进行。
	ErrReloadInFlight = errors.New("reload already in progress")
)

// Error 携带诊断上下文的错误。库产生的所有错误都是 *Error，因此可以用
// errors.As 取出 Source、Version、Attempt 等字段做日志或指标。
type Error struct {
	Op      string  // 操作名："Open"、"Reload"、"TryReload"、"Close"
	Name    string  // Manager 名称（WithName 设置），未设置时为空
	Source  string  // 配置源描述（Source.Describe），未知时为空
	Version Version // 出错时当前有效快照的版本，0 表示尚无有效快照
	Attempt uint64  // 出错时是第几次加载尝试，0 表示尚未开始加载
	Err     error
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("confwatch")
	if e.Op != "" {
		b.WriteByte('.')
		b.WriteString(e.Op)
	}
	if e.Name != "" {
		b.WriteString(" name=")
		b.WriteString(e.Name)
	}
	if e.Source != "" {
		b.WriteString(" source=")
		b.WriteString(e.Source)
	}
	if e.Version > 0 {
		fmt.Fprintf(&b, " version=%d", e.Version)
	}
	if e.Attempt > 0 {
		fmt.Fprintf(&b, " attempt=%d", e.Attempt)
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

// Unwrap 返回底层错误，使 errors.Is/errors.As 能穿透到哨兵错误与 context 错误。
func (e *Error) Unwrap() error { return e.Err }
