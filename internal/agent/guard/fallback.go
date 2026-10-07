package guard

import (
	"context"
	"fmt"
	"log"

	"deeptalk/internal/infra/metrics"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// FallbackModel 让主模型失败后自动切换备用模型。
//
// 兜底链的形状由 AGENT_SPEC 规定：主模型失败 → 原模型重试一次 →
// 依次尝试备用模型 → 全部失败返回 ErrAllModelsFailed（由调用方转成错误码）。
//
// 只在"上游打不通"时切换：调用方主动取消（ctx.Err() != nil）说明本次请求
// 已被放弃，换模型没有意义，还会白烧一次调用。
type FallbackModel struct {
	primary   model.ToolCallingChatModel
	fallbacks []model.ToolCallingChatModel
	names     []string
}

// NewFallbackModel 构造兜底链；names 用于日志，长度不足时用序号兜底。
func NewFallbackModel(primary model.ToolCallingChatModel, fallbacks []model.ToolCallingChatModel, names []string) *FallbackModel {
	return &FallbackModel{primary: primary, fallbacks: fallbacks, names: names}
}

// HasFallback 是否配置了备用模型。
func (f *FallbackModel) HasFallback() bool { return len(f.fallbacks) > 0 }

// WithTools 把同一批工具绑定到链上每个模型。
// 漏绑会导致备用模型"看不见工具"，退化成纯对话。
func (f *FallbackModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	primary, err := f.primary.WithTools(tools)
	if err != nil {
		return nil, err
	}
	bound := make([]model.ToolCallingChatModel, 0, len(f.fallbacks))
	for _, fb := range f.fallbacks {
		b, err := fb.WithTools(tools)
		if err != nil {
			return nil, err
		}
		bound = append(bound, b)
	}
	return &FallbackModel{primary: primary, fallbacks: bound, names: f.names}, nil
}

func (f *FallbackModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	msg, err := f.primary.Generate(ctx, input, opts...)
	if err == nil {
		return msg, nil
	}
	if ctx.Err() != nil {
		return nil, err
	}

	// 原模型重试一次：瞬时网络抖动占上游失败的大头，这次重试往往就够了
	if retried, retryErr := f.primary.Generate(ctx, input, opts...); retryErr == nil {
		metrics.CountAgentDegraded("retry")
		return retried, nil
	} else if ctx.Err() != nil {
		return nil, retryErr
	} else {
		err = retryErr
	}

	for i, fb := range f.fallbacks {
		metrics.CountAgentDegraded("fallback_model")
		log.Printf("[agent] primary failed, fallback to %s: %v", f.nameAt(i), err)

		m, e := fb.Generate(ctx, input, opts...)
		if e == nil {
			return m, nil
		}
		err = e
	}

	metrics.CountAgentDegraded("error")
	return nil, fmt.Errorf("%w: %v", ErrAllModelsFailed, err)
}

func (f *FallbackModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	stream, err := f.primary.Stream(ctx, input, opts...)
	if err == nil {
		return stream, nil
	}
	if ctx.Err() != nil {
		return nil, err
	}

	if retried, retryErr := f.primary.Stream(ctx, input, opts...); retryErr == nil {
		metrics.CountAgentDegraded("retry")
		return retried, nil
	} else if ctx.Err() != nil {
		return nil, retryErr
	} else {
		err = retryErr
	}

	for i, fb := range f.fallbacks {
		metrics.CountAgentDegraded("fallback_model")
		log.Printf("[agent] primary stream failed, fallback to %s: %v", f.nameAt(i), err)

		s, e := fb.Stream(ctx, input, opts...)
		if e == nil {
			return s, nil
		}
		err = e
	}

	metrics.CountAgentDegraded("error")
	return nil, fmt.Errorf("%w: %v", ErrAllModelsFailed, err)
}

func (f *FallbackModel) nameAt(i int) string {
	if i < len(f.names) && f.names[i] != "" {
		return f.names[i]
	}
	return fmt.Sprintf("fallback-%d", i+1)
}
