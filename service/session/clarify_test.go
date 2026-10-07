package session

import "testing"

// TestAugmentQuestionWording 点选项和自己填必须分开措辞。
//
// 两者都表现为"用户在上一轮追问后给了一段文字"，但对模型的意义不同：
// 前者是"在给定范围里挑了一个"，后者是"你给的选项都不对，我来说"。
// 混成同一句话会让模型误以为用户认可了预设框架——对开放式回答是误导。
func TestAugmentQuestionWording(t *testing.T) {
	const q = "帮我写个 SQL，取每个部门薪水最高的员工"

	cases := []struct {
		name string
		cl   *ClarifyContext
		want string
	}{
		{
			name: "没有澄清上下文时原样返回",
			cl:   nil,
			want: q,
		},
		{
			name: "用户点了选项",
			cl:   &ClarifyContext{Question: "你用的是哪种数据库？", Label: "MySQL"},
			want: q + "\n（补充信息：针对「你用的是哪种数据库？」，用户选择了「MySQL」）",
		},
		{
			name: "用户自己填的",
			cl:   &ClarifyContext{Question: "你用的是哪种数据库？", Label: "达梦 DM8", Custom: true},
			want: q + "\n（补充信息：针对「你用的是哪种数据库？」，用户补充说明「达梦 DM8」）",
		},
		{
			name: "回答为空时不拼任何东西",
			cl:   &ClarifyContext{Question: "你用的是哪种数据库？", Label: "   "},
			want: q,
		},
		{
			name: "没有原问题（客户端没带回来）时用短格式",
			cl:   &ClarifyContext{Label: "年假"},
			want: q + "\n（补充信息：用户选择了「年假」）",
		},
		{
			name: "没有原问题 + 自己填的",
			cl:   &ClarifyContext{Label: "调休三天", Custom: true},
			want: q + "\n（补充信息：用户补充说明「调休三天」）",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := augmentQuestion(q, c.cl); got != c.want {
				t.Errorf("\n得到: %s\n期望: %s", got, c.want)
			}
		})
	}
}
