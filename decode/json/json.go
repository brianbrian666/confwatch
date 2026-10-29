// Package json 提供 confwatch 的 JSON 解码器。
//
// 它基于 encoding/json，因此结构体使用 json 标签。与直接用 encoding/json 的
// 区别有两点，都是为了让"配置文件写错"尽早暴露，而不是静默得到一份零值配置：
//
//   - 文档内容为 null 时报错，而不是解码成功且不修改目标值；
//   - 文档结束后还有多余内容时报错。
package json

import (
	"bytes"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"io"
)

// Decoder 是 confwatch.Decoder 的 JSON 实现，零值可用。
type Decoder struct {
	// DisallowUnknownFields 为 true 时，出现目标类型中不存在的字段会报错，
	// 有助于发现拼写错误。默认 false，与 encoding/json 的默认行为一致。
	DisallowUnknownFields bool
}

// New 返回默认配置的 JSON 解码器。
func New() *Decoder { return &Decoder{} }

// Decode 把 JSON 文档解码到 dst。
func (d *Decoder) Decode(data []byte, dst any) error {
	if isNullDocument(data) {
		return errors.New("json: document is null")
	}

	dec := stdjson.NewDecoder(bytes.NewReader(data))
	if d.DisallowUnknownFields {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("json: document is empty")
		}
		return fmt.Errorf("json: %w", err)
	}
	// 拒绝 "{}garbage" 这类"合法前缀 + 多余内容"的文档。
	if err := dec.Decode(new(stdjson.RawMessage)); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("json: unexpected trailing data")
		}
		return fmt.Errorf("json: trailing data: %w", err)
	}
	return nil
}

func isNullDocument(data []byte) bool {
	trimmed := bytes.TrimSpace(data)
	return len(trimmed) == len("null") && string(trimmed) == "null"
}
