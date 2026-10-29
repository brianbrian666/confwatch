package confwatch

import (
	"context"
	"fmt"
)

// 以下三个回调由泛型选项构造，把与 T 相关的逻辑封装成闭包，用 any 承载 T 的指针。
//
// 这样 Option 本身可以不带类型参数：否则 WithName、WithSource 这类与 T 无关的
// 选项在每个调用点都必须显式写出 [Config]，因为它们无法从参数推导 T。
type (
	validatorFunc func(ctx context.Context, cfg any) error
	defaultsFunc  func(cfg any) error
	copyFunc      func(ptr any) error
)

type settings struct {
	name       string
	source     Source
	decoders   map[Format]Decoder
	validators []validatorFunc
	defaults   defaultsFunc
	copyFunc   copyFunc
	noopSkip   bool
}

func (s *settings) init() {
	s.noopSkip = true
	s.decoders = make(map[Format]Decoder, 2)
}

func (s *settings) validate() error {
	if s.source == nil {
		return fmt.Errorf("%w: no source, use WithSource", ErrNotConfigured)
	}
	if len(s.decoders) == 0 {
		return fmt.Errorf("%w: no decoder, use WithDecoder", ErrNotConfigured)
	}
	return nil
}

// Option 配置 Manager，通过 With* 函数构造。不要自行构造 Option。
//
// Option 作用于一个内部设置对象，因此用户无法实现自己的选项；需要新选项时请
// 在库内新增 With* 函数。
type Option func(*settings) error

// WithName 设置 Manager 名称，仅用于诊断信息与错误消息。
func WithName(name string) Option {
	return func(s *settings) error {
		s.name = name
		return nil
	}
}

// WithSource 设置配置源。必填。
func WithSource(src Source) Option {
	return func(s *settings) error {
		if src == nil {
			return fmt.Errorf("%w: nil source", ErrNotConfigured)
		}
		s.source = src
		return nil
	}
}

// WithDecoder 为某种格式注册解码器。至少需要一个；配置源产出的 Format 必须有
// 对应解码器，否则加载失败并返回 ErrNoDecoder。
func WithDecoder(format Format, d Decoder) Option {
	return func(s *settings) error {
		if d == nil {
			return fmt.Errorf("%w: nil decoder for %s", ErrNotConfigured, format)
		}
		s.decoders[format] = d
		return nil
	}
}

// WithNoopSkip 控制"内容未变化时是否跳过发布"。默认开启：摘要与当前快照相同时
// 不发布、不递增版本、不记录新时间戳，Reload 直接返回当前快照。
func WithNoopSkip(enabled bool) Option {
	return func(s *settings) error {
		s.noopSkip = enabled
		return nil
	}
}

// WithValidator 注册校验器。可多次调用，按注册顺序全部执行，不会短路；
// 所有失败原因会被 errors.Join 聚合后一起返回。
//
// 类型参数由实参推导，例如 WithValidator(myValidator) 中的 T 取自
// myValidator 的 Validator[Config]。
func WithValidator[T any](vs ...Validator[T]) Option {
	return func(s *settings) error {
		for i, v := range vs {
			if v == nil {
				return fmt.Errorf("%w: validator #%d is nil", ErrNotConfigured, i)
			}
			val := v
			s.validators = append(s.validators, func(ctx context.Context, cfg any) error {
				p, ok := cfg.(*T)
				if !ok {
					return fmt.Errorf("%w: validator expects %T, got %T", ErrNotConfigured, (*T)(nil), cfg)
				}
				return val.Validate(ctx, p)
			})
		}
		return nil
	}
}

// WithDefaults 设置默认值函数：在解码与拷贝之后、校验之前执行，直接修改即将发布
// 的值。可多次调用，后设置的覆盖先设置的。
//
// 它适合表达"文件里没写时的兜底值"，但注意它无法区分"没写"与"显式写了零值"。
func WithDefaults[T any](fn func(*T)) Option {
	return func(s *settings) error {
		if fn == nil {
			return fmt.Errorf("%w: nil defaults func", ErrNotConfigured)
		}
		s.defaults = func(cfg any) error {
			p, ok := cfg.(*T)
			if !ok {
				return fmt.Errorf("%w: defaults func expects %T, got %T", ErrNotConfigured, (*T)(nil), cfg)
			}
			fn(p)
			return nil
		}
		return nil
	}
}

// WithCopyFunc 在解码之后、默认值与校验之前，用 fn 返回的深拷贝替换候选值。
//
// 用于让发布的对象与被解码的原对象彻底不共享内存。只有当 T 含引用类型字段、
// 且解码器或外部缓存可能继续写这些字段时才需要它；fn 必须是真正的深拷贝。
func WithCopyFunc[T any](fn func(T) T) Option {
	return func(s *settings) error {
		if fn == nil {
			return fmt.Errorf("%w: nil copy func", ErrNotConfigured)
		}
		s.copyFunc = func(ptr any) error {
			p, ok := ptr.(*T)
			if !ok {
				return fmt.Errorf("%w: copy func expects %T, got %T", ErrNotConfigured, (*T)(nil), ptr)
			}
			*p = fn(*p)
			return nil
		}
		return nil
	}
}
