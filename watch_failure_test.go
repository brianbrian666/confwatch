package confwatch_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/brianbrian666/confwatch"
)

// panicWatcher 模拟实现有 bug 的第三方监听器。
type panicWatcher struct{}

func (panicWatcher) Run(context.Context, func()) error { panic("boom") }

// TestWatchRecoversWatcherPanic 验证监听器 panic 不会让 Watch 永久阻塞。
//
// 这是真实存在的失效路径：Manager 无法约束外部 Watcher 的实现。如果没有
// recover 兜底，承载 Watcher 的 goroutine 会在发送结果前就死掉，Watch 会永远
// 卡在 <-watcherDone 上，既拿不到错误，也无法回收。
func TestWatchRecoversWatcherPanic(t *testing.T) {
	m := openManager(t, newSource(`{"addr":":8080"}`), confwatch.WithWatcher(panicWatcher{}))

	var hookErr error
	done := make(chan error, 1)
	go func() { done <- m.Watch(context.Background()) }()

	select {
	case err := <-done:
		if !errors.Is(err, confwatch.ErrWatcher) {
			t.Fatalf("Watch = %v, want ErrWatcher", err)
		}
		if !strings.Contains(err.Error(), "panicked") {
			t.Fatalf("Watch = %v, want it to mention the panic", err)
		}
		hookErr = m.LastError()
	case <-time.After(3 * time.Second):
		t.Fatal("监听器 panic 之后 Watch 没有返回，goroutine 已泄漏")
	}
	if hookErr == nil || !errors.Is(hookErr, confwatch.ErrWatcher) {
		t.Fatalf("LastError = %v, want ErrWatcher", hookErr)
	}
	if got := m.Get().Version(); got != 1 {
		t.Fatalf("version = %d, want 1：监听器故障不应影响已发布的配置", got)
	}
}

// silentWatcher 模拟“没有触发取消就自己返回”的监听器，例如内部循环因为某个
// channel 被关闭而退出。这属于监听能力静默失效，不能被当成正常停止。
type silentWatcher struct{}

func (silentWatcher) Run(context.Context, func()) error { return nil }

// TestWatchTreatsSilentWatcherExitAsFailure 验证静默退出被上报为 ErrWatcher。
func TestWatchTreatsSilentWatcherExitAsFailure(t *testing.T) {
	m := openManager(t, newSource(`{"addr":":8080"}`), confwatch.WithWatcher(silentWatcher{}))

	done := make(chan error, 1)
	go func() { done <- m.Watch(context.Background()) }()

	select {
	case err := <-done:
		if !errors.Is(err, confwatch.ErrWatcher) {
			t.Fatalf("Watch = %v, want ErrWatcher", err)
		}
		if !strings.Contains(err.Error(), "without error") {
			t.Fatalf("Watch = %v, want it to mention the silent stop", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Watch 没有返回")
	}
}

// TestCloseCancelsInFlightWatchReload 覆盖 Close 与监听重载并发的顺序问题：
// 监听重载被卡在 Load 里（持有写者令牌），Close 必须能取消它并返回，而不是排在
// 令牌后面白等。这是 Close 实现里“先取消、后等令牌”的直接验收。
func TestCloseCancelsInFlightWatchReload(t *testing.T) {
	src := newGateSource(`{"addr":":8080"}`)
	fw := newFakeWatcher()
	m := openWith[testConfig](t, src, confwatch.WithWatcher(fw))

	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	src.setGate(gate, entered) // 故意不 close(gate)：只能靠 ctx 取消放行

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := startWatch(t, m, ctx, fw)

	before := m.Get()
	fw.fire()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("重载未进入 Load")
	}

	closed := make(chan error, 1)
	go func() { closed <- m.Close() }()

	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close = %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close 被进行中的监听重载卡住")
	}

	waitWatchDone(t, done)
	assertSnapshotUnchanged(t, before, m.Get())
	if fw.tryFire() {
		t.Fatal("Watch 返回后监听器仍在运行")
	}
}
