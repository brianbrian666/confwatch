package confwatch_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/brianbrian666/confwatch"
)

func TestZeroSnapshot(t *testing.T) {
	var snap confwatch.Snapshot[testConfig]

	if snap.Valid() {
		t.Fatal("zero snapshot must be invalid")
	}
	if got := snap.Version(); got != 0 {
		t.Fatalf("version = %d, want 0", got)
	}
	if got := snap.Digest(); got != "" {
		t.Fatalf("digest = %q, want empty", got)
	}
	if !snap.LoadedAt().IsZero() {
		t.Fatalf("loadedAt = %v, want zero", snap.LoadedAt())
	}
	if got := snap.Reason(); got != (confwatch.Reason{}) {
		t.Fatalf("reason = %v, want zero", got)
	}
	if got := snap.Value(); !reflect.DeepEqual(got, testConfig{}) {
		t.Fatalf("value = %+v, want zero", got)
	}
}

// TestValueIsACopy 说明 Value 返回的是值拷贝：修改其中的值类型字段不会影响快照。
func TestValueIsACopy(t *testing.T) {
	m := openManager(t, newSource(`{"addr":":8080","seq":1}`))

	cfg := m.Get().Value()
	cfg.Addr = "mutated"
	cfg.Seq = 42

	again := m.Get().Value()
	if again.Addr != ":8080" || again.Seq != 1 {
		t.Fatalf("snapshot was modified through Value(): %+v", again)
	}
}

// sharedDecoder 故意让发布的对象与外部 map 共享内存，用来演示"库无法保证深不可变"
// 这条边界，以及 WithCopyFunc 如何消除它。
type sharedDecoder struct {
	tags map[string]string
}

func (d *sharedDecoder) Decode(data []byte, dst any) error {
	cfg := dst.(*testConfig)
	cfg.Addr = "shared"
	cfg.Tags = d.tags
	return nil
}

// TestSharedReferencesAreTheDocumentedBoundary 记录一条无法由 API 封死的边界：
// 解码器把同一个 map 交给库之后，外部继续写这个 map 会影响已发布的快照。
func TestSharedReferencesAreTheDocumentedBoundary(t *testing.T) {
	shared := &sharedDecoder{tags: map[string]string{"env": "test"}}
	m, err := confwatch.Open[testConfig](context.Background(),
		confwatch.WithSource(newSource(`{}`)),
		confwatch.WithDecoder(confwatch.FormatJSON, shared),
	)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = m.Close() }()

	shared.tags["env"] = "mutated"

	if got := m.Get().Value().Tags["env"]; got != "mutated" {
		t.Fatalf("tags[env] = %q, want %q: without a copy func the snapshot shares the decoder's map", got, "mutated")
	}
}

// TestCopyFuncIsolatesPublishedValue 验证 WithCopyFunc 能消除上面的共享：
// 发布的对象与被解码的原对象不再共享内存。
func TestCopyFuncIsolatesPublishedValue(t *testing.T) {
	shared := &sharedDecoder{tags: map[string]string{"env": "test"}}
	m, err := confwatch.Open[testConfig](context.Background(),
		confwatch.WithSource(newSource(`{}`)),
		confwatch.WithDecoder(confwatch.FormatJSON, shared),
		confwatch.WithCopyFunc(copyTestConfig),
	)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = m.Close() }()

	shared.tags["env"] = "mutated"

	if got := m.Get().Value().Tags["env"]; got != "test" {
		t.Fatalf("tags[env] = %q, want %q: copy func must isolate the published config", got, "test")
	}
}

func TestVersionAndReasonKindString(t *testing.T) {
	if got, want := confwatch.Version(7).String(), "7"; got != want {
		t.Fatalf("Version.String() = %q, want %q", got, want)
	}
	tests := []struct {
		kind confwatch.ReasonKind
		want string
	}{
		{confwatch.ReasonInitial, "initial"},
		{confwatch.ReasonManualReload, "manual"},
		{confwatch.ReasonKind(99), "unknown(99)"},
	}
	for _, tc := range tests {
		if got := tc.kind.String(); got != tc.want {
			t.Fatalf("ReasonKind(%d).String() = %q, want %q", tc.kind, got, tc.want)
		}
	}
}

func TestFormatString(t *testing.T) {
	tests := []struct {
		format confwatch.Format
		want   string
	}{
		{confwatch.FormatJSON, "json"},
		{confwatch.FormatYAML, "yaml"},
		{confwatch.Format(99), "format(99)"},
	}
	for _, tc := range tests {
		if got := tc.format.String(); got != tc.want {
			t.Fatalf("Format(%d).String() = %q, want %q", tc.format, got, tc.want)
		}
	}
}
