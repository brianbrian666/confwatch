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
	if _, err := m.reload(ctx, "Open", Reason{Kind: ReasonInitial}, false); err != nil {
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
		m.mu.Lock()
		m.failed++
		m.lastErr = err
		m.mu.Unlock()
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

// Close 标记 Manager 已关闭。它会等待正在进行的发布结束，返回后不会再有任何新
// 快照被发布。Close 是幂等的。
//
// 关闭后 Get 仍返回最后一份有效快照（只读安全），但 Reload/TryReload 返回
// ErrClosed。注意：若某个 Reload 的配置源忽略 ctx 并长时间阻塞，Close 会一直
// 等待它结束。
func (m *Manager[T]) Close() error {
	if err := m.checkInitialized("Close"); err != nil {
		return err
	}
	if err := m.acquire(context.Background(), "Close", false); err != nil {
		return err
	}
	defer m.release()

	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

// LastError 返回最近一次加载失败的错误；最近一次加载成功（含因摘要相同而跳过）
// 则返回 nil。它不受 Close 影响。
func (m *Manager[T]) LastError() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastErr
}

// Stats 是 Manager 的累计计数器。
type Stats struct {
	// Attempts 是已开始的加载次数，包含 Open 的首次加载。
	Attempts uint64
	// Published 是成功发布的次数，等于当前版本号。
	Published uint64
	// Failed 是失败的加载次数。
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
