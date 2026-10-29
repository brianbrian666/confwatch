package confwatch_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/brianbrian666/confwatch"
	"github.com/brianbrian666/confwatch/source/file"
)

// fakeWatcher 由测试显式驱动，用来在没有真实文件系统事件的情况下确定性地
// 验证 Manager 的监听生命周期与更新语义。
type fakeWatcher struct {
	started chan struct{}
	trigger chan struct{}
	fails   chan error
	// delivered 让 fire 同时等到 notify 真正执行完成，这样“触发已投递”
	// 就是一个确定的同步点。
	delivered chan struct{}
	once      sync.Once
}

func newFakeWatcher() *fakeWatcher {
	return &fakeWatcher{
		started:   make(chan struct{}),
		trigger:   make(chan struct{}),
		fails:     make(chan error),
		delivered: make(chan struct{}),
	}
}

func (f *fakeWatcher) Run(ctx context.Context, notify func()) error {
	f.once.Do(func() { close(f.started) })
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-f.trigger:
			notify()
			f.delivered <- struct{}{}
		case err := <-f.fails:
			return err
		}
	}
}

// fire 同步地投递一次“可能变了”：返回时 notify 已执行完毕。
// 监听已停止时会一直阻塞，因此只在确认监听仍在运行时使用。
func (f *fakeWatcher) fire() {
	f.trigger <- struct{}{}
	<-f.delivered
}

// tryFire 在监听仍在运行时投递一次触发并返回 true。
func (f *fakeWatcher) tryFire() bool {
	select {
	case f.trigger <- struct{}{}:
		return true
	default:
		return false
	}
}

func (f *fakeWatcher) fail(err error) { f.fails <- err }

// startWatch 启动 Watch 并等到监听器真正开始运行。
func startWatch(t *testing.T, m *confwatch.Manager[testConfig], ctx context.Context, fw *fakeWatcher) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- m.Watch(ctx) }()
	select {
	case <-fw.started:
	case <-time.After(3 * time.Second):
		t.Fatal("监听器未启动")
	}
	return done
}

func waitWatchDone(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Watch 正常停止时必须返回 nil, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Watch 未在超时内返回")
	}
}

// waitFor 在超时前轮询条件成立。它替代对异步事件顺序的硬编码假设，只用于测试。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待 %s 超时", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestWatchRequiresWatcher(t *testing.T) {
	m := openManager(t, newSource(`{"addr":":8080"}`))

	err := m.Watch(context.Background())
	if !errors.Is(err, confwatch.ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
}

func TestWatchRejectsNilContext(t *testing.T) {
	fw := newFakeWatcher()
	m := openManager(t, newSource(`{"addr":":8080"}`), confwatch.WithWatcher(fw))

	//nolint:staticcheck // 故意传 nil context 验证防御
	err := m.Watch(nil)
	if !errors.Is(err, confwatch.ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
}

func TestWatchPublishesOnTrigger(t *testing.T) {
	src := newSource(`{"addr":":8080"}`)
	fw := newFakeWatcher()
	m := openManager(t, src, confwatch.WithWatcher(fw))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := startWatch(t, m, ctx, fw)

	src.setData(`{"addr":":9090"}`)
	fw.fire()

	waitFor(t, "版本 2", func() bool { return m.Get().Version() == 2 })
	if got := m.Get().Value().Addr; got != ":9090" {
		t.Fatalf("addr = %q, want :9090", got)
	}
	if got := m.Get().Reason().Kind; got != confwatch.ReasonFileChanged {
		t.Fatalf("reason = %v, want file-changed", got)
	}
	if err := m.LastError(); err != nil {
		t.Fatalf("LastError = %v, want nil", err)
	}

	cancel()
	waitWatchDone(t, done)
}

// TestWatchKeepsSnapshotOnInvalidContent 覆盖"内容不完整"：解析失败时旧配置必须
// 逐字节不变，错误可读，且后续触发仍能成功。
func TestWatchKeepsSnapshotOnInvalidContent(t *testing.T) {
	src := newSource(`{"addr":":8080"}`)
	fw := newFakeWatcher()

	var hookErr error
	var mu sync.Mutex
	m := openManager(t, src,
		confwatch.WithWatcher(fw),
		confwatch.WithErrorHook(func(err error) {
			mu.Lock()
			defer mu.Unlock()
			hookErr = err
		}),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := startWatch(t, m, ctx, fw)

	before := m.Get()
	src.setData(`{"addr":":9090"`) // 截断的 JSON
	fw.fire()

	waitFor(t, "记录错误", func() bool { return m.LastError() != nil })
	if !errors.Is(m.LastError(), confwatch.ErrDecode) {
		t.Fatalf("LastError = %v, want ErrDecode", m.LastError())
	}
	assertSnapshotUnchanged(t, before, m.Get())

	mu.Lock()
	gotHook := hookErr
	mu.Unlock()
	if !errors.Is(gotHook, confwatch.ErrDecode) {
		t.Fatalf("ErrorHook 收到 %v, want ErrDecode", gotHook)
	}

	// 文件被补全：下一次触发成功发布。
	src.setData(`{"addr":":9090"}`)
	fw.fire()
	waitFor(t, "版本 2", func() bool { return m.Get().Version() == 2 })
	if got := m.Get().Value().Addr; got != ":9090" {
		t.Fatalf("addr = %q, want :9090", got)
	}
	if err := m.LastError(); err != nil {
		t.Fatalf("恢复后 LastError = %v, want nil", err)
	}

	cancel()
	waitWatchDone(t, done)
}

func TestWatchKeepsSnapshotOnValidationFailure(t *testing.T) {
	src := newSource(`{"addr":":8080"}`)
	fw := newFakeWatcher()
	m := openManager(t, src,
		confwatch.WithWatcher(fw),
		confwatch.WithValidator(confwatch.ValidatorFunc[testConfig](func(_ context.Context, cfg *testConfig) error {
			if cfg.Addr == "" {
				return errors.New("addr is required")
			}
			return nil
		})),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := startWatch(t, m, ctx, fw)

	before := m.Get()
	src.setData(`{"addr":""}`)
	fw.fire()

	waitFor(t, "记录校验错误", func() bool { return m.LastError() != nil })
	if !errors.Is(m.LastError(), confwatch.ErrValidation) {
		t.Fatalf("LastError = %v, want ErrValidation", m.LastError())
	}
	assertSnapshotUnchanged(t, before, m.Get())

	// 监听未被打断：修好配置后仍可发布。
	src.setData(`{"addr":":7070"}`)
	fw.fire()
	waitFor(t, "版本 2", func() bool { return m.Get().Version() == 2 })

	cancel()
	waitWatchDone(t, done)
}

// TestWatchKeepsSnapshotWhenFileMissing 覆盖"文件暂时不存在"以及随后的重建。
func TestWatchKeepsSnapshotWhenFileMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.json")
	if err := os.WriteFile(path, []byte(`{"addr":":8080"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	fw := newFakeWatcher()
	m := openManager(t, file.New(path), confwatch.WithWatcher(fw))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := startWatch(t, m, ctx, fw)

	before := m.Get()
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	fw.fire()

	waitFor(t, "记录读取错误", func() bool { return m.LastError() != nil })
	if !errors.Is(m.LastError(), confwatch.ErrSource) {
		t.Fatalf("LastError = %v, want ErrSource", m.LastError())
	}
	if !errors.Is(m.LastError(), fs.ErrNotExist) {
		t.Fatalf("LastError 应保留底层错误: %v", m.LastError())
	}
	assertSnapshotUnchanged(t, before, m.Get())

	// 文件重新出现：旧配置在缺文件期间一直可读，恢复后正常发布。
	if err := os.WriteFile(path, []byte(`{"addr":":9090"}`), 0o600); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	fw.fire()
	waitFor(t, "版本 2", func() bool { return m.Get().Version() == 2 })
	if got := m.Get().Value().Addr; got != ":9090" {
		t.Fatalf("addr = %q, want :9090", got)
	}

	cancel()
	waitWatchDone(t, done)
}

func TestWatchStopsOnContextCancel(t *testing.T) {
	src := newSource(`{"addr":":8080"}`)
	fw := newFakeWatcher()
	m := openManager(t, src, confwatch.WithWatcher(fw))

	ctx, cancel := context.WithCancel(context.Background())
	done := startWatch(t, m, ctx, fw)

	cancel()
	waitWatchDone(t, done)
	cancel() // 重复取消是安全的

	// 停止后不再有后台工作：监听器的接收者已经退出。
	if fw.tryFire() {
		t.Fatal("Watch 返回后监听器仍在运行")
	}
	src.setData(`{"addr":":9090"}`)
	if got := m.Get().Version(); got != 1 {
		t.Fatalf("停止后版本不应变化, got %v", got)
	}
}

func TestWatchStopsOnClose(t *testing.T) {
	src := newSource(`{"addr":":8080"}`)
	fw := newFakeWatcher()
	m := openWith[testConfig](t, src, confwatch.WithWatcher(fw))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := startWatch(t, m, ctx, fw)

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitWatchDone(t, done)
	if err := m.Close(); err != nil {
		t.Fatalf("重复 Close: %v", err)
	}

	if fw.tryFire() {
		t.Fatal("Close 之后监听器仍在运行")
	}
	if err := m.Watch(ctx); !errors.Is(err, confwatch.ErrClosed) {
		t.Fatalf("关闭后 Watch = %v, want ErrClosed", err)
	}
}

func TestWatchReturnsWatcherFailure(t *testing.T) {
	fw := newFakeWatcher()
	var hookErr error
	var mu sync.Mutex
	m := openManager(t, newSource(`{"addr":":8080"}`),
		confwatch.WithWatcher(fw),
		confwatch.WithErrorHook(func(err error) {
			mu.Lock()
			defer mu.Unlock()
			hookErr = err
		}),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := startWatch(t, m, ctx, fw)

	fw.fail(errors.New("inotify queue overflow"))

	select {
	case err := <-done:
		if !errors.Is(err, confwatch.ErrWatcher) {
			t.Fatalf("Watch = %v, want ErrWatcher", err)
		}
		var e *confwatch.Error
		if !errors.As(err, &e) || e.Op != "Watch" {
			t.Fatalf("err = %#v, want *Error with Op=Watch", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("监听器失败后 Watch 未返回")
	}

	if !errors.Is(m.LastError(), confwatch.ErrWatcher) {
		t.Fatalf("LastError = %v, want ErrWatcher", m.LastError())
	}
	mu.Lock()
	gotHook := hookErr
	mu.Unlock()
	if !errors.Is(gotHook, confwatch.ErrWatcher) {
		t.Fatalf("ErrorHook 收到 %v, want ErrWatcher", gotHook)
	}

	// 监听失败不影响已有配置，也不影响手动 Reload。
	if m.Get().Version() != 1 {
		t.Fatalf("监听失败不应改变快照")
	}
	if _, err := m.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
}

func TestWatchRejectsSecondWatch(t *testing.T) {
	fw := newFakeWatcher()
	m := openManager(t, newSource(`{"addr":":8080"}`), confwatch.WithWatcher(fw))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := startWatch(t, m, ctx, fw)

	if err := m.Watch(ctx); !errors.Is(err, confwatch.ErrAlreadyWatching) {
		t.Fatalf("第二个 Watch = %v, want ErrAlreadyWatching", err)
	}

	cancel()
	waitWatchDone(t, done)
}

func TestWatchIsRestartable(t *testing.T) {
	src := newSource(`{"addr":":8080"}`)
	fw := newFakeWatcher()
	m := openManager(t, src, confwatch.WithWatcher(fw))

	ctx1, cancel1 := context.WithCancel(context.Background())
	done1 := startWatch(t, m, ctx1, fw)
	cancel1()
	waitWatchDone(t, done1)

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	done2 := startWatch(t, m, ctx2, fw)

	src.setData(`{"addr":":9090"}`)
	fw.fire()
	waitFor(t, "版本 2", func() bool { return m.Get().Version() == 2 })

	cancel2()
	waitWatchDone(t, done2)
}

// TestWatchCoalescesTriggersWhileReloading 验证"多个事件合并"：一次慢速重载
// 期间连续到达的触发只会在信箱里留下一个待处理信号，不会排队重放。
func TestWatchCoalescesTriggersWhileReloading(t *testing.T) {
	src := newGateSource(`{"addr":":8080"}`)
	fw := newFakeWatcher()
	m := openManager(t, src, confwatch.WithWatcher(fw))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := startWatch(t, m, ctx, fw)

	gate := make(chan struct{})
	entered := make(chan struct{}, 8)
	src.setGate(gate, entered)
	src.setData(`{"addr":":9090"}`)

	fw.fire()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("重载未进入 Load")
	}

	// 重载卡在 Load 上时连续触发 5 次：信箱容量为 1，多余的被丢弃。
	for i := 0; i < 5; i++ {
		fw.fire()
	}
	close(gate)

	waitFor(t, "合并后的重载完成", func() bool {
		s := m.Stats()
		return s.Attempts == 3 && s.Noops == 1
	})

	// 内容未变，合并后的那一轮被摘要判幂等跳过。
	if got := m.Stats().Published; got != 2 {
		t.Fatalf("Published = %d, want 2", got)
	}

	cancel()
	waitWatchDone(t, done)
}

// TestWatchErrorHookPanicIsContained 验证用户回调 panic 不会把监听循环带走。
func TestWatchErrorHookPanicIsContained(t *testing.T) {
	src := newSource(`{"addr":":8080"}`)
	fw := newFakeWatcher()
	m := openManager(t, src,
		confwatch.WithWatcher(fw),
		confwatch.WithErrorHook(func(error) { panic("boom") }),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := startWatch(t, m, ctx, fw)

	src.setData(`not json`)
	fw.fire()
	waitFor(t, "记录错误", func() bool { return m.LastError() != nil })

	// 监听仍然存活。
	src.setData(`{"addr":":9090"}`)
	fw.fire()
	waitFor(t, "版本 2", func() bool { return m.Get().Version() == 2 })

	cancel()
	waitWatchDone(t, done)
}

// TestWatchConcurrentWithManualReload 覆盖文件事件与手动 Reload 并发：两者共用
// 同一个写者令牌，版本必须单调递增且不重复。
func TestWatchConcurrentWithManualReload(t *testing.T) {
	src := newSource(`{"addr":":8080"}`)
	fw := newFakeWatcher()
	m := openManager(t, src, confwatch.WithWatcher(fw))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := startWatch(t, m, ctx, fw)

	stop := make(chan struct{})
	observed := make(chan confwatch.Version, 1024)
	var sampler sync.WaitGroup
	sampler.Add(1)
	go func() {
		defer sampler.Done()
		var last confwatch.Version
		for {
			select {
			case <-stop:
				return
			default:
			}
			if v := m.Get().Version(); v != last {
				last = v
				select {
				case observed <- v:
				default:
				}
			}
		}
	}()

	var writers sync.WaitGroup
	for i := 0; i < 4; i++ {
		writers.Add(1)
		go func(i int) {
			defer writers.Done()
			for j := 0; j < 25; j++ {
				src.setData(fmt.Sprintf(`{"addr":":80%d%d"}`, i, j))
				fw.fire()
				if _, err := m.Reload(ctx); err != nil {
					t.Errorf("Reload: %v", err)
					return
				}
			}
		}(i)
	}
	writers.Wait()
	close(stop)
	sampler.Wait()
	close(observed)

	var prev confwatch.Version
	for v := range observed {
		if v <= prev {
			t.Fatalf("版本必须严格递增: %v -> %v", prev, v)
		}
		prev = v
	}
	if got, want := m.Stats().Published, uint64(m.Get().Version()); got != want {
		t.Fatalf("Published = %d, Version = %d", got, want)
	}
	if got, want := m.Stats().Attempts, m.Stats().Published; got < want {
		t.Fatalf("Attempts = %d < Published = %d", got, want)
	}

	cancel()
	waitWatchDone(t, done)
}
