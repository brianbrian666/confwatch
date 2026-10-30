package confwatch_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/brianbrian666/confwatch"
	jsoncodec "github.com/brianbrian666/confwatch/decode/json"
)

func TestOpenPublishesInitialSnapshot(t *testing.T) {
	src := newSource(`{"seq":1,"addr":":8080","tags":{"env":"test"},"list":[1,2]}`)
	m := openManager(t, src)

	snap := m.Get()
	if !snap.Valid() {
		t.Fatal("snapshot must be valid after Open")
	}
	if got, want := snap.Version(), confwatch.Version(1); got != want {
		t.Fatalf("version = %d, want %d", got, want)
	}
	if got, want := snap.Reason().Kind, confwatch.ReasonInitial; got != want {
		t.Fatalf("reason = %v, want %v", got, want)
	}
	if snap.Digest() == "" {
		t.Fatal("digest must be set")
	}
	if snap.LoadedAt().IsZero() {
		t.Fatal("loadedAt must be set")
	}

	cfg := snap.Value()
	if cfg.Addr != ":8080" {
		t.Fatalf("addr = %q, want %q", cfg.Addr, ":8080")
	}
	if got, want := cfg.Tags, map[string]string{"env": "test"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("tags = %v, want %v", got, want)
	}
	if got, want := cfg.List, []int{1, 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("list = %v, want %v", got, want)
	}

	if got, want := m.Stats(), (confwatch.Stats{Attempts: 1, Published: 1}); got != want {
		t.Fatalf("stats = %+v, want %+v", got, want)
	}
	if got := src.loadCount(); got != 1 {
		t.Fatalf("source loads = %d, want 1", got)
	}
}

func TestOpenRejectsNilContext(t *testing.T) {
	m, err := confwatch.Open[testConfig](nil, //nolint:staticcheck // 故意传 nil 以验证错误处理
		confwatch.WithSource(newSource(`{}`)),
		confwatch.WithDecoder(confwatch.FormatJSON, jsoncodec.New()),
	)
	if m != nil {
		t.Fatal("expected nil manager")
	}
	if !errors.Is(err, confwatch.ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
}

func TestOpenRequiresSourceAndDecoder(t *testing.T) {
	tests := []struct {
		name string
		opts []confwatch.Option
	}{
		{name: "no options"},
		{name: "no decoder", opts: []confwatch.Option{
			confwatch.WithSource(newSource(`{}`)),
		}},
		{name: "no source", opts: []confwatch.Option{
			confwatch.WithDecoder(confwatch.FormatJSON, jsoncodec.New()),
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, err := confwatch.Open[testConfig](context.Background(), tc.opts...)
			if m != nil {
				t.Fatal("expected nil manager")
			}
			if !errors.Is(err, confwatch.ErrNotConfigured) {
				t.Fatalf("err = %v, want ErrNotConfigured", err)
			}
		})
	}
}

func TestOpenFailsOnInvalidPayload(t *testing.T) {
	tests := []struct {
		name string
		data string
		want error
	}{
		{name: "syntax error", data: `{"addr":`, want: confwatch.ErrDecode},
		{name: "null document", data: `null`, want: confwatch.ErrDecode},
		{name: "type mismatch", data: `{"seq":"abc"}`, want: confwatch.ErrDecode},
		{name: "trailing data", data: `{"addr":":1"}junk`, want: confwatch.ErrDecode},
		{name: "empty payload", data: ``, want: confwatch.ErrDecode},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, err := confwatch.Open[testConfig](context.Background(),
				confwatch.WithSource(newSource(tc.data)),
				confwatch.WithDecoder(confwatch.FormatJSON, jsoncodec.New()),
			)
			if m != nil {
				t.Fatal("expected nil manager")
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestOpenFailsOnSourceError(t *testing.T) {
	src := newSource(`{}`)
	src.setError(errors.New("disk on fire"))

	_, err := confwatch.Open[testConfig](context.Background(),
		confwatch.WithSource(src),
		confwatch.WithDecoder(confwatch.FormatJSON, jsoncodec.New()),
	)
	if !errors.Is(err, confwatch.ErrSource) {
		t.Fatalf("err = %v, want ErrSource", err)
	}
	for _, want := range []string{"disk on fire", "source=test:mutable", "confwatch.Open"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err %q must contain %q", err, want)
		}
	}
}

func TestOpenFailsOnUnknownFormat(t *testing.T) {
	src := newSource(`{"addr":":1"}`)
	src.setFormat(confwatch.Format(99))

	_, err := confwatch.Open[testConfig](context.Background(),
		confwatch.WithSource(src),
		confwatch.WithDecoder(confwatch.FormatJSON, jsoncodec.New()),
	)
	if !errors.Is(err, confwatch.ErrNoDecoder) {
		t.Fatalf("err = %v, want ErrNoDecoder", err)
	}
}

func TestReloadPublishesNewVersion(t *testing.T) {
	src := newSource(`{"seq":1,"addr":":8080"}`)
	m := openManager(t, src)
	first := m.Get()

	src.setData(`{"seq":2,"addr":":9090"}`)
	snap, err := m.Reload(context.Background())
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got, want := snap.Version(), confwatch.Version(2); got != want {
		t.Fatalf("version = %d, want %d", got, want)
	}
	if got, want := snap.Reason().Kind, confwatch.ReasonManualReload; got != want {
		t.Fatalf("reason = %v, want %v", got, want)
	}
	if got := snap.Value().Addr; got != ":9090" {
		t.Fatalf("addr = %q, want %q", got, ":9090")
	}
	if got := m.Get().Version(); got != 2 {
		t.Fatalf("Get().Version = %d, want 2", got)
	}

	// 之前取得的快照必须保持原样。
	if first.Version() != 1 || first.Value().Addr != ":8080" {
		t.Fatalf("old snapshot was mutated: %+v", first.Value())
	}
	if first.Digest() == snap.Digest() {
		t.Fatal("digest must change after content change")
	}
	if got, want := m.Stats(), (confwatch.Stats{Attempts: 2, Published: 2}); got != want {
		t.Fatalf("stats = %+v, want %+v", got, want)
	}
}

func TestReloadFailureKeepsCurrentSnapshot(t *testing.T) {
	t.Run("decode error", func(t *testing.T) {
		src := newSource(`{"addr":":8080"}`)
		m := openManager(t, src)
		before := m.Get()

		src.setData(`{"addr":`)
		_, err := m.Reload(context.Background())
		if !errors.Is(err, confwatch.ErrDecode) {
			t.Fatalf("err = %v, want ErrDecode", err)
		}
		assertSnapshotUnchanged(t, before, m.Get())
		if m.LastError() == nil {
			t.Fatal("LastError must be set after a failed reload")
		}
		if got := m.Stats().Failed; got != 1 {
			t.Fatalf("failed = %d, want 1", got)
		}
	})

	t.Run("source error", func(t *testing.T) {
		src := newSource(`{"addr":":8080"}`)
		m := openManager(t, src)
		before := m.Get()

		src.setError(errors.New("permission denied"))
		_, err := m.Reload(context.Background())
		if !errors.Is(err, confwatch.ErrSource) {
			t.Fatalf("err = %v, want ErrSource", err)
		}
		assertSnapshotUnchanged(t, before, m.Get())
	})

	t.Run("validation error", func(t *testing.T) {
		src := newSource(`{"addr":":8080"}`)
		m := openManager(t, src,
			confwatch.WithValidator(confwatch.ValidatorFunc[testConfig](func(_ context.Context, c *testConfig) error {
				if c.Addr == "" {
					return errors.New("addr is required")
				}
				return nil
			})),
		)
		before := m.Get()

		src.setData(`{"addr":""}`)
		_, err := m.Reload(context.Background())
		if !errors.Is(err, confwatch.ErrValidation) {
			t.Fatalf("err = %v, want ErrValidation", err)
		}
		assertSnapshotUnchanged(t, before, m.Get())
	})
}

func TestReloadAggregatesAllValidationErrors(t *testing.T) {
	src := newSource(`{"addr":":8080","seq":1}`)
	m := openManager(t, src,
		confwatch.WithValidator(confwatch.ValidatorFunc[testConfig](func(_ context.Context, c *testConfig) error {
			if c.Addr == "" {
				return errors.New("addr is required")
			}
			return nil
		})),
		confwatch.WithValidator(confwatch.ValidatorFunc[testConfig](func(_ context.Context, c *testConfig) error {
			if c.Seq == 0 {
				return errors.New("seq must be positive")
			}
			return nil
		})),
	)

	src.setData(`{"addr":"","seq":0}`)
	_, err := m.Reload(context.Background())
	if !errors.Is(err, confwatch.ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
	// 校验器不短路：两个失败原因都要出现。
	for _, want := range []string{"addr is required", "seq must be positive"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err %q must contain %q", err, want)
		}
	}
	if got := m.Get().Version(); got != 1 {
		t.Fatalf("version = %d, want 1 (failure must not publish)", got)
	}
}

func TestReloadNoopSkipsPublish(t *testing.T) {
	t.Run("enabled by default", func(t *testing.T) {
		src := newSource(`{"addr":":8080"}`)
		m := openManager(t, src)

		snap, err := m.Reload(context.Background())
		if err != nil {
			t.Fatalf("Reload: %v", err)
		}
		if got := snap.Version(); got != 1 {
			t.Fatalf("version = %d, want 1 (identical content must not publish)", got)
		}
		if got, want := m.Stats(), (confwatch.Stats{Attempts: 2, Published: 1, Noops: 1}); got != want {
			t.Fatalf("stats = %+v, want %+v", got, want)
		}
	})

	t.Run("disabled", func(t *testing.T) {
		src := newSource(`{"addr":":8080"}`)
		m := openManager(t, src, confwatch.WithNoopSkip(false))

		snap, err := m.Reload(context.Background())
		if err != nil {
			t.Fatalf("Reload: %v", err)
		}
		if got := snap.Version(); got != 2 {
			t.Fatalf("version = %d, want 2", got)
		}
	})
}

func TestReloadUsesPayloadDigestWhenProvided(t *testing.T) {
	src := &fixedSource{
		payload: confwatch.Payload{Data: []byte(`{"addr":":8080"}`), Format: confwatch.FormatJSON, Digest: "custom"},
	}
	m := openManager(t, src)
	if got := m.Get().Digest(); got != "custom" {
		t.Fatalf("digest = %q, want %q", got, "custom")
	}
}

func TestTryReloadWhileReloadInFlight(t *testing.T) {
	src := newGateSource(`{"addr":":8080"}`)
	m := openManager(t, src)

	// 内容必须变化，否则会因摘要相同而被跳过，不产生发布。
	src.setData(`{"addr":":9090"}`)

	entered := make(chan struct{}, 1)
	gate := make(chan struct{})
	src.setGate(gate, entered)

	reloadErr := make(chan error, 1)
	go func() {
		_, err := m.Reload(context.Background())
		reloadErr <- err
	}()

	<-entered // 此时 Reload 已持有写者令牌
	if _, err := m.TryReload(context.Background()); !errors.Is(err, confwatch.ErrReloadInFlight) {
		t.Fatalf("err = %v, want ErrReloadInFlight", err)
	}

	close(gate)
	if err := <-reloadErr; err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := m.Get().Version(); got != 2 {
		t.Fatalf("version = %d, want 2", got)
	}
}

func TestReloadCanceled(t *testing.T) {
	src := newGateSource(`{"addr":":8080"}`)
	m := openManager(t, src)
	before := m.Get()

	src.setData(`{"addr":":9090"}`)
	entered := make(chan struct{}, 1)
	gate := make(chan struct{})
	src.setGate(gate, entered)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-entered
		cancel()
	}()

	_, err := m.Reload(ctx)
	if !errors.Is(err, confwatch.ErrCanceled) {
		t.Fatalf("err = %v, want ErrCanceled", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want it to wrap context.Canceled", err)
	}
	assertSnapshotUnchanged(t, before, m.Get())
}

func TestCloseIsIdempotentAndBlocksReload(t *testing.T) {
	m := openManager(t, newSource(`{"addr":":8080"}`))

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	// 关闭后仍然可以安全读取最后一份有效快照。
	if snap := m.Get(); !snap.Valid() || snap.Version() != 1 {
		t.Fatalf("snapshot after Close = %+v, want valid version 1", snap)
	}

	if _, err := m.Reload(context.Background()); !errors.Is(err, confwatch.ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
	if _, err := m.TryReload(context.Background()); !errors.Is(err, confwatch.ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
}

func TestZeroValueManagerIsRejected(t *testing.T) {
	var m confwatch.Manager[testConfig]

	if _, err := m.Reload(context.Background()); !errors.Is(err, confwatch.ErrNotInitialized) {
		t.Fatalf("err = %v, want ErrNotInitialized", err)
	}
	if _, err := m.TryReload(context.Background()); !errors.Is(err, confwatch.ErrNotInitialized) {
		t.Fatalf("err = %v, want ErrNotInitialized", err)
	}
	if err := m.Close(); !errors.Is(err, confwatch.ErrNotInitialized) {
		t.Fatalf("err = %v, want ErrNotInitialized", err)
	}
	// Get 不 panic，也不返回有效快照。
	if snap := m.Get(); snap.Valid() {
		t.Fatalf("zero value manager returned a valid snapshot: %+v", snap)
	}
	if m.LastError() != nil {
		t.Fatalf("LastError = %v, want nil", m.LastError())
	}
}

func TestWithDefaultsRunsBeforeValidation(t *testing.T) {
	m := openManager(t, newSource(`{"addr":""}`),
		confwatch.WithDefaults(func(c *testConfig) {
			if c.Addr == "" {
				c.Addr = ":8080"
			}
		}),
		confwatch.WithValidator(confwatch.ValidatorFunc[testConfig](func(_ context.Context, c *testConfig) error {
			if c.Addr == "" {
				return errors.New("addr is required")
			}
			return nil
		})),
	)

	if got := m.Get().Value().Addr; got != ":8080" {
		t.Fatalf("addr = %q, want default %q", got, ":8080")
	}
}

func TestValidatorTypeMismatchIsRejected(t *testing.T) {
	type otherConfig struct {
		X int `json:"x"`
	}

	_, err := confwatch.Open[testConfig](context.Background(),
		confwatch.WithSource(newSource(`{"addr":":1"}`)),
		confwatch.WithDecoder(confwatch.FormatJSON, jsoncodec.New()),
		confwatch.WithValidator(confwatch.ValidatorFunc[otherConfig](func(context.Context, *otherConfig) error {
			return nil
		})),
	)
	if !errors.Is(err, confwatch.ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
}

func TestErrorCarriesDiagnostics(t *testing.T) {
	src := newSource(`{"addr":":1"}`)
	m := openManager(t, src, confwatch.WithName("svc"))
	src.setData(`{"addr":`)

	_, err := m.Reload(context.Background())

	var e *confwatch.Error
	if !errors.As(err, &e) {
		t.Fatalf("err = %v, want *confwatch.Error", err)
	}
	if e.Op != "Reload" || e.Name != "svc" || e.Source != "test:mutable" {
		t.Fatalf("unexpected error context: %+v", e)
	}
	if e.Version != 1 {
		t.Fatalf("version = %d, want 1", e.Version)
	}
	if e.Attempt != 2 {
		t.Fatalf("attempt = %d, want 2", e.Attempt)
	}

	msg := err.Error()
	for _, want := range []string{"confwatch.Reload", "name=svc", "source=test:mutable", "version=1", "attempt=2", "decode failed"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error message %q must contain %q", msg, want)
		}
	}
}

func assertSnapshotUnchanged[T any](t *testing.T, before, after confwatch.Snapshot[T]) {
	t.Helper()
	if after.Version() != before.Version() {
		t.Fatalf("version changed: %d -> %d", before.Version(), after.Version())
	}
	if after.Digest() != before.Digest() {
		t.Fatalf("digest changed: %q -> %q", before.Digest(), after.Digest())
	}
	if !after.LoadedAt().Equal(before.LoadedAt()) {
		t.Fatalf("loadedAt changed: %v -> %v", before.LoadedAt(), after.LoadedAt())
	}
	if !reflect.DeepEqual(after.Value(), before.Value()) {
		t.Fatalf("value changed: %+v -> %+v", before.Value(), after.Value())
	}
}

// fixedSource 返回固定的载荷，用于验证 Payload.Digest 被尊重。
type fixedSource struct {
	payload confwatch.Payload
}

func (s *fixedSource) Load(context.Context) (confwatch.Payload, error) { return s.payload, nil }
func (s *fixedSource) Describe() string                                { return "test:fixed" }

// TestCanceledReloadIsNotAFailure 钉住“取消/关闭不是失败”的口径。
//
// 优雅关闭时被 ctx 取消的那次监听重载，曾经会把 Failed+1 并把 LastError 改成
// ErrCanceled，导致基于 Failed>0 或 LastError!=nil 的告警在每次重启时误报。
func TestCanceledReloadIsNotAFailure(t *testing.T) {
	src := newGateSource(`{"addr":":8080"}`)
	m := openManager(t, src)

	gate, entered := make(chan struct{}), make(chan struct{}, 1)
	src.setGate(gate, entered)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := m.Reload(ctx)
		done <- err
	}()

	// 确定性地等到 Load 真的进入阻塞，而不是 sleep。
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Reload 未进入 Load")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, confwatch.ErrCanceled) {
			t.Fatalf("err = %v, want ErrCanceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("取消后 Reload 未返回")
	}

	if got, want := m.Stats(), (confwatch.Stats{Attempts: 2, Published: 1}); got != want {
		t.Fatalf("stats = %+v, want %+v", got, want)
	}
	if err := m.LastError(); err != nil {
		t.Fatalf("LastError = %v, want nil（取消不算失败）", err)
	}
	if got := m.Get().Version(); got != 1 {
		t.Fatalf("version = %d, want 1", got)
	}

	// 关闭后的 Reload 同理：返回 ErrClosed，但不留假故障。
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := m.Reload(context.Background()); !errors.Is(err, confwatch.ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
	if got, want := m.Stats().Failed, uint64(0); got != want {
		t.Fatalf("Failed = %d, want %d（取消与关闭都不计入）", got, want)
	}
	if err := m.LastError(); err != nil {
		t.Fatalf("LastError = %v, want nil", err)
	}
}

// TestNoopReloadReturnsCurrentSnapshot 记录 noop 的返回语义：内容未变时返回的是
// 当前快照（版本、发布原因都保持上一次真正发布时的值），调用方要判断“是否发布了
// 新版本”只能比较调用前后的 Version()。
func TestNoopReloadReturnsCurrentSnapshot(t *testing.T) {
	m := openManager(t, newSource(`{"addr":":8080"}`))

	snap, err := m.Reload(context.Background())
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := snap.Version(); got != 1 {
		t.Fatalf("version = %d, want 1", got)
	}
	if got := snap.Reason().Kind; got != confwatch.ReasonInitial {
		t.Fatalf("reason = %v, want initial（noop 不刷新发布原因）", got)
	}
	if got := m.Stats().Noops; got != 1 {
		t.Fatalf("Noops = %d, want 1", got)
	}
}

// panickingValidator 模拟用户代码里的编程错误（例如向 nil map 写入）。
type panickingValidator struct{}

func (panickingValidator) Validate(context.Context, *testConfig) error { panic("validator boom") }

// TestValidatorPanicPropagates 钉住 panic 语义：库只对监听器与 ErrorHook 的 panic
// 兜底（不 recover 会让 Watch 永久阻塞或污染监听循环），配置源/解码器/校验器的
// panic 属于编程错误，必须按 Go 的默认行为向上传播，不能被静默吞掉。
func TestValidatorPanicPropagates(t *testing.T) {
	defer func() {
		if p := recover(); p == nil {
			t.Fatal("校验器 panic 必须向上传播，不能被库吞掉")
		}
	}()

	openWith[testConfig](t, newSource(`{"addr":":8080"}`), confwatch.WithValidator(panickingValidator{}))
	t.Fatal("不应执行到这里：panic 必须传播")
}
