package file

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brianbrian666/confwatch"
)

func TestFormatOf(t *testing.T) {
	tests := []struct {
		path   string
		want   confwatch.Format
		wantOK bool
	}{
		{path: "a.json", want: confwatch.FormatJSON, wantOK: true},
		{path: "a.yaml", want: confwatch.FormatYAML, wantOK: true},
		{path: "a.yml", want: confwatch.FormatYAML, wantOK: true},
		{path: "A.YAML", want: confwatch.FormatYAML, wantOK: true},
		{path: "/etc/app/conf.JSON", want: confwatch.FormatJSON, wantOK: true},
		{path: "a.toml"},
		{path: "noext"},
	}
	for _, tc := range tests {
		got, err := FormatOf(tc.path)
		if tc.wantOK && err != nil {
			t.Fatalf("FormatOf(%q) error: %v", tc.path, err)
		}
		if !tc.wantOK {
			if err == nil {
				t.Fatalf("FormatOf(%q) = %v, want error", tc.path, got)
			}
			continue
		}
		if got != tc.want {
			t.Fatalf("FormatOf(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.yaml")
	content := "server:\n  addr: \":8080\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	src := New(path)
	if got, want := src.Describe(), "file:"+path; got != want {
		t.Fatalf("Describe() = %q, want %q", got, want)
	}

	payload, err := src.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := string(payload.Data), content; got != want {
		t.Fatalf("data = %q, want %q", got, want)
	}
	if got := payload.Format; got != confwatch.FormatYAML {
		t.Fatalf("format = %v, want yaml", got)
	}
	if payload.Digest != "" {
		t.Fatalf("digest = %q, want empty (manager computes it)", payload.Digest)
	}
}

func TestLoadMissingFile(t *testing.T) {
	src := New(filepath.Join(t.TempDir(), "missing.json"))
	_, err := src.Load(context.Background())
	if err == nil {
		t.Fatal("Load = nil, want error")
	}
	if !strings.Contains(err.Error(), "read ") {
		t.Fatalf("err = %q, want read context", err)
	}
}

func TestLoadEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	if _, err := New(path).Load(context.Background()); err == nil {
		t.Fatal("Load = nil, want error for empty file")
	}
}

func TestLoadHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := New("whatever.json").Load(ctx); err == nil {
		t.Fatal("Load = nil, want context error")
	}
}

// TestNewWithFormat 覆盖没有扩展名的文件（容器挂载点常见）。
func TestNewWithFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config") // 无扩展名
	if err := os.WriteFile(path, []byte(`{"addr":":8080"}`), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	// 未指定格式时无法推断，必须报错而不是静默猜一个。
	src := New(path)
	if _, err := src.Load(context.Background()); err == nil {
		t.Fatal("Load = nil, want inability to infer format")
	}

	payload, err := NewWithFormat(path, confwatch.FormatJSON).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := payload.Format; got != confwatch.FormatJSON {
		t.Fatalf("format = %v, want json", got)
	}

	// FormatUnknown 等价于按扩展名推断。
	yamlPath := filepath.Join(dir, "app.yaml")
	if err := os.WriteFile(yamlPath, []byte("a: 1\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	payload, err = NewWithFormat(yamlPath, confwatch.FormatUnknown).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := payload.Format; got != confwatch.FormatYAML {
		t.Fatalf("format = %v, want yaml", got)
	}
}
