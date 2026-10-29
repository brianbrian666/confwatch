// Package yaml 提供 confwatch 的 YAML 解码器。
//
// 解码路径是：YAML -> 通用 Go 值 -> 归一化为 JSON 数据模型 -> encoding/json -> T。
// 这样同一份结构体只需要一套 json 标签就能同时支持 YAML 与 JSON 配置，也保证两
// 种格式对未知字段、数字、布尔、null 的处理完全一致。
//
// 代价（属于有意设计，已写入文档）：
//
//   - 只识别 json 标签，不识别 yaml 标签；
//   - YAML 特有类型（时间戳、!!binary）被转换成 JSON 能表达的形式，因此
//     time.Duration 字段不能使用 "5s" 这种字面量；
//   - 映射的键必须是字符串，非字符串键直接报错，而不是静默转换。
package yaml

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	yamlv3 "gopkg.in/yaml.v3"
)

// Decoder 是 confwatch.Decoder 的 YAML 实现，零值可用。
type Decoder struct{}

// New 返回 YAML 解码器。
func New() *Decoder { return &Decoder{} }

// Decode 把 YAML 文档解码到 dst。
func (d *Decoder) Decode(data []byte, dst any) error {
	var raw any
	if err := yamlv3.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("yaml: %w", err)
	}
	// 空文档与 "null" 文档都会得到 nil。此时若继续解码，json 会把 null 解成
	// "什么也不做"，我们会静默发布一份零值配置，因此必须报错。
	if raw == nil {
		return errors.New("yaml: document is empty or null")
	}

	tree, err := normalize(raw)
	if err != nil {
		return err
	}
	buf, err := json.Marshal(tree)
	if err != nil {
		return fmt.Errorf("yaml: %w", err)
	}
	if err := json.Unmarshal(buf, dst); err != nil {
		return fmt.Errorf("yaml: %w", err)
	}
	return nil
}

// normalize 把 yaml.v3 产出的值转换成只含 JSON 数据模型类型的树。
func normalize(v any) (any, error) {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			n, err := normalize(val)
			if err != nil {
				return nil, err
			}
			out[k] = n
		}
		return out, nil
	case map[any]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			key, ok := k.(string)
			if !ok {
				return nil, fmt.Errorf("yaml: map key %v (%T) is not a string", k, k)
			}
			n, err := normalize(val)
			if err != nil {
				return nil, err
			}
			out[key] = n
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			n, err := normalize(val)
			if err != nil {
				return nil, err
			}
			out[i] = n
		}
		return out, nil
	case nil, bool, string, int, int64, uint64, float64, time.Time, []byte:
		return x, nil
	default:
		return nil, fmt.Errorf("yaml: unsupported value type %T", v)
	}
}
