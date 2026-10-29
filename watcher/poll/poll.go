// Package poll 提供零第三方依赖的变化监听器：定期读取文件、比对内容摘要。
//
// 与事件式监听（watcher/fsnotify）相比，轮询把"事件是否可靠"这个问题直接消掉：
// 原子重命名、删除后重建、编辑器先写临时文件、事件被内核队列丢弃，最终都表现为
// "某次读取的内容与上次不同"。代价是最坏情况下滞后一个轮询周期，以及每个周期
// 一次文件读取与一次哈希（配置文件通常只有几 KB，开销在微秒级）。
//
// 它适合单个文件、网络文件系统、以及容器内 ConfigMap 这类符号链接切换的场景。
package poll

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"time"
)

// DefaultInterval 是未设置 Interval 时使用的轮询周期。
const DefaultInterval = time.Second

// Watcher 轮询单个文件。零值不可用，用 New 构造。
//
// 字段（Path/Interval）必须在 Run 启动前设置好；启动后修改字段属于数据竞争，
// Run 内部不会重新读取它们。Watcher 自身的状态有锁保护，State/Poll/Run 可以
// 安全并发调用；但通常只让 Run 独占使用。
type Watcher struct {
	// Path 是要观察的文件路径。必填。
	Path string
	// Interval 是轮询周期，<= 0 时使用 DefaultInterval。
	Interval time.Duration

	// ticks 允许测试注入受控的时钟，为 nil 时使用 time.Ticker。
	ticks <-chan time.Time

	mu    sync.Mutex
	known fileState
	seen  bool
}

// fileState 是"一次观察的结果"，三者互斥：摘要、读取错误、文件不存在。
//
// 直接比较结构体即可判断变化，因此连续多次修改、删除后重建、原子替换都不需要
// 额外的状态机。
type fileState struct {
	digest string // 成功读取时的内容摘要
	err    string // 读取失败时的错误描述
}

// New 构造一个轮询 Path 的监听器。
func New(path string) *Watcher {
	return &Watcher{Path: path, Interval: DefaultInterval}
}

// Poll 立即观察一次，并报告与上一次观察相比状态是否发生变化。
//
// 首次调用总是报告 true（还没有基线）。文件不存在、读取失败也算状态变化，
// 这样 Manager 能记录错误并保留旧配置，而文件恢复后又能再次触发。
func (w *Watcher) Poll() bool {
	st := w.observe()

	w.mu.Lock()
	defer w.mu.Unlock()
	changed := !w.seen || st != w.known
	w.seen, w.known = true, st
	return changed
}

// State 返回最近一次观察到的状态描述，仅用于日志与测试。
func (w *Watcher) State() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case !w.seen:
		return "unobserved"
	case w.known.err != "":
		return "error: " + w.known.err
	case w.known.digest == "":
		return "missing"
	default:
		return w.known.digest
	}
}

// Run 实现 confwatch.Watcher：阻塞轮询，直到 ctx 结束。
//
// 启动时先记录基线，然后立即调用一次 notify。这个“对齐”不能省：Open 的首次
// 读取与本监听器建立基线之间有一个窗口，若文件恰好在这个窗口内变化，仅靠比较
// 前后基线会把这次变化当成“从未发生”。多触发一次是无害的：Manager 用载荷摘要
// 判幂等，内容没变时不会发布。
func (w *Watcher) Run(ctx context.Context, notify func()) error {
	if w.Path == "" {
		return errors.New("poll: empty path")
	}
	interval := w.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}

	// 基线：记住启动时的状态，之后的轮询只报告真正的变化。
	w.Poll()
	notify()

	var ticks <-chan time.Time
	if w.ticks != nil {
		ticks = w.ticks
	} else {
		t := time.NewTicker(interval)
		defer t.Stop()
		ticks = t.C
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticks:
			if w.Poll() {
				notify()
			}
		}
	}
}

func (w *Watcher) observe() fileState {
	data, err := os.ReadFile(w.Path)
	switch {
	case err == nil:
		sum := sha256.Sum256(data)
		return fileState{digest: hex.EncodeToString(sum[:])}
	case errors.Is(err, fs.ErrNotExist):
		return fileState{}
	default:
		// 权限、占用等读取错误也是"状态变化"：交给 Manager 记录，旧配置不受影响。
		return fileState{err: err.Error()}
	}
}

// Describe 返回监听目标的描述，便于日志。
func (w *Watcher) Describe() string {
	return fmt.Sprintf("poll:%s@%s", w.Path, w.Interval)
}
