package confwatch

import (
	"context"
	"strconv"
)

// Format 是配置载荷的格式标识。解码器按 Format 注册与查找。
type Format uint8

const (
	// FormatJSON 表示 JSON 载荷。
	FormatJSON Format = iota + 1
	// FormatYAML 表示 YAML 载荷。
	FormatYAML
)

func (f Format) String() string {
	switch f {
	case FormatJSON:
		return "json"
	case FormatYAML:
		return "yaml"
	default:
		return "format(" + strconv.Itoa(int(f)) + ")"
	}
}

// Payload 是配置源产出的原始载荷。
type Payload struct {
	// Data 是原始字节，不能为空。实现方不得在后续调用中复用这块内存，
	// 否则已发布配置可能与外部写入共享底层数组。
	Data []byte
	// Format 决定使用哪个已注册的解码器。
	Format Format
	// Digest 是可选的内容摘要。留空时由 Manager 用 sha256(Data) 计算。
	// 摘要用于判断内容是否真的变化，从而避免无意义的版本递增。
	Digest string
}

// Source 是配置来源的抽象。实现必须并发安全（同一个 Source 可能被反复调用）、
// 必须尊重 ctx 的取消，并且每次调用返回的 Data 不得被后续调用复用。
type Source interface {
	Load(ctx context.Context) (Payload, error)
	// Describe 返回用于诊断的简短来源描述，例如 "file:/etc/app/a.yaml"。
	// 它会在错误信息中出现，应保持稳定且开销极低。
	Describe() string
}

// Decoder 把某一种格式的原始字节解码到 T。
//
// 实现必须做到：每次调用都产生一份全新的、不与任何外部缓存共享可变内存的值。
// 失败的 Decode 不得写入部分结果后又返回 nil。
type Decoder interface {
	Decode(data []byte, dst any) error
}
