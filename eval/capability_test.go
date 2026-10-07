package eval

import (
	"strings"
	"testing"
)

// TestWilson95SmallSampleIsHonest Wilson 区间在小样本上必须诚实。
//
// 这是整个能力评测里最重要的一条保护：3 个样本全对时，
// 正态近似（Wald）会给出 [1.0, 1.0] —— 相当于宣称"绝对可靠"。
// Wilson 给出约 [0.44, 1.0]，这才让人有依据说"样本还不够"。
func TestWilson95SmallSampleIsHonest(t *testing.T) {
	lower, upper := Wilson95(3, 3)

	if upper != 1 {
		t.Errorf("3/3 的上界应当是 1，实际 %.3f", upper)
	}
	if lower > 0.5 {
		t.Errorf("3/3 的下界不该高于 0.5（说明区间被算窄了，等于假装可靠），实际 %.3f", lower)
	}
	if lower < 0.3 {
		t.Errorf("3/3 的下界不该低于 0.3，实际 %.3f", lower)
	}
}

func TestWilson95HandlesEdges(t *testing.T) {
	if l, u := Wilson95(0, 0); l != 0 || u != 0 {
		t.Errorf("n=0 应当返回 0,0，实际 %.3f,%.3f", l, u)
	}
	// 全错
	l, u := Wilson95(0, 10)
	if l != 0 {
		t.Errorf("0/10 的下界应当是 0，实际 %.3f", l)
	}
	if u < 0.2 || u > 0.4 {
		t.Errorf("0/10 的上界应当在 0.2~0.4，实际 %.3f", u)
	}
	// 区间必须包住点估计
	for _, tc := range [][2]int{{7, 10}, {19, 35}, {1, 5}} {
		l, u := Wilson95(tc[0], tc[1])
		p := float64(tc[0]) / float64(tc[1])
		if !(l <= p && p <= u) {
			t.Errorf("%d/%d 的区间 [%.3f,%.3f] 应当包住点估计 %.3f", tc[0], tc[1], l, u, p)
		}
	}
}

// TestVerdictInsufficientIsNotFail 样本不足必须判 insufficient，而不是 fail。
//
// 把"样本不足"报成"不达标"，会让门禁在样本少的阶段反复误报，
// 而反复误报的门禁最后一定会被人关掉 —— 那比没有门禁更糟。
func TestVerdictInsufficientIsNotFail(t *testing.T) {
	// 3 个样本、全对、阈值 0.9 —— 直觉上"达标"，但样本太少，不下结论
	s := newScore("refusalAccuracy", 3, 3, 0.9)
	if s.Verdict != VerdictInsufficient {
		t.Fatalf("3 个样本应当判 insufficient，实际 %s", s.Verdict)
	}
	if !strings.Contains(s.String(), "样本不足") {
		t.Errorf("展示文案应当说明样本不足：%s", s.String())
	}

	// 同样的比例，样本够了才给结论
	s = newScore("refusalAccuracy", 20, 20, 0.9)
	if s.Verdict != VerdictPass {
		t.Fatalf("20/20 应当达标，实际 %s", s.Verdict)
	}
}

func TestVerdictFailsOnlyWithEnoughSamples(t *testing.T) {
	// 样本够 + 低于阈值 → fail
	if s := newScore("m", 5, 20, 0.5); s.Verdict != VerdictFail {
		t.Fatalf("5/20 低于阈值 0.5，应当判 fail，实际 %s", s.Verdict)
	}
	// 不设阈值 → 永远不拦
	if s := newScore("m", 0, 3, 0); s.Verdict != VerdictPass {
		t.Fatalf("不设阈值时不该拦，实际 %s", s.Verdict)
	}
}

// TestBuildDimensionsSplitsByDifficulty 分维度必须真的把用例切开。
//
// 这是"企业级"的核心：一个总分无法指导优化 —— 它涨了，可能是简单档
// 多了两道题。分档之后才看得出"困难档从 0.31 涨到 0.58"这种真的变强。
func TestBuildDimensionsSplitsByDifficulty(t *testing.T) {
	cases := []Case{
		{ID: "a", Difficulty: "easy", Tags: []string{"single-hop"}, ExpectSources: []string{"x"}},
		{ID: "b", Difficulty: "easy", Tags: []string{"single-hop"}, ExpectSources: []string{"x"}},
		{ID: "c", Difficulty: "hard", Tags: []string{"refusal"}, ExpectRefusal: true},
	}
	results := []CaseResult{
		{ID: "a", ScoredRecall: true, RetrievalHit: true},
		{ID: "b", ScoredRecall: true, RetrievalHit: false},
		{ID: "c", ScoredRefusal: true, RefusalOK: true},
	}

	dims := BuildDimensions(cases, results, Metrics{RetrievalRecall: 0.5})

	byName := map[string]Dimension{}
	for _, d := range dims {
		byName[d.Kind+"="+d.Name] = d
	}

	easy, ok := byName["difficulty=easy"]
	if !ok {
		t.Fatal("应当有一个 difficulty=easy 维度")
	}
	if easy.Total != 2 {
		t.Fatalf("easy 档应当有 2 条，实际 %d", easy.Total)
	}
	// 2 条里命中 1 条 = 0.5；样本 2 < 10 → 样本不足
	if s := easy.ScoreOf("retrievalRecall"); s.Value != 0.5 || s.Verdict != VerdictInsufficient {
		t.Fatalf("easy 档召回应当为 0.5 且样本不足，实际 %.2f/%s", s.Value, s.Verdict)
	}

	hard, ok := byName["difficulty=hard"]
	if !ok {
		t.Fatal("应当有一个 difficulty=hard 维度")
	}
	if hard.Total != 1 {
		t.Fatalf("hard 档应当有 1 条，实际 %d", hard.Total)
	}

	// 标签维度也要有
	if _, ok := byName["tag=refusal"]; !ok {
		t.Error("应当按 tag 也切出一份")
	}
}

// TestBuildDimensionsOrderIsStable 输出顺序必须稳定。
//
// map 迭代顺序随机，不排序的话同一个报告每次跑出来顺序都不一样，
// 对比两次结果时非常难受。
func TestBuildDimensionsOrderIsStable(t *testing.T) {
	cases := []Case{
		{ID: "a", Difficulty: "hard"},
		{ID: "b", Difficulty: "easy"},
		{ID: "c", Difficulty: "medium"},
	}
	results := make([]CaseResult, len(cases))
	for i := range results {
		results[i].ID = cases[i].ID
	}

	for run := 0; run < 5; run++ {
		dims := BuildDimensions(cases, results, Metrics{})
		var got []string
		for _, d := range dims {
			got = append(got, d.Name)
		}
		want := []string{"easy", "medium", "hard"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("顺序应当按易到难稳定输出，实际 %v", got)
		}
	}
}

// TestCapabilitySummaryOnlyReportsFail 门禁只收 fail，不收 insufficient。
//
// 样本不足该做的是去补样本，而不是拦下一次发布。
func TestCapabilitySummaryOnlyReportsFail(t *testing.T) {
	dims := []Dimension{
		{Kind: "difficulty", Name: "easy", Scores: []Score{
			newScore("retrievalRecall", 2, 3, 0.9), // 样本不足
		}},
		{Kind: "difficulty", Name: "hard", Scores: []Score{
			newScore("retrievalRecall", 2, 20, 0.5), // 真 fail
		}},
	}

	failures := CapabilitySummary(dims)
	if len(failures) != 1 {
		t.Fatalf("应当只报 1 条 fail，实际 %d 条：%v", len(failures), failures)
	}
	if !strings.Contains(failures[0], "hard") {
		t.Errorf("报出来的应当是 hard 档：%s", failures[0])
	}

	if got := InsufficientSummary(dims); len(got) != 1 {
		t.Fatalf("应当有 1 条样本不足提示，实际 %d", len(got))
	}
}
