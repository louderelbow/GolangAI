package aihelper

import (
	"context"
	"deeptalk/common/resilience"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
)

// weatherClient 天气工具的外部调用必须带超时：
// Agent 的 MaxStep 只有 5，但一个挂住的 HTTP 请求能让整轮对话永远不返回。
var weatherClient = &http.Client{Timeout: 10 * time.Second}

// mustTool 注册工具时校验错误：InferTool 失败会返回 nil，
// 一旦把 nil 塞进 Agent，后续调用直接 panic。
func mustTool(t tool.InvokableTool, err error, name string) tool.InvokableTool {
	if err != nil || t == nil {
		log.Printf("[tools] register tool %s failed: %v", name, err)
	}
	return t
}

// ====================== 计算器工具 ======================

type calculatorInput struct {
	Expression string `json:"expression" jsonschema:"description=数学表达式 支持加减乘除和括号 例如 2+3*4 或 (1+2)*3"`
}

type calculatorOutput struct {
	Expression string `json:"expression"`
	Result     string `json:"result"`
}

func NewCalculatorTool() tool.InvokableTool {
	t, err := toolutils.InferTool[calculatorInput, calculatorOutput](
		"calculator",
		"计算数学表达式的结果，支持加减乘除和括号",
		func(ctx context.Context, input calculatorInput) (calculatorOutput, error) {
			result, err := evalSimple(input.Expression)
			if err != nil {
				return calculatorOutput{Expression: input.Expression, Result: "错误: " + err.Error()}, nil
			}
			return calculatorOutput{
				Expression: input.Expression,
				Result:     strconv.FormatFloat(result, 'f', -1, 64),
			}, nil
		},
	)
	return mustTool(t, err, "calculator")
}

func evalSimple(expr string) (float64, error) {
	expr = strings.ReplaceAll(expr, "×", "*")
	expr = strings.ReplaceAll(expr, "÷", "/")
	expr = strings.ReplaceAll(expr, " ", "")
	if expr == "" {
		return 0, fmt.Errorf("空表达式")
	}
	return parseAddSub(expr)
}

func parseAddSub(s string) (float64, error) {
	parens := 0
	for i := len(s) - 1; i >= 0; i-- {
		switch s[i] {
		case ')':
			parens++
		case '(':
			parens--
		case '+':
			if parens == 0 {
				left, err := parseAddSub(s[:i])
				if err != nil {
					return 0, err
				}
				right, err := parseMulDiv(s[i+1:])
				if err != nil {
					return 0, err
				}
				return left + right, nil
			}
		case '-':
			if parens == 0 && i > 0 {
				left, err := parseAddSub(s[:i])
				if err != nil {
					return 0, err
				}
				right, err := parseMulDiv(s[i+1:])
				if err != nil {
					return 0, err
				}
				return left - right, nil
			}
		}
	}
	return parseMulDiv(s)
}

func parseMulDiv(s string) (float64, error) {
	parens := 0
	for i := len(s) - 1; i >= 0; i-- {
		switch s[i] {
		case ')':
			parens++
		case '(':
			parens--
		case '*':
			if parens == 0 {
				left, err := parseMulDiv(s[:i])
				if err != nil {
					return 0, err
				}
				right, err := parseAtom(s[i+1:])
				if err != nil {
					return 0, err
				}
				return left * right, nil
			}
		case '/':
			if parens == 0 {
				left, err := parseMulDiv(s[:i])
				if err != nil {
					return 0, err
				}
				right, err := parseAtom(s[i+1:])
				if err != nil {
					return 0, err
				}
				if right == 0 {
					return 0, fmt.Errorf("除以零")
				}
				return left / right, nil
			}
		}
	}
	return parseAtom(s)
}

func parseAtom(s string) (float64, error) {
	if s == "" {
		return 0, fmt.Errorf("无效表达式")
	}
	if s[0] == '(' && s[len(s)-1] == ')' {
		return parseAddSub(s[1 : len(s)-1])
	}
	return strconv.ParseFloat(s, 64)
}

// ====================== 日期时间工具 ======================

type datetimeInput struct {
	Timezone string `json:"timezone" jsonschema:"description=时区 如 Asia/Shanghai 留空默认本地时区"`
}

type datetimeOutput struct {
	Date     string `json:"date"`
	Time     string `json:"time"`
	Weekday  string `json:"weekday"`
	UnixTime int64  `json:"unix_time"`
}

func NewDateTimeTool() tool.InvokableTool {
	t, err := toolutils.InferTool[datetimeInput, datetimeOutput](
		"datetime",
		"查询当前日期时间、星期和 Unix 时间戳",
		func(ctx context.Context, input datetimeInput) (datetimeOutput, error) {
			loc := time.Local
			if input.Timezone != "" {
				var err error
				loc, err = time.LoadLocation(input.Timezone)
				if err != nil {
					loc = time.Local
				}
			}
			now := time.Now().In(loc)
			return datetimeOutput{
				Date:     now.Format("2006-01-02"),
				Time:     now.Format("15:04:05"),
				Weekday:  now.Weekday().String(),
				UnixTime: now.Unix(),
			}, nil
		},
	)
	return mustTool(t, err, "datetime")
}

// ====================== 字数统计工具 ======================

type wordCountInput struct {
	Text string `json:"text" jsonschema:"description=需要统计的文本"`
}

type wordCountOutput struct {
	CharCount int `json:"char_count"`
	ByteLen   int `json:"byte_len"`
}

func NewWordCountTool() tool.InvokableTool {
	t, err := toolutils.InferTool[wordCountInput, wordCountOutput](
		"word_count",
		"统计文本的字符数和字节数",
		func(ctx context.Context, input wordCountInput) (wordCountOutput, error) {
			result := wordCountOutput{
				CharCount: utf8.RuneCountInString(input.Text),
				ByteLen:   len(input.Text),
			}
			log.Printf("[Tool:word_count] input=%s -> chars=%d bytes=%d", input.Text, result.CharCount, result.ByteLen)
			return result, nil
		},
	)
	return mustTool(t, err, "word_count")
}

// ====================== 天气工具 ======================

type weatherInput struct {
	City string `json:"city" jsonschema:"description=城市名称 支持中文和英文"`
}

type weatherOutput struct {
	City   string `json:"city"`
	Report string `json:"report"`
}

func NewWeatherTool() tool.InvokableTool {
	t, err := toolutils.InferTool[weatherInput, weatherOutput](
		"get_weather",
		"查询指定城市的天气信息",
		func(ctx context.Context, input weatherInput) (weatherOutput, error) {
			city := strings.TrimSpace(input.City)
			if city == "" {
				return weatherOutput{City: city, Report: "请提供城市名称"}, nil
			}
			report, err := queryWeatherDirect(ctx, city)
			if err != nil {
				return weatherOutput{City: city, Report: "天气查询失败: " + err.Error()}, nil
			}
			return weatherOutput{City: city, Report: report}, nil
		},
	)
	return mustTool(t, err, "get_weather")
}

// queryWeatherDirect 查询天气：必须带 ctx、超时、状态码检查与读取上限
func queryWeatherDirect(ctx context.Context, city string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	url := fmt.Sprintf("https://wttr.in/%s?format=%%l:+%%C+%%t+%%h+%%w&lang=zh", url.PathEscape(city))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := resilience.Do(resilience.HTTPKey("wttr-weather"), func() (*http.Response, error) {
		return weatherClient.Do(req)
	})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("weather api status %d", resp.StatusCode)
	}

	// 限制读取大小，避免第三方返回超大内容打爆内存
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", err
	}
	result := strings.Map(func(r rune) rune {
		if r > 127 && !unicode.Is(unicode.Han, r) {
			return -1
		}
		return r
	}, string(data))
	return strings.TrimSpace(result), nil
}

// ====================== 工具集合注册 ======================

func RegisterAllTools() []tool.BaseTool {
	return []tool.BaseTool{
		NewCalculatorTool(),
		NewDateTimeTool(),
		NewWordCountTool(),
		NewWeatherTool(),
	}
}
