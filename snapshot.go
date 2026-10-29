package confwatch

import (
	"strconv"
	"time"
)

// Version 是配置的发布序号：从 1 开始，只在成功发布时递增。
// 失败的重载、因内容未变化而跳过的重载都不消耗版本号。
type Version uint64

func (v Version) String() string { return strconv.FormatUint(uint64(v), 10) }

// ReasonKind 说明一次发布的来源。
type ReasonKind uint8

const (
	// ReasonInitial 表示 Open 时的首次加载。
	ReasonInitial ReasonKind = iota + 1
	// ReasonManualReload 表示由调用方调用 Reload 或 TryReload 触发。
	ReasonManualReload
)

func (k ReasonKind) String() string {
	switch k {
	case ReasonInitial:
		return "initial"
	case ReasonManualReload:
		return "manual"
	default:
		return "unknown(" + strconv.Itoa(int(k)) + ")"
	}
}

// Reason 描述一次发布的来源与附加信息。
type Reason struct {
	Kind   ReasonKind
	Detail string
}

// header 是快照的元数据。它与配置值放在同一个对象里，因此二者总是一致的。
type header struct {
	name     string
	version  Version
	loadedAt time.Time
	digest   string
	reason   Reason
}

// Snapshot 是一份已发布配置的不可变视图。零值表示"没有有效快照"：
// 此时 Valid 返回 false，Value 返回 T 的零值。
//
// 已发布的 Snapshot 在库内部永不修改，可以被任意多个 goroutine 长期持有并读取。
// Value 返回 T 的浅拷贝，其中引用类型字段的只读约束见包文档。
type Snapshot[T any] struct {
	hdr header
	val T
}

// Valid 报告该快照是否由一次成功发布产生。
func (s Snapshot[T]) Valid() bool { return s.hdr.version > 0 }

// Value 返回配置值的浅拷贝。
//
// 修改返回值中的值类型字段不会影响快照；修改其中的 map/slice/指针内容属于
// 未定义行为，详见包文档"调用方持有的配置引用"。
func (s Snapshot[T]) Value() T { return s.val }

// Version 返回发布序号，零值快照返回 0。
func (s Snapshot[T]) Version() Version { return s.hdr.version }

// LoadedAt 返回该快照的发布时刻，零值快照返回 time.Time 零值。
func (s Snapshot[T]) LoadedAt() time.Time { return s.hdr.loadedAt }

// Digest 返回构建该快照时使用的载荷摘要，零值快照返回空字符串。
func (s Snapshot[T]) Digest() string { return s.hdr.digest }

// Reason 返回该快照的发布原因，零值快照返回 Reason 零值。
func (s Snapshot[T]) Reason() Reason { return s.hdr.reason }
