package confwatch

import "context"

// Validator 校验候选配置。校验发生在发布之前，因此校验失败不会影响当前配置。
//
// 实现必须并发安全且只读：v 指向即将发布的对象，校验器不得修改它，也不得保存
// 该指针供以后使用（否则等于在发布后写入已发布的快照）。需要修改配置请使用
// WithDefaults。
type Validator[T any] interface {
	Validate(ctx context.Context, v *T) error
}

// ValidatorFunc 让普通函数满足 Validator。
type ValidatorFunc[T any] func(ctx context.Context, v *T) error

func (f ValidatorFunc[T]) Validate(ctx context.Context, v *T) error { return f(ctx, v) }
