package resilience

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// TestIsDownstreamSuccess 这个函数是两套熔断器**唯一**的判定口径，
// 所以它自己必须有测试 —— 一旦被改错，两处熔断器会同时失效。
//
// 两条边界都是**反直觉**的，任何一条搞反都会造成真实故障：
//   - 取消要豁免：否则几个用户关页面就能把下游判死
//   - 超时不能豁免：否则熔断器永远不会跳闸
func TestIsDownstreamSuccess(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
		why  string
	}{
		{"正常成功", nil, true, "err == nil 当然是成功"},

		{"调用方取消", context.Canceled, true,
			"用户点停止/关页面 —— 是用户的选择，不是下游故障"},

		{"包装过的取消", fmt.Errorf("模型调用失败: %w", context.Canceled), true,
			"真实链路里 ctx 取消往往已被 SDK 包了一层，所以必须用 errors.Is"},

		{"下游超时", context.DeadlineExceeded, false,
			"超时是最常见的故障形态；豁免它熔断器就永远不跳闸（Redis 不可达时踩过）"},

		{"包装过的超时", fmt.Errorf("dial tcp: %w", context.DeadlineExceeded), false,
			"go-redis 等客户端在连接超时后返回的错误正好满足 errors.Is(DeadlineExceeded)"},

		{"普通错误", errors.New("上游 500"), false, "常规失败"},

		{"两者都有", errors.Join(context.Canceled, context.DeadlineExceeded), true,
			"同时包含取消和超时时，按'调用方主动取消'处理 —— 至少有一部分是用户造成的，不该判下游死"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsDownstreamSuccess(c.err); got != c.want {
				t.Errorf("IsDownstreamSuccess(%v) = %v，期望 %v\n理由：%s", c.err, got, c.want, c.why)
			}
		})
	}
}
