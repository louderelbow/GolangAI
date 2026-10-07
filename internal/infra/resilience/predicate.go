package resilience

import (
	"context"
	"errors"
)

// IsDownstreamSuccess 是"这次调用对下游来说算不算成功"的**唯一判定口径**。
//
// 直接传给 gobreaker 的 IsSuccessful。项目里有两套熔断器，必须共用它：
//
//	resilience（[resilience]）        按失败率熔断，作用在「调用过程中」
//	inference（[inference.breaker]）  按连续失败数在「入队前」短路
//
// 为什么必须共用而不是各写各的：这个判定有两处**反直觉**的边界，
// 分开写的时候一边想到、另一边必然漏掉 —— 事实上就漏过：
// inference 那边少了"取消不算失败"，结果 5 个用户关掉页面就能把模型
// 熔断掉，而下游完全健康，故障表现为"偶发性的服务不可用"。
//
// 两处边界：
//
//  1. **调用方主动取消 → 不算失败**
//     用户点「停止生成」或关页面会让 ctx 取消。那是用户的选择，
//     不是下游的故障，不该累计到熔断计数里。
//
//  2. **下游超时 → 必须算失败**（所以这里刻意**不**豁免 DeadlineExceeded）
//     下游超时（连接超时 / 读超时 / 我们自己的 WithTimeout）恰恰是最常见的
//     故障形态，而 go-redis 之类的客户端在连接超时后返回的错误正好满足
//     errors.Is(err, context.DeadlineExceeded)。一旦把它也豁免，
//     熔断器就**永远不会跳闸**——压测时踩过：Redis 不可达时 p50 一直卡在
//     2058ms，熔断器却始终 closed，每个请求都白等一个完整超时。
//
// 用 errors.Is 而不是 err == context.Canceled：真实链路里 ctx 取消往往
// 已经被 http.Client / eino / 各家 SDK 包了一层。
func IsDownstreamSuccess(err error) bool {
	return err == nil || errors.Is(err, context.Canceled)
}
