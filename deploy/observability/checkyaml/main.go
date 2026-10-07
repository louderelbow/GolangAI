// checkyaml 校验观测配置的 YAML。
//
// 为什么需要它：Grafana / Prometheus 对坏 YAML 的报错都很含糊
// （Prometheus 只会说 "error loading config"，Grafana 直接起不来），
// 而缩进错一格在编辑器里几乎看不出来。跑一下这个再起容器，省一轮排查。
//
// 用项目里已有的 YAML 库，不需要装任何新东西：
//
//	go run ./deploy/observability/checkyaml
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/goccy/go-yaml"
)

func main() {
	base := baseDir()
	files := []string{
		"prometheus.yml",
		"docker-compose.yml",
		"grafana/provisioning/datasources/prometheus.yml",
		"grafana/provisioning/dashboards/dashboards.yml",
		"targets/deeptalk.yml",
	}

	bad := 0
	for _, name := range files {
		path := filepath.Join(base, name)
		b, err := os.ReadFile(path)
		if err != nil {
			fmt.Printf("  ✗ 读不到 %s: %v\n", path, err)
			bad++
			continue
		}

		// targets/ 下的是 file_sd 格式：**顶层是数组**，不是 map。
		// 所以这里不能一律按 map 解析。
		if strings.HasPrefix(name, "targets/") {
			var list []map[string]any
			if err := yaml.Unmarshal(b, &list); err != nil {
				fmt.Printf("  ✗ YAML 错误 %s:\n    %v\n", path, err)
				bad++
				continue
			}
			fmt.Printf("  ✓ %s\n", path)
			printTargets(list, "      target")
			continue
		}

		var doc map[string]any
		if err := yaml.Unmarshal(b, &doc); err != nil {
			fmt.Printf("  ✗ YAML 错误 %s:\n    %v\n", path, err)
			bad++
			continue
		}
		fmt.Printf("  ✓ %s\n", path)

		if name == "prometheus.yml" {
			reportPrometheus(doc)
		}
	}

	if bad > 0 {
		fmt.Printf("\n%d 个文件有问题\n", bad)
		os.Exit(1)
	}
	fmt.Println("\n全部通过")
}

// baseDir 定位配置文件所在目录（deploy/observability）。
//
// 为什么不用相对路径：相对路径是相对**当前工作目录**的。而
// `go run ./deploy/observability/checkyaml` 的工作目录是项目根，
// 这时 `../prometheus.yml` 会指到项目外面去——一个"从哪跑决定了能不能用"
// 的工具，别人第一次用就会觉得它坏了。
//
// runtime.Caller 拿的是编译期嵌入的源文件路径，与工作目录无关。
func baseDir() string {
	if _, file, _, ok := runtime.Caller(0); ok {
		// main.go 在 .../deploy/observability/checkyaml/ 里，往上一层
		return filepath.Dir(filepath.Dir(file))
	}
	// 兜底：-trimpath 构建时源路径不可用，退回当前目录
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

// reportPrometheus 把抓取目标打出来。
//
// 只验"能解析"是不够的：static_configs 缩进错了照样解析成功，
// 只是 targets 变成空 —— 那种配置 Prometheus 起得来但抓不到任何东西，
// 表现和"后端没跑"一模一样，最难查。
func reportPrometheus(doc map[string]any) {
	if g, ok := doc["global"].(map[string]any); ok {
		fmt.Printf("      scrape_interval = %v\n", g["scrape_interval"])
	}

	jobs, _ := doc["scrape_configs"].([]any)
	if len(jobs) == 0 {
		fmt.Println("      ⚠ 没有任何 scrape_configs，Prometheus 会抓不到东西")
		return
	}
	for _, raw := range jobs {
		j, _ := raw.(map[string]any)
		name := fmt.Sprint(j["job_name"])

		var targets []string
		for _, sc := range asList(j["static_configs"]) {
			m, _ := sc.(map[string]any)
			for _, t := range asList(m["targets"]) {
				targets = append(targets, fmt.Sprint(t))
			}
		}

		if len(targets) > 0 {
			fmt.Printf("      job %-12v -> %v\n", name, targets)
			continue
		}

		// file_sd：目标在别的文件里，直接把它们读出来打出去。
		// 只说"用了 file_sd"没有用——真正要确认的是**文件里的地址**。
		files := []string{}
		for _, fc := range asList(j["file_sd_configs"]) {
			m, _ := fc.(map[string]any)
			for _, f := range asList(m["files"]) {
				files = append(files, fmt.Sprint(f))
			}
		}
		if len(files) == 0 {
			fmt.Printf("      ⚠ job %v 既没有 targets 也没有 file_sd_configs\n", name)
			continue
		}

		fmt.Printf("      job %-12v -> file_sd %v\n", name, files)
		for _, f := range files {
			// 配置里写的是容器路径，取 basename 回仓库里找。可能是通配符
			// （/etc/prometheus/targets/*.yml），所以要按 glob 展开，
			// 否则会把 "*.yml" 当成文件名去读，报一个假的"读不到"。
			pattern := filepath.Join(baseDir(), "targets", filepath.Base(f))
			matched, err := filepath.Glob(pattern)
			if err != nil || len(matched) == 0 {
				fmt.Printf("        ⚠ targets/ 下没有匹配 %s 的文件（还没跑过 refresh-target.sh？）\n",
					filepath.Base(f))
				continue
			}
			for _, m := range matched {
				reportTargetsFile(filepath.Base(m))
			}
		}
	}
}

// reportTargetsFile 读一个 file_sd 文件（按文件名，从仓库的 targets/ 里找），
// 把它携带的地址打出来。
//
// 为什么按文件名而不是容器里的绝对路径：容器里是
// /etc/prometheus/targets/deeptalk.yml，在本机不存在。真正要确认的是
// **文件里写的地址**，所以按 basename 回到仓库目录里读同名文件即可。
func reportTargetsFile(name string) {
	b, err := os.ReadFile(filepath.Join(baseDir(), "targets", name))
	if err != nil {
		fmt.Printf("        ⚠ 读不到 targets/%s（还没跑过 refresh-target.sh？）\n", name)
		return
	}
	var list []map[string]any
	if err := yaml.Unmarshal(b, &list); err != nil {
		fmt.Printf("        ✗ targets/%s 格式不对: %v\n", name, err)
		return
	}
	printTargets(list, "        └─ 当前目标")
}

// printTargets 打印一份 file_sd 目标列表。
func printTargets(list []map[string]any, prefix string) {
	if len(list) == 0 {
		fmt.Printf("%s: ⚠ 空文件 —— Prometheus 会认为这个 job 没有目标\n", prefix)
		return
	}
	for _, item := range list {
		var ts []string
		for _, t := range asList(item["targets"]) {
			ts = append(ts, fmt.Sprint(t))
		}
		fmt.Printf("%s: %v\n", prefix, ts)
	}
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}
