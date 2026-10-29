package confwatch_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/brianbrian666/confwatch"
	jsoncodec "github.com/brianbrian666/confwatch/decode/json"
)

// seqSource 每次 Load 都返回内容不同的配置，并把调用序号写进配置本身。
type seqSource struct{ n atomic.Uint64 }

func (s *seqSource) Load(ctx context.Context) (confwatch.Payload, error) {
	if err := ctx.Err(); err != nil {
		return confwatch.Payload{}, err
	}
	n := s.n.Add(1)
	return confwatch.Payload{
		Data:   fmt.Appendf(nil, `{"seq":%d}`, n),
		Format: confwatch.FormatJSON,
	}, nil
}

func (s *seqSource) Describe() string { return "test:seq" }

// TestConcurrentReloadVersionIntegrity 验证并发 Reload 的两个核心语义：
//   - 每次发布拿到唯一版本号，没有重复、没有覆盖；
//   - 版本号与配置内容严格配对（第 N 次发布的内容就是第 N 次读取到的内容）。
//
// 如果版本号在临界区外分配，或者两次发布相互覆盖，配对断言必然失败。
func TestConcurrentReloadVersionIntegrity(t *testing.T) {
	src := &seqSource{}
	m, err := confwatch.Open[testConfig](context.Background(),
		confwatch.WithSource(src),
		confwatch.WithDecoder(confwatch.FormatJSON, jsoncodec.New()),
	)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = m.Close() }()

	const goroutines, perGoroutine = 8, 25
	total := confwatch.Version(goroutines*perGoroutine + 1) // 首次加载占用版本 1

	type record struct {
		version confwatch.Version
		seq     uint64
	}
	var (
		mu      sync.Mutex
		records []record
	)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				snap, err := m.Reload(context.Background())
				if err != nil {
					t.Errorf("Reload: %v", err)
					return
				}
				mu.Lock()
				records = append(records, record{version: snap.Version(), seq: snap.Value().Seq})
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if t.Failed() {
		return
	}

	if got := len(records); got != goroutines*perGoroutine {
		t.Fatalf("records = %d, want %d", got, goroutines*perGoroutine)
	}

	seen := make(map[confwatch.Version]bool, len(records))
	for _, r := range records {
		if seen[r.version] {
			t.Fatalf("duplicate version %d", r.version)
		}
		seen[r.version] = true
		if got := uint64(r.version); got != r.seq {
			t.Fatalf("version %d paired with seq %d: snapshot content does not match its version", r.version, r.seq)
		}
	}
	for v := confwatch.Version(2); v <= total; v++ {
		if !seen[v] {
			t.Fatalf("version %d is missing", v)
		}
	}
	if got := m.Get().Version(); got != total {
		t.Fatalf("final version = %d, want %d", got, total)
	}
	if got, want := m.Stats().Attempts, uint64(total); got != want {
		t.Fatalf("attempts = %d, want %d", got, want)
	}
}

// pairConfig 的两个字段在任何一份有效配置中都必须相等。
// 撕裂读（读到混合了新旧值的配置）会让这个不变量被破坏。
type pairConfig struct {
	A int64 `json:"a"`
	B int64 `json:"b"`
}

type pairSource struct{ n atomic.Int64 }

func (s *pairSource) Load(ctx context.Context) (confwatch.Payload, error) {
	if err := ctx.Err(); err != nil {
		return confwatch.Payload{}, err
	}
	n := s.n.Add(1)
	return confwatch.Payload{
		Data:   fmt.Appendf(nil, `{"a":%d,"b":%d}`, n, n),
		Format: confwatch.FormatJSON,
	}, nil
}

func (s *pairSource) Describe() string { return "test:pair" }

// TestConcurrentSnapshotIntegrity 在持续发布的同时高频读取，断言读者永远看到
// 一份内部一致的配置。这是"原子发布 + 对象不可变"的可执行证据。
func TestConcurrentSnapshotIntegrity(t *testing.T) {
	src := &pairSource{}
	m, err := confwatch.Open[pairConfig](context.Background(),
		confwatch.WithSource(src),
		confwatch.WithDecoder(confwatch.FormatJSON, jsoncodec.New()),
	)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = m.Close() }()

	stop := make(chan struct{})
	var readers sync.WaitGroup
	for i := 0; i < 8; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				snap := m.Get()
				if !snap.Valid() {
					t.Error("snapshot must stay valid")
					return
				}
				cfg := snap.Value()
				if cfg.A != cfg.B {
					t.Errorf("torn read: a=%d b=%d", cfg.A, cfg.B)
					return
				}
			}
		}()
	}

	const reloads = 200
	for i := 0; i < reloads; i++ {
		if _, err := m.Reload(context.Background()); err != nil {
			t.Fatalf("reload %d: %v", i, err)
		}
	}
	close(stop)
	readers.Wait()

	final := m.Get()
	if got, want := final.Version(), confwatch.Version(reloads+1); got != want {
		t.Fatalf("final version = %d, want %d", got, want)
	}
	if got, want := final.Value().A, int64(reloads+1); got != want {
		t.Fatalf("final a = %d, want %d", got, want)
	}
}

// TestOldSnapshotsAreNotReused 验证每次发布都产生独立的对象图：
// 早期快照在经历多次更新后，内容与它们各自的版本仍然一致。
func TestOldSnapshotsAreNotReused(t *testing.T) {
	src := newSource(`{"seq":1,"addr":":1","tags":{"k":"v1"},"list":[1]}`)
	m := openManager(t, src)

	var held []confwatch.Snapshot[testConfig]
	held = append(held, m.Get())

	for i := 2; i <= 6; i++ {
		src.setData(fmt.Sprintf(`{"seq":%d,"addr":":%d","tags":{"k":"v%d"},"list":[%d]}`, i, i, i, i))
		snap, err := m.Reload(context.Background())
		if err != nil {
			t.Fatalf("reload %d: %v", i, err)
		}
		held = append(held, snap)
	}

	for i, snap := range held {
		wantSeq := uint64(i + 1)
		cfg := snap.Value()
		if cfg.Seq != wantSeq {
			t.Fatalf("snapshot %d: seq = %d, want %d", i, cfg.Seq, wantSeq)
		}
		if got, want := cfg.Tags["k"], fmt.Sprintf("v%d", wantSeq); got != want {
			t.Fatalf("snapshot %d: tag = %q, want %q", i, got, want)
		}
		if len(cfg.List) != 1 || cfg.List[0] != int(wantSeq) {
			t.Fatalf("snapshot %d: list = %v, want [%d]", i, cfg.List, wantSeq)
		}
	}
}
