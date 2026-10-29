package confwatch_test

import (
	"context"
	"errors"
	"testing"

	"github.com/brianbrian666/confwatch"
	jsoncodec "github.com/brianbrian666/confwatch/decode/json"
)

func TestNilOptionsAreRejected(t *testing.T) {
	tests := []struct {
		name string
		opt  confwatch.Option
	}{
		{name: "nil option", opt: nil},
		{name: "nil source", opt: confwatch.WithSource(nil)},
		{name: "nil decoder", opt: confwatch.WithDecoder(confwatch.FormatJSON, nil)},
		{name: "nil validator", opt: confwatch.WithValidator[testConfig](nil)},
		{name: "nil defaults", opt: confwatch.WithDefaults[testConfig](nil)},
		{name: "nil copy func", opt: confwatch.WithCopyFunc[testConfig](nil)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, err := confwatch.Open[testConfig](context.Background(),
				confwatch.WithSource(newSource(`{}`)),
				confwatch.WithDecoder(confwatch.FormatJSON, jsoncodec.New()),
				tc.opt,
			)
			if m != nil {
				t.Fatal("expected nil manager")
			}
			if !errors.Is(err, confwatch.ErrNotConfigured) {
				t.Fatalf("err = %v, want ErrNotConfigured", err)
			}
		})
	}
}
