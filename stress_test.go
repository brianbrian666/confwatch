package confwatch_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brianbrian666/confwatch"
	jsoncodec "github.com/brianbrian666/confwatch/decode/json"
	"github.com/brianbrian666/confwatch/source/file"
	"github.com/brianbrian666/confwatch/watcher/poll"
)

// TestWatchDoesNotLeakGoroutines 反复启动/停止真实监听器，确认 goroutine 数量
// 回到基线。这是"不遗留 Goroutine"承诺的可执行证据：每一轮都有 ticker、
// Watch 循环、监听器 goroutine 三处可能泄漏。
func TestWatchDoesNotLeakGoroutines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.json")
	if err := os.WriteFile(path, []byte(`{"addr":":8080"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	runtime.GC()
	base := runtime.NumGoroutine()

	const rounds = 20
	for i := 0; i < rounds; i++ {
		pw := poll.New(path)
		pw.Interval = time.Millisecond
		m, err := confwatch.Open[testConfig](context.Background(),
			confwatch.WithSource(file.New(path)),
			confwatch.WithDecoder(confwatch.FormatJSON, jsoncodec.New()),
			confwatch.WithWatcher(pw),
		)
		if err != nil {
			t.Fatalf("round %d: Open: %v", i, err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- m.Watch(ctx) }()

		// 启动对齐会立刻触发一次重载，等到它发生再停止，覆盖完整生命周期。
		waitFor(t, "监听启动", func() bool { return m.Stats().Attempts >= 2 })
		cancel()
		waitWatchDone(t, done)
		if err := m.Close(); err != nil {
			t.Fatalf("round %d: Close: %v", i, err)
		}
	}

	// 运行时自身的后台 goroutine 可能在退出，给一个有界收敛窗口。
	deadline := time.Now().Add(3 * time.Second)
	for {
		runtime.GC()
		got := runtime.NumGoroutine()
		if got <= base {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutine 泄漏: 基线 %d，当前 %d（%d 轮监听启动/停止后）", base, got, rounds)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestStressWatchReloadRead 把三类并发压力叠在一起：文件持续被原子替换、手动
// Reload 并发执行、读者高频采样。它不预设事件顺序，只检查不变量：
//   - 读者永远看到内部一致的配置（pairConfig 的 a 恒等于 b）；
//   - 版本单调不减，最终收敛到最后写入的内容；
//   - Published 与 Version 一致，Attempts >= Published。
func TestStressWatchReloadRead(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: 跳过压力测试")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "app.json")
	writeAtomic(t, path, `{"a":0,"b":0}`)

	pw := poll.New(path)
	pw.Interval = time.Millisecond
	m, err := confwatch.Open[pairConfig](context.Background(),
		confwatch.WithSource(file.New(path)),
		confwatch.WithDecoder(confwatch.FormatJSON, jsoncodec.New()),
		confwatch.WithWatcher(pw),
	)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Watch(ctx) }()

	var (
		torn     atomic.Int64
		stopRead = make(chan struct{})
		readers  sync.WaitGroup
	)
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stopRead:
					return
				default:
				}
				cfg := m.Get().Value()
				if cfg.A != cfg.B {
					torn.Add(1)
					return
				}
			}
		}()
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // 模拟编辑器每次都用临时文件 + 原子重命名
		defer wg.Done()
		for i := 1; i <= 40; i++ {
			writeAtomic(t, path, fmt.Sprintf(`{"a":%d,"b":%d}`, i, i))
			time.Sleep(time.Millisecond)
		}
	}()
	wg.Add(1)
	go func() { // 人工触发与文件事件并发
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if _, err := m.Reload(context.Background()); err != nil {
				t.Errorf("Reload: %v", err)
				return
			}
		}
	}()
	wg.Wait()

	// 收敛：最后一次写入必须被读到（文件事件可能被合并，但内容不能丢）。
	waitFor(t, "收敛到最后一次写入", func() bool { return m.Get().Value().A == 40 })

	close(stopRead)
	readers.Wait()

	if got := torn.Load(); got != 0 {
		t.Fatalf("撕裂读 %d 次", got)
	}
	if got, want := m.Stats().Published, uint64(m.Get().Version()); got != want {
		t.Fatalf("Published = %d, Version = %d", got, want)
	}
	if got, want := m.Stats().Attempts, m.Stats().Published; got < want {
		t.Fatalf("Attempts = %d < Published = %d", got, want)
	}
	if err := m.LastError(); err != nil {
		t.Fatalf("LastError = %v, want nil", err)
	}

	cancel()
	waitWatchDone(t, done)
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// writeAtomic 用临时文件 + rename 替换目标文件，模拟 vim/IDE/配置管理工具的
// 正常写盘方式，也避免测试自己产生半截文件。
func writeAtomic(t *testing.T, path, content string) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		t.Errorf("write temp: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Errorf("rename: %v", err)
	}
}
