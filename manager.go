package confwatch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// snapshotBox 让 atomic.Pointer 指向一个独立对象。发布后该对象及其中可达的数据
// 都不再被写入，这是无锁读取成立的前提。
type snapshotBox[T any] struct{ snap Snapshot[T] }

// Manager 持有一份当前有效配置，并负责以事务方式更新它。
//
// 读路径完全无锁：Get 只做一次 atomic.Pointer.Load 和一次结构体拷贝。
// 写路径由容量为 1 的 token channel 串行化：同一时刻只有一个发布在进行，
// 因此版本号单调递增、不重复，也不会相互覆盖。
//
// Manager 不能拷贝，必须通过 Open 构造。
type Manager[T any] struct {
	set settings

	cur atomic.Pointer[snapshotBox[T]]

	// token 是写者令牌。用 channel 而不是 sync.Mutex，是为了让等待令牌的过程
	// 可以被 context 取消。
	token chan struct{}

	// baseCtx 在 Close 时取消，用来停掉后台监听。它不携带任何值，只表达
	// “Manager 仍然活着”。
	baseCtx context.Context
	cancel  context.CancelFunc

	// watching 保证同一 Manager 上最多一个 Watch 在运行。
	watching atomic.Bool

	// mu 只保护下面几个诊断字段与 closed，临界区极短，不与 IO 混用。
	// 加锁顺序：token -> mu，永不反向。
	mu       sync.Mutex
	closed   bool
	lastErr  error
	attempts uint64
	failed   uint64
	noops    uint64
}

// Open 构造 Manager 并完成首次加载。成功后立即存在版本 1 的有效快照。
//
// 首次加载失败时返回 (nil, error)：不存在"Manager 已创建但没有有效配置"的中间
// 状态，避免上层拿到一份静默的零值配置。ctx 只约束本次加载，不与 Manager 的
// 生命周期绑定。
func Open[T any](ctx context.Context, opts ...Option) (*Manager[T], error) {
	if ctx == nil {
		return nil, &Error{Op: "Open", Err: fmt.Errorf("%w: nil context", ErrNotConfigured)}
	}

	var set settings
	set.init()
	for i, opt := range opts {
		if opt == nil {
			return nil, &Error{Op: "Open", Err: fmt.Errorf("%w: option #%d is nil", ErrNotConfigured, i)}
		}
		if err := opt(&set); err != nil {
			return nil, &Error{Op: "Open", Err: err}
		}
	}
	if err := set.validate(); err != nil {
		return nil, &Error{Op: "Open", Err: err}
	}

	m := &Manager[T]{set: set, token: make(chan struct{}, 1)}
	m.baseCtx, m.cancel = context.WithCancel(context.Background())
	if _, err := m.reload(ctx, "Open", Reason{Kind: ReasonInitial}, false); err != nil {
		m.cancel()
		return nil, err
	}
	return m, nil
}

// Get 返回当前有效快照。它不阻塞、不加锁、不分配，可在任意 goroutine 并发调用。
//
// 在 nil 或零值 Manager 上调用返回无效快照而不是 panic。
func (m *Manager[T]) Get() Snapshot[T] {
	if m == nil {
		return Snapshot[T]{}
	}
	if b := m.cur.Load(); b != nil {
		return b.snap
	}
	return Snapshot[T]{}
}

// Reload 重新执行完整流水线：读取 -> 解码 -> 拷贝 -> 默认值 -> 校验 -> 发布。
//
// 任何一步失败都返回错误，当前快照保持不变。若新载荷的摘要与当前快照相同，
// 则不发布、不递增版本，直接返回当前快照（见 WithNoopSkip）。
//
// 并发调用会排队等待写者令牌，等待过程可被 ctx 取消。
func (m *Manager[T]) Reload(ctx context.Context) (Snapshot[T], error) {
	return m.reload(ctx, "Reload", Reason{Kind: ReasonManualReload}, false)
}

// TryReload 与 Reload 相同，但不会等待正在进行的发布：此时立即返回
// ErrReloadInFlight。它适合由定时器或事件循环调用，避免堆积等待者。
func (m *Manager[T]) TryReload(ctx context.Context) (Snapshot[T], error) {
	return m.reload(ctx, "TryReload", Reason{Kind: ReasonManualReload}, true)
}

func (m *Manager[T]) reload(ctx context.Context, op string, reason Reason, tryOnly bool) (Snapshot[T], error) {
	if err := m.checkInitialized(op); err != nil {
		return Snapshot[T]{}, err
	}
	if ctx == nil {
		return Snapshot[T]{}, m.errAt(op, Snapshot[T]{}, 0, fmt.Errorf("%w: nil context", ErrNotConfigured))
	}
	if err := ctx.Err(); err != nil {
		return Snapshot[T]{}, m.errAt(op, Snapshot[T]{}, 0, fmt.Errorf("%w: %w", ErrCanceled, err))
	}
	if err := m.acquire(ctx, op, tryOnly); err != nil {
		return Snapshot[T]{}, err
	}
	defer m.release()

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Snapshot[T]{}, m.errAt(op, Snapshot[T]{}, 0, ErrClosed)
	}
	m.attempts++
	attempt := m.attempts
	m.mu.Unlock()

	// 只有持有令牌的 goroutine 会发布，因此这里读到的 cur 在本轮内不会被别人改。
	cur := m.Get()

	snap, err := m.build(ctx, op, cur, reason, attempt)
	if err != nil {
		// 取消与关闭不是配置失败，不能记成 Failed/LastError：否则每次优雅关闭
		// 都会留下一条假故障，基于 Failed>0 或 LastError!=nil 的告警必然误报。
		if !errors.Is(err, ErrCanceled) && !errors.Is(err, ErrClosed) {
			m.mu.Lock()
			m.failed++
			m.lastErr = err
			m.mu.Unlock()
		}
		return Snapshot[T]{}, err
	}

	if cur.Valid() && m.set.noopSkip && snap.hdr.digest == cur.hdr.digest {
		m.mu.Lock()
		m.noops++
		m.lastErr = nil
		m.mu.Unlock()
		return cur, nil
	}

	m.cur.Store(&snapshotBox[T]{snap: snap})

	m.mu.Lock()
	m.lastErr = nil
	m.mu.Unlock()
	return snap, nil
}

// build 在写者令牌内构造候选快照。它从不触碰 m.cur：失败时当前配置逐字节不变。
func (m *Manager[T]) build(ctx context.Context, op string, cur Snapshot[T], reason Reason, attempt uint64) (Snapshot[T], *Error) {
	fail := func(err error) (Snapshot[T], *Error) {
		return Snapshot[T]{}, m.errAt(op, cur, attempt, err)
	}

	if err := ctx.Err(); err != nil {
		return fail(fmt.Errorf("%w: %w", ErrCanceled, err))
	}

	payload, err := m.set.source.Load(ctx)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return fail(fmt.Errorf("%w: %w", ErrCanceled, cerr))
		}
		return fail(fmt.Errorf("%w: %w", ErrSource, err))
	}
	if len(payload.Data) == 0 {
		return fail(fmt.Errorf("%w: payload is empty", ErrDecode))
	}

	dec, ok := m.set.decoders[payload.Format]
	if !ok {
		return fail(fmt.Errorf("%w: %s", ErrNoDecoder, payload.Format))
	}

	// 每次加载都解码到一个全新的值，绝不复用上一份快照的内部缓冲区。
	var val T
	if err := dec.Decode(payload.Data, &val); err != nil {
		return fail(fmt.Errorf("%w: %w", ErrDecode, err))
	}
	if fn := m.set.copyFunc; fn != nil {
		if err := fn(&val); err != nil {
			return fail(fmt.Errorf("%w: %w", ErrNotConfigured, err))
		}
	}
	if fn := m.set.defaults; fn != nil {
		if err := fn(&val); err != nil {
			return fail(fmt.Errorf("%w: %w", ErrNotConfigured, err))
		}
	}
	if len(m.set.validators) > 0 {
		var errs []error
		for _, v := range m.set.validators {
			if err := v(ctx, &val); err != nil {
				errs = append(errs, err)
			}
		}
		if len(errs) > 0 {
			return fail(fmt.Errorf("%w: %w", ErrValidation, errors.Join(errs...)))
		}
	}

	// 校验通过后仍要尊重取消：已取消的 Reload 不应发布。
	if err := ctx.Err(); err != nil {
		return fail(fmt.Errorf("%w: %w", ErrCanceled, err))
	}

	digest := payload.Digest
	if digest == "" {
		sum := sha256.Sum256(payload.Data)
		digest = hex.EncodeToString(sum[:])
	}

	return Snapshot[T]{
		hdr: header{
			name:     m.set.name,
			version:  cur.hdr.version + 1,
			loadedAt: time.Now(),
			digest:   digest,
			reason:   reason,
		},
		val: val,
	}, nil
}

// Watch 启动变化监听并将其阻塞在当前 goroutine，直到 ctx 结束、Close 被调用，
// 或监听器自身失败。
//
// 监听路径不新开发布通道：每次触发都调用与 Reload 完全相同的流水线
// （读取 -> 解码 -> 拷贝 -> 默认值 -> 校验 -> 发布）。因此监听器无法绕过校验，
// 失败时当前配置逐字节不变，且后续触发仍会重试。
//
// 触发信号进入容量为 1 的合并信箱：连续多个事件最多排一次重载，慢速重载不会
// 让事件堆积，也不会反向阻塞监听器。
//
// 返回语义：ctx 取消或 Close 属于正常停止，返回 nil；监听器不可恢复的失败（包括
// panic 与未报错就返回）返回包装了 ErrWatcher 的错误，并同时写入 LastError 与
// ErrorHook。后台重载失败不会终止监听，只写 LastError 与 ErrorHook，旧配置继续生效。
//
// ctx 决定整个监听的生命周期，而不只是某一次加载：与 Open 不同，这里的 ctx 一旦
// 结束（包括 WithTimeout/WithDeadline 到期）监听就停止并返回 nil。不要把带超时的
// 启动 ctx 复用给 Watch，否则热更新会在超时后静默停止。
//
// 同一个 Manager 同时只允许一个 Watch；Watch 返回后可以再次启动。
func (m *Manager[T]) Watch(ctx context.Context) error {
	if err := m.checkInitialized("Watch"); err != nil {
		return err
	}
	if ctx == nil {
		return m.errAt("Watch", Snapshot[T]{}, 0, fmt.Errorf("%w: nil context", ErrNotConfigured))
	}
	w := m.set.watcher
	if w == nil {
		return m.errAt("Watch", m.Get(), 0, fmt.Errorf("%w: no watcher, use WithWatcher", ErrNotConfigured))
	}

	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return m.errAt("Watch", m.Get(), 0, ErrClosed)
	}
	if !m.watching.CompareAndSwap(false, true) {
		return m.errAt("Watch", m.Get(), 0, ErrAlreadyWatching)
	}
	defer m.watching.Store(false)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Close 也会停掉监听：baseCtx 取消时连带取消 runCtx。
	stopAfterClose := context.AfterFunc(m.baseCtx, cancel)
	defer stopAfterClose()

	// 容量为 1 的合并信箱：重复触发被丢弃，不排队、不扩容。
	wake := make(chan struct{}, 1)
	notify := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}

	watcherDone := make(chan error, 1)
	go func() { watcherDone <- runWatcher(runCtx, w, notify) }()

	for {
		select {
		case <-runCtx.Done():
			// 先等监听器退出，保证 Watch 返回后不遗留任何 goroutine。
			<-watcherDone
			return nil

		case err := <-watcherDone:
			// runWatcher 只在 ctx 结束时返回 nil，其余情况都是非正常退出。
			if err == nil {
				return nil
			}
			fatal := m.errAt("Watch", m.Get(), 0, fmt.Errorf("%w: %w", ErrWatcher, err))
			m.reportError(fatal)
			return fatal

		case <-wake:
			if _, err := m.reload(runCtx, "Watch", Reason{Kind: ReasonFileChanged}, false); err != nil {
				// reload 已经把取消/关闭排除在 Failed/LastError 之外，这里只需阻止
				// 把它们送进 ErrorHook：监听停止不是配置错误。
				if !errors.Is(err, ErrCanceled) && !errors.Is(err, ErrClosed) {
					m.reportError(err)
				}
			}
		}
	}
}

// runWatcher 启动监听器并把它归约为三类结果：nil（ctx 结束，正常停止）、错误
// （监听器自身的不可恢复故障）、错误（panic 或未报错就停止）。
//
// 这一层不能省：如果监听器 panic 或静默返回，未经包装的 goroutine 会让
// watcherDone 永远收不到值，Watch 就会永远阻塞在 <-watcherDone 上，成为一个
// 无法回收的 goroutine。用户实现的 Watcher 不受本库控制，必须在这里兜底。
func runWatcher(ctx context.Context, w Watcher, notify func()) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("watcher panicked: %v", p)
		}
	}()
	if err := w.Run(ctx, notify); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return nil
	}
	// 未报错就返回，且 ctx 仍然活着：监听已静默失效，不能当成正常停止。
	return errors.New("watcher stopped without error")
}

// reportError 把错误记录到 LastError 并通知 ErrorHook。
//
// 它只用在调用方拿不到错误的路径（监听循环）。hook 的 panic 被吞掉：回调是
// 用户的代码，不能让它把监听循环一起带走。
func (m *Manager[T]) reportError(err error) {
	m.mu.Lock()
	m.lastErr = err
	hook := m.set.onError
	m.mu.Unlock()

	if hook == nil {
		return
	}
	defer func() { _ = recover() }()
	hook(err)
}

func (m *Manager[T]) acquire(ctx context.Context, op string, tryOnly bool) error {
	if tryOnly {
		select {
		case m.token <- struct{}{}:
			return nil
		default:
			return m.errAt(op, Snapshot[T]{}, 0, ErrReloadInFlight)
		}
	}
	select {
	case m.token <- struct{}{}:
		return nil
	case <-ctx.Done():
		return m.errAt(op, Snapshot[T]{}, 0, fmt.Errorf("%w: %w", ErrCanceled, ctx.Err()))
	}
}

func (m *Manager[T]) release() { <-m.token }

// Close 标记 Manager 已关闭，并停掉正在运行的 Watch。它会等待正在进行的发布
// 结束，返回后不会再有任何新快照被发布。Close 是幂等的。
//
// 关闭后 Get 仍返回最后一份有效快照（只读安全），但 Reload/TryReload/Watch
// 返回 ErrClosed。关闭本身既不写入也不清除 LastError。注意：若某个 Reload 的配置源
// 忽略 ctx 并长时间阻塞，Close 会一直等待它结束；Watch 的退出由其自身的 Run 返回
// 保证，不在 Close 内等待。
func (m *Manager[T]) Close() error {
	if err := m.checkInitialized("Close"); err != nil {
		return err
	}

	// 顺序很重要：先拒绝新写入并取消监听，最后才等写者令牌。
	//
	// 反过来做（先等令牌再取消）会让 Close 无限期地等一个正在等 ctx 取消的
	// 监听重载：它持有令牌，而能取消它的信号恰恰排队在 Close 后面。先 cancel
	// 能让下一次 ctx 检查把它中止，令牌随即释放。
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()

	// 取消 baseCtx，让所有监听循环停止。cancel 可重复调用。
	m.cancel()

	// 拿到令牌说明所有在途发布都已结束；返回后不会再出现新版本。
	if err := m.acquire(context.Background(), "Close", false); err != nil {
		return err
	}
	defer m.release()
	return nil
}

// LastError 返回最近一次加载失败的错误；最近一次加载成功（含因摘要相同而跳过）
// 则返回 nil。它不受 Close 影响。
//
// 取消与被关闭都不算失败：它们不会写入 LastError，也不会送进 ErrorHook。
func (m *Manager[T]) LastError() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastErr
}

// Stats 是 Manager 的累计计数器。
//
// 它不是同一时刻的原子截面：在途加载已经计入 Attempts、但尚未发布时会临时出现
// Attempts 恰好比 Published 大 1 的情况，这属于正常过程而非丢失更新。
type Stats struct {
	// Attempts 是已开始的加载次数，包含 Open 的首次加载。被取消或撞上关闭的加载
	// 计入 Attempts，但不计入 Failed、Published 与 Noops。
	Attempts uint64
	// Published 是成功发布的次数，等于当前版本号。
	Published uint64
	// Failed 是失败的加载次数。取消与关闭不计入。
	Failed uint64
	// Noops 是因摘要未变化而跳过发布的次数。
	Noops uint64
}

// Stats 返回当前计数器快照。
func (m *Manager[T]) Stats() Stats {
	if m == nil {
		return Stats{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return Stats{
		Attempts:  m.attempts,
		Published: uint64(m.Get().Version()),
		Failed:    m.failed,
		Noops:     m.noops,
	}
}

// checkInitialized 拒绝零值 Manager：零值没有 token，任何写入都会永久阻塞。
func (m *Manager[T]) checkInitialized(op string) error {
	if m == nil || m.token == nil {
		return &Error{Op: op, Err: ErrNotInitialized}
	}
	return nil
}

func (m *Manager[T]) errAt(op string, cur Snapshot[T], attempt uint64, err error) *Error {
	return &Error{
		Op:      op,
		Name:    m.set.name,
		Source:  m.describeSource(),
		Version: cur.hdr.version,
		Attempt: attempt,
		Err:     err,
	}
}

func (m *Manager[T]) describeSource() string {
	if m == nil || m.set.source == nil {
		return ""
	}
	return m.set.source.Describe()
}
