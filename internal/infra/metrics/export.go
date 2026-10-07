package metrics

import (
	"io"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
)

// WriteTo 把当前指标按 Prometheus 文本格式写出去。
//
// 给**非 HTTP 的进程**用：评测 CLI 跑完把结果落盘或打到标准输出，
// 交给 Pushgateway 或 node_exporter 的 textfile collector。
//
// 为什么不直接复用 Handler()：那是 http.Handler，需要一个真实的
// *http.Request 才能安全工作（它要读 method 与 Accept 头做内容协商），
// 在 CLI 里伪造一个 request 只为拿文本，是把简单的事绕复杂了。
// 查询 DefaultGatherer 才是更底层的入口。
func WriteTo(w io.Writer) error {
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		return err
	}

	enc := expfmt.NewEncoder(w, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, mf := range families {
		if err := enc.Encode(mf); err != nil {
			return err
		}
	}
	return nil
}
