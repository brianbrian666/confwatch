package json

import (
	"strings"
	"testing"
)

type sample struct {
	Addr string `json:"addr"`
	Port int    `json:"port"`
}

func TestDecodeValid(t *testing.T) {
	var got sample
	if err := New().Decode([]byte(`{"addr":":8080","port":80}`), &got); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if want := (sample{Addr: ":8080", Port: 80}); got != want {
		t.Fatalf("decoded = %+v, want %+v", got, want)
	}
}

func TestDecodeRejects(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "empty document", data: "", want: "empty"},
		{name: "null document", data: "  null  ", want: "null"},
		{name: "syntax error", data: `{"addr":`, want: "unexpected EOF"},
		{name: "trailing data", data: `{"addr":":1"}junk`, want: "trailing"},
		{name: "type mismatch", data: `{"port":"abc"}`, want: "type"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got sample
			err := New().Decode([]byte(tc.data), &got)
			if err == nil {
				t.Fatalf("Decode(%q) = nil, want error", tc.data)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %q, want it to contain %q", err, tc.want)
			}
			if !strings.HasPrefix(err.Error(), "json: ") {
				t.Fatalf("err = %q, want json: prefix", err)
			}
		})
	}
}

func TestDecodeUnknownFields(t *testing.T) {
	data := []byte(`{"addr":":8080","unknown":1}`)

	var lenient sample
	if err := New().Decode(data, &lenient); err != nil {
		t.Fatalf("lenient Decode: %v", err)
	}

	strict := &Decoder{DisallowUnknownFields: true}
	var strictDst sample
	if err := strict.Decode(data, &strictDst); err == nil {
		t.Fatal("strict Decode = nil, want error for unknown field")
	}
}
