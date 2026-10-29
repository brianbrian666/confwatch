package confwatch_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/brianbrian666/confwatch"
	yamlcodec "github.com/brianbrian666/confwatch/decode/yaml"
	"github.com/brianbrian666/confwatch/source/file"
	fswatcher "github.com/brianbrian666/confwatch/watcher/fsnotify"
	"github.com/brianbrian666/confwatch/watcher/poll"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// writeFileAtomic 模拟编辑器的"临时文件 + rename"保存方式。
func writeFileAtomic(t *testing.T, path, body string) {
	t.Helper()
	tmp := filepath.Join(filepath.Dir(path), ".tmp-"+filepath.Base(path))
	writeFile(t, tmp, body)
	if err := os.Rename(tmp, path); err != nil {
		t.Fatalf("rename %s: %v", tmp, err)
	}
}

func openWatched[T any](t *testing.T, src confwatch.Source, opts ...confwatch.Option) *confwatch.Manager[T] {
	t.Helper()
	m := openWith[T](t, src, opts...)
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// TestWatchFileEndToEndWithFsnotify 用真实文件与真实内核事件验证：
// 内容变化、原子替换、删除、重建。
func TestWatchFileEndToEndWithFsnotify(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.json")
	writeFile(t, path, `{"addr":":8080"}`)

	m := openManager(t, file.New(path), confwatch.WithWatcher(fswatcher.New(path)))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.Watch(ctx) }()

	// 1. 直接写文件。
	writeFile(t, path, `{"addr":":9090"}`)
	waitFor(t, "版本 2", func() bool { return m.Get().Version() == 2 })
	if got := m.Get().Value().Addr; got != ":9090" {
		t.Fatalf("addr = %q, want :9090", got)
	}

	// 2. 原子替换（编辑器保存方式）。
	writeFileAtomic(t, path, `{"addr":":7070"}`)
	waitFor(t, "版本 3", func() bool { return m.Get().Version() == 3 })
	if got := m.Get().Value().Addr; got != ":7070" {
		t.Fatalf("addr = %q, want :7070", got)
	}

	// 3. 删除：旧配置继续生效，错误被记录。
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	waitFor(t, "记录读取错误", func() bool { return m.LastError() != nil })
	if !errors.Is(m.LastError(), confwatch.ErrSource) {
		t.Fatalf("LastError = %v, want ErrSource", m.LastError())
	}
	if got := m.Get().Value().Addr; got != ":7070" || m.Get().Version() != 3 {
		t.Fatalf("删除后快照不应变化: version=%v addr=%q", m.Get().Version(), got)
	}

	// 4. 重建：恢复自动更新。
	writeFile(t, path, `{"addr":":6060"}`)
	waitFor(t, "版本 4", func() bool { return m.Get().Version() == 4 })
	if got := m.Get().Value().Addr; got != ":6060" {
		t.Fatalf("addr = %q, want :6060", got)
	}
	if err := m.LastError(); err != nil {
		t.Fatalf("恢复后 LastError = %v, want nil", err)
	}

	// 5. 停止：Watch 正常返回，Close 后不再有更新。
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Watch = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Watch 未返回")
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	final := m.Get().Version()
	writeFile(t, path, `{"addr":":5050"}`)
	time.Sleep(100 * time.Millisecond)
	if got := m.Get().Version(); got != final {
		t.Fatalf("停止后仍有发布: %v -> %v", final, got)
	}
	// 已取消的 ctx 会先于关闭状态生效，所以这里用新的 ctx 验证 ErrClosed。
	if _, err := m.Reload(context.Background()); !errors.Is(err, confwatch.ErrClosed) {
		t.Fatalf("关闭后 Reload = %v, want ErrClosed", err)
	}
}

// TestWatchRapidChangesConverge 覆盖“连续多次修改”：中间可能只发布部分状态，
// 但最终快照必须等于最后写入的内容。
func TestWatchRapidChangesConverge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.json")
	writeFile(t, path, `{"addr":":1000"}`)

	m := openManager(t, file.New(path), confwatch.WithWatcher(fswatcher.New(path)))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.Watch(ctx) }()

	want := ""
	for i := 1; i <= 5; i++ {
		want = fmt.Sprintf(":100%d", i)
		writeFile(t, path, fmt.Sprintf(`{"addr":%q}`, want))
	}

	waitFor(t, "收敛到最后一次写入", func() bool { return m.Get().Value().Addr == want })
	if err := m.LastError(); err != nil {
		t.Fatalf("LastError = %v, want nil", err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Watch = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Watch 未返回")
	}
}

// yamlConfig 用来验证 YAML 文件的热更新（YAML 与 JSON 共用 json 标签）。
type yamlConfig struct {
	Addr    string `json:"addr"`
	Workers int    `json:"workers"`
}

// TestWatchFileEndToEndWithPoll 用零依赖的轮询监听器验证 YAML 文件的
// 变化、无效内容与恢复。
func TestWatchFileEndToEndWithPoll(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.yaml")
	writeFile(t, path, "addr: \":8080\"\nworkers: 2\n")

	pw := poll.New(path)
	pw.Interval = 5 * time.Millisecond

	m := openWatched[yamlConfig](t, file.New(path),
		confwatch.WithWatcher(pw),
		confwatch.WithDecoder(confwatch.FormatYAML, yamlcodec.New()),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.Watch(ctx) }()

	writeFile(t, path, "addr: \":9090\"\nworkers: 4\n")
	waitFor(t, "版本 2", func() bool { return m.Get().Version() == 2 })
	if got := m.Get().Value(); got.Addr != ":9090" || got.Workers != 4 {
		t.Fatalf("value = %+v, want addr=:9090 workers=4", got)
	}

	// 无效 YAML：保留旧配置。
	before := m.Get()
	writeFile(t, path, "addr: [unclosed\n")
	waitFor(t, "记录解析错误", func() bool { return m.LastError() != nil })
	if !errors.Is(m.LastError(), confwatch.ErrDecode) {
		t.Fatalf("LastError = %v, want ErrDecode", m.LastError())
	}
	assertSnapshotUnchanged(t, before, m.Get())

	// 修复后继续自动更新。
	writeFile(t, path, "addr: \":6060\"\nworkers: 8\n")
	waitFor(t, "版本 3", func() bool { return m.Get().Version() == 3 })
	if got := m.Get().Value(); got.Addr != ":6060" || got.Workers != 8 {
		t.Fatalf("value = %+v, want addr=:6060 workers=8", got)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Watch = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Watch 未返回")
	}
}
