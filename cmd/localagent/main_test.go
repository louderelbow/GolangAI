package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	agentlocal "deeptalk/internal/agent/local"
)

// newTestAgent 造一个工作区，并返回指向它的 agent，以及一个**工作区之外**的目录。
//
// 真实路径要对齐生产：resolveWorkspace 会 EvalSymlinks，
// 而已解析过的 root 与未解析的 root 在 Windows 上可能不是同一个字符串
// （临时目录常带 8.3 短名），拿错一个 within() 就会误判。
func newTestAgent(t *testing.T) (a *agent, root, outside string) {
	t.Helper()

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("解析工作区失败: %v", err)
	}
	outside, err = filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("解析外部目录失败: %v", err)
	}
	return &agent{root: root}, root, outside
}

// TestResolveRejectsEscape 是这整套功能的**安全核心**：
// 模型给的路径绝不能跑出工作区。
//
// 模型完全可能（被提示词诱导、或者干脆自己编）写出 "../../../etc/passwd"
// 这种参数。它跑在服务端，但路径解析发生在用户自己的机器上——
// 这里拒绝掉，服务端就没有任何绕过的办法。
func TestResolveRejectsEscape(t *testing.T) {
	a, root, outside := newTestAgent(t)
	outsideName := filepath.Base(outside)

	cases := []struct {
		name string
		path string
	}{
		{"上一级", ".."},
		{"父目录的文件", filepath.Join("..", outsideName, "secret.txt")},
		{"多级穿越", filepath.Join("..", "..", "..", "etc", "passwd")},
		{"穿越后再回来也不行", filepath.Join("..", "sub", "..", "x")},
		{"正斜杠穿越", "../" + outsideName + "/secret.txt"},
		{"绝对路径", filepath.Join(outside, "secret.txt")},
	}
	if runtime.GOOS == "windows" {
		cases = append(cases, struct{ name, path string }{"Windows 绝对路径", `C:\Windows\win.ini`})
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := a.resolve(c.path)
			if err == nil {
				t.Fatalf("路径 %q 必须被拒绝，却解析成了 %q", c.path, got)
			}
			if !strings.Contains(err.Error(), "工作区") && !strings.Contains(err.Error(), "相对") {
				t.Errorf("拒绝理由应当是「越界」或「必须相对路径」，实际 %q", err.Error())
			}
		})
	}
	_ = root
}

// TestResolveAllowsInside 正常路径必须放行，否则功能没法用。
func TestResolveAllowsInside(t *testing.T) {
	a, root, _ := newTestAgent(t)

	sub := filepath.Join(root, "internal")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	file := filepath.Join(sub, "a.go")
	if err := os.WriteFile(file, []byte("package a"), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}

	for _, rel := range []string{"", ".", "internal", "internal/a.go", "./internal/./a.go"} {
		got, err := a.resolve(rel)
		if err != nil {
			t.Fatalf("路径 %q 应当被允许，实际 %v", rel, err)
		}
		if !within(root, got) {
			t.Fatalf("路径 %q 解析到了工作区之外: %q", rel, got)
		}
	}
}

// TestResolveRejectsSymlinkEscape 符号链接逃逸必须被挡住。
//
// 只校验"逻辑路径在工作区内"是不够的：`ln -s /outside link` 之后，
// `link/secret.txt` 在逻辑上完全位于工作区内，但真实路径在外面。
// 这就是为什么 resolve 要再做一次 EvalSymlinks 校验。
func TestResolveRejectsSymlinkEscape(t *testing.T) {
	a, root, outside := newTestAgent(t)

	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("机密"), 0o644); err != nil {
		t.Fatalf("写外部文件失败: %v", err)
	}

	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		// Windows 上建符号链接需要开发者模式或管理员权限
		t.Skipf("当前环境无法创建符号链接（%v），跳过软链逃逸用例", err)
	}

	if got, err := a.resolve("escape/secret.txt"); err == nil {
		t.Fatalf("借符号链接逃逸必须被拒绝，却解析成了 %q", got)
	}
	out, err := a.readFile(`{"path":"escape/secret.txt"}`)
	if err != nil || !strings.Contains(out, "工作区") {
		t.Fatalf("read_file 也必须挡住符号链接逃逸，实际 out=%q err=%v", out, err)
	}
}

// TestApplicationOutcomesAreResultsNotFailures 应用层的结局必须作为**结果**
// 回给模型，而不是 error。
//
// 这是安全与可用性的交叉点，也是实际踩出来的：
//   - 回 error 会让上层把它当"工具故障"——给用户弹一条"没取到数据"的告警，
//     还会累加 read_file 的熔断计数。模型多试几次越界路径就能把熔断器打开，
//     连合法的读取也一起被打死。
//   - 上层的故障文案还写着"可以换一种方式重试"，等于在鼓励模型反复撞同一堵墙。
//     （实测：模型对 ../go.mod 和 C:\Windows\win.ini 各试了两次才放弃。）
//
// 所以越界、不存在、参数坏，统统走结果；只有"未知工具"这种部署问题才是故障。
func TestApplicationOutcomesAreResultsNotFailures(t *testing.T) {
	a, _, outside := newTestAgent(t)

	cases := []struct {
		name string
		call func() (string, error)
		want string
	}{
		{
			name: "越界",
			call: func() (string, error) { return a.readFile(`{"path":"../` + filepath.Base(outside) + `/x"}`) },
			want: "不要再试",
		},
		{
			name: "绝对路径",
			call: func() (string, error) { return a.readFile(`{"path":"C:\\Windows\\win.ini"}`) },
			want: "工作区",
		},
		{
			name: "文件不存在",
			call: func() (string, error) { return a.readFile(`{"path":"nope.txt"}`) },
			want: "无法访问",
		},
		{
			name: "参数坏掉",
			call: func() (string, error) { return a.readFile("不是 JSON") },
			want: "参数格式",
		},
		{
			name: "读目录",
			call: func() (string, error) { return a.readFile(`{"path":"."}`) },
			want: "list_files",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := c.call()
			if err != nil {
				t.Fatalf("应用层结局不该走 error（会被当成工具故障并累计熔断），实际 %v", err)
			}
			if !strings.Contains(out, c.want) {
				t.Errorf("结果里应含 %q，实际 %q", c.want, out)
			}
		})
	}
}

// TestExecuteUnknownTool 服务端比本地新时，要给出能定位问题的提示。
//
// 这一种才真的是故障：不是模型调错了，而是两边的工具集对不上。
func TestExecuteUnknownTool(t *testing.T) {
	a, _, _ := newTestAgent(t)

	_, err := a.execute(frameForTest(t, "write_file", `{}`))
	if err == nil {
		t.Fatal("未知工具应当是故障")
	}
	if !strings.Contains(err.Error(), "版本") {
		t.Errorf("错误信息应提示版本不一致，实际 %q", err.Error())
	}
}

// TestResolveAllowsAbsoluteInsideWorkspace 绝对路径允许，但必须在工作区内。
//
// 这是实际踩出来的：用户最自然的说法是「读一下 C:\code\proj\README.md」，
// 模型会照抄这个绝对路径。早先的版本一律拒绝绝对路径，于是功能在最常见的
// 场景下直接不可用——模型只能猜一个相对路径再试一次。
func TestResolveAllowsAbsoluteInsideWorkspace(t *testing.T) {
	a, root, outside := newTestAgent(t)

	inside := filepath.Join(root, "internal", "a.go")
	if err := os.MkdirAll(filepath.Dir(inside), 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(inside, []byte("package a"), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}

	got, err := a.resolve(inside)
	if err != nil {
		t.Fatalf("工作区内的绝对路径应当放行，实际 %v", err)
	}
	if got != inside {
		t.Fatalf("解析结果不对：%q，期望 %q", got, inside)
	}

	// 相对形式与绝对形式必须指向同一个文件
	rel, err := a.resolve(filepath.Join("internal", "a.go"))
	if err != nil {
		t.Fatalf("相对路径应当放行，实际 %v", err)
	}
	if rel != got {
		t.Fatalf("相对与绝对应指向同一处：%q vs %q", rel, got)
	}

	// 工作区外的绝对路径照样拒绝
	if _, err := a.resolve(filepath.Join(outside, "secret.txt")); err == nil {
		t.Fatal("工作区外的绝对路径必须被拒绝")
	}
	if out, err := a.readFile(`{"path":` + strconv.Quote(filepath.Join(outside, "x")) + `}`); err != nil ||
		!strings.Contains(out, "工作区") {
		t.Fatalf("read_file 也应拒绝工作区外的绝对路径，实际 out=%q err=%v", out, err)
	}
}

// TestWithin 边界：工作区自身算在内，兄弟目录不算。
func TestWithin(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "ws")

	cases := []struct {
		path string
		want bool
	}{
		{root, true},
		{filepath.Join(root, "a"), true},
		{filepath.Join(root, "a", "b"), true},
		{filepath.Join(root, ".."), false},
		{filepath.Join(root, "..", "other"), false},
		// 前缀相同但不是子目录：分隔符校验必须挡住它
		{root + "-sibling", false},
	}
	for _, c := range cases {
		if got := within(root, c.path); got != c.want {
			t.Errorf("within(%q, %q) = %v，期望 %v", root, c.path, got, c.want)
		}
	}
}

// TestReadFileTruncates 超大文件要截断并注明，而不是把上下文灌爆。
func TestReadFileTruncates(t *testing.T) {
	a, root, _ := newTestAgent(t)

	big := filepath.Join(root, "big.txt")
	if err := os.WriteFile(big, []byte(strings.Repeat("a", maxReadBytes+1024)), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}

	out, err := a.readFile(`{"path":"big.txt"}`)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if !strings.Contains(out, "截断") {
		t.Error("被截断时必须在结果里注明，模型才能决定下一步")
	}
	if len(out) > maxReadBytes*2 {
		t.Errorf("输出不该超过上限太多，实际 %d 字节", len(out))
	}
}

// TestReadFileRejectsBinary 二进制文件不该被塞进上下文。
func TestReadFileRejectsBinary(t *testing.T) {
	a, root, _ := newTestAgent(t)

	bin := filepath.Join(root, "a.bin")
	if err := os.WriteFile(bin, []byte{0x7f, 'E', 'L', 'F', 0x00, 0x01, 0x02}, 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}

	out, err := a.readFile(`{"path":"a.bin"}`)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if !strings.Contains(out, "二进制") {
		t.Errorf("二进制文件应返回提示而不是内容，实际 %q", out)
	}
}

// TestReadFileRejectsDirectory 已并入 TestApplicationOutcomesAreResultsNotFailures，
// 这里不再重复：那条用例覆盖了"读目录"这个分支。

// TestListFilesReportsWorkspace 列目录要报出工作区根，模型才知道自己在哪。
func TestListFilesReportsWorkspace(t *testing.T) {
	a, root, _ := newTestAgent(t)

	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}

	out, err := a.listFiles(`{"path":"."}`)
	if err != nil {
		t.Fatalf("列目录失败: %v", err)
	}
	if !strings.Contains(out, "工作区根") || !strings.Contains(out, "sub/") || !strings.Contains(out, "a.txt") {
		t.Errorf("目录列表不完整:\n%s", out)
	}
}

// TestExecuteUnknownTool 见上方（应用层结局 vs 故障的对照）。

// frameForTest 造一帧，避免测试里到处拼结构体字面量。
func frameForTest(t *testing.T, tool, args string) agentlocal.Frame {
	t.Helper()
	return agentlocal.Frame{Type: agentlocal.FrameCall, ID: "1", Tool: tool, Args: args}
}
