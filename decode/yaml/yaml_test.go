package yaml

import (
	"reflect"
	"strings"
	"testing"
)

type sampleDB struct {
	DSN     string `json:"dsn"`
	MaxOpen int    `json:"max_open"`
}

type sampleServer struct {
	Addr  string   `json:"addr"`
	Hosts []string `json:"hosts"`
}

type sample struct {
	Server sampleServer `json:"server"`
	DB     sampleDB     `json:"db"`
	Peers  []sampleDB   `json:"peers"`
}

func TestDecodeValid(t *testing.T) {
	doc := `
server:
  addr: ":8080"
  hosts:
    - a
    - b
db:
  dsn: "postgres://localhost/app"
  max_open: 20
peers:
  - dsn: "postgres://peer1/app"
    max_open: 5
`
	var got sample
	if err := New().Decode([]byte(doc), &got); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	want := sample{
		Server: sampleServer{Addr: ":8080", Hosts: []string{"a", "b"}},
		DB:     sampleDB{DSN: "postgres://localhost/app", MaxOpen: 20},
		Peers:  []sampleDB{{DSN: "postgres://peer1/app", MaxOpen: 5}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded = %+v, want %+v", got, want)
	}
}

// TestYAMLOnlyTagIsIgnored 记录一条有意的设计限制：解码走 JSON 数据模型，
// 因此只识别 json 标签，yaml 标签不生效。
func TestYAMLOnlyTagIsIgnored(t *testing.T) {
	var got struct {
		Value int `yaml:"yaml_only"`
	}
	if err := New().Decode([]byte("yaml_only: 7"), &got); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Value != 0 {
		t.Fatalf("value = %d, want 0: yaml tags must be ignored", got.Value)
	}
}

func TestDecodeRejects(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want string
	}{
		{name: "empty document", doc: "", want: "empty"},
		{name: "comment only", doc: "# nothing here\n", want: "empty"},
		{name: "null document", doc: "null\n", want: "empty or null"},
		{name: "syntax error", doc: "server: [unclosed\n", want: "yaml: "},
		{name: "type mismatch", doc: "db:\n  max_open: not-a-number\n", want: "yaml: "},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got sample
			err := New().Decode([]byte(tc.doc), &got)
			if err == nil {
				t.Fatalf("Decode(%q) = nil, want error", tc.doc)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestDecodeNonStringKeyIsRejected(t *testing.T) {
	var got map[string]any
	if err := New().Decode([]byte("1: a\n2: b\n"), &got); err == nil {
		t.Fatalf("Decode = nil, want error for non-string key (got %v)", got)
	}
}

func TestDecodeObjectIntoStringFails(t *testing.T) {
	var got string
	if err := New().Decode([]byte("a: 1\n"), &got); err == nil {
		t.Fatal("Decode = nil, want error")
	}
}

// TestDecodeYAMLSpecificScalars 记录 YAML 特有标量被归一化成 JSON 形式的规则：
// 时间戳变成 RFC3339 字符串，!!binary 变成解码后的字符串。
func TestDecodeYAMLSpecificScalars(t *testing.T) {
	var got struct {
		When string `json:"when"`
		Blob string `json:"blob"`
	}
	doc := "when: 2026-10-29T10:00:00Z\nblob: !!binary aGk=\n"
	if err := New().Decode([]byte(doc), &got); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if want := "2026-10-29T10:00:00Z"; got.When != want {
		t.Fatalf("when = %q, want %q", got.When, want)
	}
	if want := "hi"; got.Blob != want {
		t.Fatalf("blob = %q, want %q", got.Blob, want)
	}
}
