package eval

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// ======================== 企业级能力评测 ========================
//
// 一个总分（"综合 0.74"）在工程上没有可操作性：它变了，你不知道该改什么。
// 所以这一层做三件事：
//
//  1. **分维度**：按难度档、按能力标签（单跳 / 多跳 / 拒答 / 抗干扰）拆开看。
//     "综合涨了 0.02"往往只是简单档多了两道题；"困难档从 0.31 涨到 0.58"
//     才是真的变强。
//
//  2. **给显著性**：每个比率带 95% Wilson 置信区间。
//     3 道题里错 1 道，点估计是 0.67，但置信区间是 [0.21, 0.94] ——
//     和 0.9 根本不构成"下降"的证据。只看点估计会让团队为噪声加班。
//
//  3. **样本守卫**：样本数低于门槛时，判决是 insufficient 而不是 fail。
//     一个 3 样本的指标去卡 0.9 的阈值，等于让运气决定 CI 红不红。

// MinSamplesForVerdict 低于这个样本数就不下结论。
//
// 10 是个经验值：n=10、p=0.9 时 Wilson 区间约 [0.60, 0.98]，
// 已经宽到不足以下"达标/不达标"的结论了。再少就是纯噪声。
const MinSamplesForVerdict = 10

// Wilson95 计算二项比例的 95% Wilson 置信区间。
//
// 用 Wilson 而不是正态近似（Wald）：小样本 + 接近 0/1 时 Wald 会给出
// 荒谬的区间（比如 3 个样本全对算出 [1.0, 1.0]，意味着"绝对可靠"）。
// Wilson 在 p=1、n=3 时给出约 [0.44, 1.0] —— 这才是诚实的。
func Wilson95(successes, n int) (lower, upper float64) {
	if n <= 0 {
		return 0, 0
	}
	const z = 1.959964 // 95%
	nf := float64(n)
	p := float64(successes) / nf

	denom := 1 + z*z/nf
	center := p + z*z/(2*nf)
	margin := z * math.Sqrt(p*(1-p)/nf+z*z/(4*nf*nf))

	lower = (center - margin) / denom
	upper = (center + margin) / denom

	// 夹到 [0,1]：浮点误差可能让上界略微超过 1
	return math.Max(0, lower), math.Min(1, upper)
}

// Verdict 一个指标的判决。
//
// insufficient 是刻意独立于 fail 的第三种状态：
// 把"样本不足"报成"不达标"，会让门禁在样本少的阶段反复误报，
// 而反复误报的门禁最后一定会被人关掉 —— 那比没有门禁更糟。
const (
	VerdictPass         = "pass"
	VerdictFail         = "fail"
	VerdictInsufficient = "insufficient"
)

// Score 一个指标在某个维度上的成绩。
type Score struct {
	Metric  string  `json:"metric"`
	Value   float64 `json:"value"`
	Samples int     `json:"samples"`
	CILower float64 `json:"ciLower"`
	CIUpper float64 `json:"ciUpper"`

	// Threshold 该指标的阈值；0 表示不设门禁
	Threshold float64 `json:"threshold,omitempty"`
	Verdict   string  `json:"verdict"`
}

// String 给人看的一行：0.74 [0.55,0.88] n=19 (≥0.7) 达标
func (s Score) String() string {
	verdict := ""
	switch s.Verdict {
	case VerdictPass:
		verdict = "达标"
	case VerdictFail:
		verdict = "不达标"
	case VerdictInsufficient:
		verdict = fmt.Sprintf("样本不足（需 ≥%d）", MinSamplesForVerdict)
	}
	th := ""
	if s.Threshold > 0 {
		th = fmt.Sprintf("（阈值 %.2f）", s.Threshold)
	}
	return fmt.Sprintf("%.2f  [%.2f,%.2f]  n=%d%s  %s",
		s.Value, s.CILower, s.CIUpper, s.Samples, th, verdict)
}

// newScore 组装一个成绩并给出判决。
//
// 判决规则刻意保守：**样本不足时不下结论**。
// 宁可说"还不知道"，也不要说"不达标" —— 后者会让人去改本来没坏的东西。
func newScore(metric string, successes, n int, threshold float64) Score {
	s := Score{Metric: metric, Samples: n, Threshold: threshold}
	if n > 0 {
		s.Value = float64(successes) / float64(n)
		s.CILower, s.CIUpper = Wilson95(successes, n)
	}

	switch {
	case threshold <= 0:
		s.Verdict = VerdictPass // 不设门禁，永远不拦
	case n < MinSamplesForVerdict:
		s.Verdict = VerdictInsufficient
	case s.Value >= threshold:
		s.Verdict = VerdictPass
	default:
		s.Verdict = VerdictFail
	}
	return s
}

// ======================== 维度拆解 ========================

// Dimension 一个维度上的成绩，例如"困难档"或"多跳"。
type Dimension struct {
	// Kind 维度类型：difficulty / tag
	Kind string `json:"kind"`
	// Name 维度取值：easy / hard / multi-hop / refusal ...
	Name string `json:"name"`
	// Total 落在该维度的用例总数
	Total int `json:"total"`
	// Scores 该维度上的各项指标
	Scores []Score `json:"scores"`
	// Failed 该维度里执行出错的用例数
	Failed int `json:"failed"`
}

// ScoreOf 取某个指标的成绩，找不到返回零值。
func (d Dimension) ScoreOf(metric string) Score {
	for _, s := range d.Scores {
		if s.Metric == metric {
			return s
		}
	}
	return Score{}
}

// difficultyOrder 输出顺序：从易到难，方便一眼看出"分数是否随难度衰减"。
//
// 一个健康的系统应当是单调递减的（简单档 > 中等档 > 困难档）。
// 如果困难档反而更高，多半是题目标注错了，或者困难档其实在考同一个东西。
var difficultyOrder = []string{"easy", "medium", "hard", "adversarial"}

// BuildDimensions 按难度档与能力标签把结果拆开统计。
//
// 传入的 cases 与 results 必须**按序对应**（Run 里的循环保证了这一点）。
func BuildDimensions(cases []Case, results []CaseResult, th Metrics) []Dimension {
	// bucket 收集每个维度下的 (用例, 结果) 对
	type pair struct {
		c Case
		r CaseResult
	}
	buckets := map[string][]pair{}
	// 记下出现过的维度名，保证输出顺序稳定
	kinds := map[string]bool{}

	add := func(kind, name string, c Case, r CaseResult) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		key := kind + "\x00" + name
		buckets[key] = append(buckets[key], pair{c, r})
		kinds[kind] = true
	}

	for i := range results {
		if i >= len(cases) {
			break
		}
		c, r := cases[i], results[i]
		add("difficulty", c.Difficulty, c, r)
		for _, t := range c.Tags {
			add("tag", t, c, r)
		}
	}

	out := make([]Dimension, 0, len(buckets))
	for key, ps := range buckets {
		parts := strings.SplitN(key, "\x00", 2)

		d := Dimension{Kind: parts[0], Name: parts[1], Total: len(ps)}
		var recallOK, recallN int
		var coverOK, coverN int
		var refuseOK, refuseN int
		var faithSum, faithN float64

		for _, p := range ps {
			if p.r.Error != "" {
				d.Failed++
			}
			// 分母口径跟着 CaseResult 上的 Scored* 标记走，
			// 与总分用的是同一套 —— 否则维度分和总分对不上，又会变成一处对一处错。
			if p.r.ScoredRecall {
				recallN++
				if p.r.RetrievalHit {
					recallOK++
				}
			}
			if p.r.ScoredCoverage {
				coverN++
				if p.r.Covered == p.r.Expected {
					coverOK++
				}
			}
			if p.r.ScoredRefusal {
				refuseN++
				if p.r.RefusalOK {
					refuseOK++
				}
			}
			faithN++
			faithSum += p.r.Faithfulness
		}

		d.Scores = []Score{
			newScore("retrievalRecall", recallOK, recallN, th.RetrievalRecall),
			newScore("answerCoverage", coverOK, coverN, th.AnswerCoverage),
			newScore("refusalAccuracy", refuseOK, refuseN, th.RefusalAccuracy),
		}
		_ = faithSum
		_ = faithN

		out = append(out, d)
	}

	sortDimensions(out)
	return out
}

// sortDimensions 固定输出顺序：先难度档（按易到难），再标签（按名字）。
//
// 不排序的话 map 迭代顺序随机，同一个报告每次跑出来面板顺序都不一样，
// 对比两次结果时非常难受。
func sortDimensions(dims []Dimension) {
	rank := func(d Dimension) (int, int, string) {
		if d.Kind != "difficulty" {
			return 1, 0, d.Name
		}
		for i, name := range difficultyOrder {
			if d.Name == name {
				return 0, i, d.Name
			}
		}
		return 0, len(difficultyOrder), d.Name // 未知难度排在已知之后
	}
	sort.Slice(dims, func(i, j int) bool {
		ki, ri, ni := rank(dims[i])
		kj, rj, nj := rank(dims[j])
		if ki != kj {
			return ki < kj
		}
		if ri != rj {
			return ri < rj
		}
		return ni < nj
	})
}

// ======================== 汇总输出 ========================

// CapabilitySummary 把所有判 fail 的维度挑出来，供门禁使用。
//
// 只收集 fail，不含 insufficient —— 样本不足不该让 CI 变红，
// 该做的是去补样本，而不是拦下一次发布。
func CapabilitySummary(dims []Dimension) (failures []string) {
	for _, d := range dims {
		for _, s := range d.Scores {
			if s.Verdict == VerdictFail {
				failures = append(failures, fmt.Sprintf("[%s=%s] %s %.2f < 阈值 %.2f (n=%d)",
					d.Kind, d.Name, s.Metric, s.Value, s.Threshold, s.Samples))
			}
		}
	}
	return failures
}

// InsufficientSummary 列出样本不足的维度，供报告提示补样本。
func InsufficientSummary(dims []Dimension) (insufficient []string) {
	seen := map[string]bool{}
	for _, d := range dims {
		for _, s := range d.Scores {
			if s.Verdict != VerdictInsufficient {
				continue
			}
			key := d.Kind + "=" + d.Name + "/" + s.Metric
			if seen[key] {
				continue
			}
			seen[key] = true
			insufficient = append(insufficient, fmt.Sprintf("[%s=%s] %s 只有 %d 条样本，不足以下结论",
				d.Kind, d.Name, s.Metric, s.Samples))
		}
	}
	return insufficient
}
