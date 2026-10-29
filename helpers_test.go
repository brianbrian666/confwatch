package confwatch_test

import (
	"context"
	"maps"
	"slices"
	"sync"
	"testing"

	"github.com/brianbrian666/confwatch"
	jsoncodec "github.com/brianbrian666/confwatch/decode/json"
)

// testConfig 故意同时包含值类型与引用类型字段：
//   - Addr/Seq 是值类型，修改 Value 返回的拷贝不影响快照；
//   - Tags/List 是引用类型，是"API 无法保证深不可变"的边界所在。
type testConfig struct {
	Seq  uint64            `json:"seq"`
	Addr string            `json:"addr"`
	Tags map[string]string `json:"tags"`
	List []int             `json:"list"`
}

// copyTestConfig 是 testConfig 的深拷贝，用作 WithCopyFunc 的实参。
func copyTestConfig(in testConfig) testConfig {
	out := in
	out.Tags = maps.Clone(in.Tags)
	out.List = slices.Clone(in.List)
	return out
}

// mutableSource 是内容可变、线程安全的配置源。每次 Load 都返回新分配的字节切片。
type mutableSource struct {
	mu     sync.Mutex
	data   string
	format confwatch.Format
	err    error
	loads  int
}

func newSource(data string) *mutableSource {
	return &mutableSource{data: data, format: confwatch.FormatJSON}
}

func (s *mutableSource) Load(ctx context.Context) (confwatch.Payload, error) {
	if err := ctx.Err(); err != nil {
		return confwatch.Payload{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loads++
	if s.err != nil {
		return confwatch.Payload{}, s.err
	}
	return confwatch.Payload{Data: []byte(s.data), Format: s.format}, nil
}

func (s *mutableSource) Describe() string { return "test:mutable" }

func (s *mutableSource) setData(data string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = data
}

func (s *mutableSource) setError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *mutableSource) setFormat(format confwatch.Format) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.format = format
}

func (s *mutableSource) loadCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loads
}

// gateSource 在 gate 非 nil 时让 Load 阻塞，直到 gate 被关闭或 ctx 取消。
// 用它构造确定性的并发测试，避免依赖 time.Sleep 做同步。
type gateSource struct {
	mu      sync.Mutex
	data    string
	gate    chan struct{}
	entered chan struct{}
}

func newGateSource(data string) *gateSource { return &gateSource{data: data} }

func (s *gateSource) Load(ctx context.Context) (confwatch.Payload, error) {
	s.mu.Lock()
	gate, entered, data := s.gate, s.entered, s.data
	s.mu.Unlock()

	if gate != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-gate:
		case <-ctx.Done():
			return confwatch.Payload{}, ctx.Err()
		}
	}
	return confwatch.Payload{Data: []byte(data), Format: confwatch.FormatJSON}, nil
}

func (s *gateSource) Describe() string { return "test:gate" }

func (s *gateSource) setGate(gate, entered chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gate, s.entered = gate, entered
}

func (s *gateSource) setData(data string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = data
}

func openManager(t *testing.T, src confwatch.Source, opts ...confwatch.Option) *confwatch.Manager[testConfig] {
	t.Helper()
	m := openWith[testConfig](t, src, opts...)
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func openWith[T any](t *testing.T, src confwatch.Source, opts ...confwatch.Option) *confwatch.Manager[T] {
	t.Helper()
	all := append([]confwatch.Option{
		confwatch.WithSource(src),
		confwatch.WithDecoder(confwatch.FormatJSON, jsoncodec.New()),
	}, opts...)
	m, err := confwatch.Open[T](context.Background(), all...)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return m
}
