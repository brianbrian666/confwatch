package confwatch_test

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/brianbrian666/confwatch"
	jsoncodec "github.com/brianbrian666/confwatch/decode/json"
)

// 这些包级变量存在的唯一目的是防止编译器把被测调用优化掉。
var (
	sinkSnapshot confwatch.Snapshot[testConfig]
	sinkVersion  confwatch.Version
	sinkAddr     string
	sinkStats    confwatch.Stats
)

func mustManager(tb testing.TB, src confwatch.Source, opts ...confwatch.Option) *confwatch.Manager[testConfig] {
	tb.Helper()
	all := make([]confwatch.Option, 0, len(opts)+2)
	all = append(all, confwatch.WithSource(src), confwatch.WithDecoder(confwatch.FormatJSON, jsoncodec.New()))
	all = append(all, opts...)
	m, err := confwatch.Open[testConfig](context.Background(), all...)
	if err != nil {
		tb.Fatalf("Open: %v", err)
	}
	tb.Cleanup(func() {
		if err := m.Close(); err != nil {
			tb.Errorf("Close: %v", err)
		}
	})
	return m
}

// TestGetDoesNotAllocate 把文档里「读路径不分配」的承诺变成可执行断言。
// 写者路径允许分配（解码、默认值、校验都需要）；读路径分配意味着每次取
// 配置都在给 GC 施压，这在每个请求都读一次配置的服务里是真实的成本。
func TestGetDoesNotAllocate(t *testing.T) {
	m := mustManager(t, newSource(`{"seq":1,"addr":":8080","tags":{"a":"b"},"list":[1,2,3]}`))

	// 预热，排除首次调用可能带来的惰性初始化。
	sinkAddr = m.Get().Value().Addr
	sinkVersion = m.Get().Version()

	if got := testing.AllocsPerRun(1000, func() { sinkAddr = m.Get().Value().Addr }); got != 0 {
		t.Fatalf("Get().Value().Addr 每次分配 %.2f 次, want 0", got)
	}
	if got := testing.AllocsPerRun(1000, func() { sinkSnapshot = m.Get() }); got != 0 {
		t.Fatalf("Get() 每次分配 %.2f 次, want 0", got)
	}
	if got := testing.AllocsPerRun(1000, func() { sinkStats = m.Stats() }); got != 0 {
		t.Fatalf("Stats() 每次分配 %.2f 次, want 0", got)
	}
}

// BenchmarkGet 测量无锁读路径（原子加载 + 结构体拷贝）的成本。
func BenchmarkGet(b *testing.B) {
	m := mustManager(b, newSource(`{"seq":1,"addr":":8080","tags":{"a":"b"},"list":[1,2,3]}`))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sinkSnapshot = m.Get()
	}
}

// BenchmarkGetValue 把快照里的配置拷贝出来，与 BenchmarkGet 的差值就是
// 结构体拷贝的成本。
func BenchmarkGetValue(b *testing.B) {
	m := mustManager(b, newSource(`{"seq":1,"addr":":8080","tags":{"a":"b"},"list":[1,2,3]}`))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sinkAddr = m.Get().Value().Addr
	}
}

// BenchmarkGetParallel 在多个 P 上并发读，验证读路径没有隐藏的争用。
func BenchmarkGetParallel(b *testing.B) {
	m := mustManager(b, newSource(`{"seq":1,"addr":":8080","tags":{"a":"b"},"list":[1,2,3]}`))
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		var addr string
		n := 0
		for pb.Next() {
			addr = m.Get().Value().Addr
			n++
		}
		// RunParallel 可能给某个 goroutine 分配 0 次迭代，只有真的跑过才校验。
		if n > 0 && addr == "" {
			b.Error("配置地址为空")
		}
	})
}

// benchSeqSource 产生与 BenchmarkReloadNoop 相同形状的文档，只是序号递增，
// 保证两个基准测量的是同一份文档的“不发布”与“发布”成本，可以直接相减。
type benchSeqSource struct{ n atomic.Uint64 }

func (s *benchSeqSource) Load(ctx context.Context) (confwatch.Payload, error) {
	if err := ctx.Err(); err != nil {
		return confwatch.Payload{}, err
	}
	n := s.n.Add(1)
	data := append([]byte(`{"seq":`), strconv.AppendUint(nil, n, 10)...)
	data = append(data, `,"addr":":8080","tags":{"a":"b"},"list":[1,2,3]}`...)
	return confwatch.Payload{Data: data, Format: confwatch.FormatJSON}, nil
}

func (s *benchSeqSource) Describe() string { return "bench:seq" }

// BenchmarkReloadNoop 测量内容未变化时的完整流水线：它仍然要读取、解码、
// 校验，只是最后因摘要相同而不发布。这是监听器被高频触发时的稳态成本。
func BenchmarkReloadNoop(b *testing.B) {
	m := mustManager(b, newSource(`{"seq":1,"addr":":8080","tags":{"a":"b"},"list":[1,2,3]}`))
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := m.Reload(ctx); err != nil {
			b.Fatalf("Reload: %v", err)
		}
	}
}

// BenchmarkReloadPublish 测量内容每次都变化时的成本，也就是真实更新的延迟下界
// （不含文件系统与监听器的开销）。文档形状与 BenchmarkReloadNoop 完全相同。
func BenchmarkReloadPublish(b *testing.B) {
	m := mustManager(b, &benchSeqSource{})
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := m.Reload(ctx); err != nil {
			b.Fatalf("Reload: %v", err)
		}
	}
}

// BenchmarkReloadParallel 让多个 goroutine 同时 Reload。写者令牌会把它们串行化，
// 这里量的是排队与令牌交接的成本，而不是并行吞吐。
func BenchmarkReloadParallel(b *testing.B) {
	m := mustManager(b, &pairSource{})
	ctx := context.Background()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := m.Reload(ctx); err != nil {
				b.Error(err)
				return
			}
		}
	})
}
