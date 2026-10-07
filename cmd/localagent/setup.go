package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// ======================== 连接参数持久化 ========================
//
// 让用户只配置一次。启动参数、环境变量、上次记住的值，优先级从高到低；
// 连上之后把最终生效的一组写回磁盘，于是"以后直接双击就行"。
//
// 存的位置是用户自己的家目录，不随程序目录走——重新下载一个 exe 不该丢配置。

type agentConfig struct {
	Server    string `json:"server"`
	Token     string `json:"token"`
	Workspace string `json:"workspace"`
	// WorkspaceChosen 记录工作区是不是用户明确选过的。
	// 没选过时才在启动时弹选择框，选过就直接用——否则每次开机都弹一个框，
	// 比终端还烦。
	WorkspaceChosen bool `json:"workspaceChosen"`
}

func configDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".deeptalk"
	}
	return filepath.Join(home, ".deeptalk")
}

func configPath() string { return filepath.Join(configDir(), "agent.json") }

func loadConfig() agentConfig {
	var c agentConfig
	b, err := os.ReadFile(configPath())
	if err != nil {
		return c
	}
	if err := json.Unmarshal(b, &c); err != nil {
		log.Printf("忽略损坏的配置文件 %s: %v", configPath(), err)
		return agentConfig{}
	}
	return c
}

func saveConfig(c agentConfig) {
	if err := os.MkdirAll(configDir(), 0o700); err != nil {
		log.Printf("保存配置失败（不影响本次运行）: %v", err)
		return
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return
	}
	// 0600：文件里有 token
	if err := os.WriteFile(configPath(), b, 0o600); err != nil {
		log.Printf("保存配置失败（不影响本次运行）: %v", err)
	}
}

// ======================== 系统目录选择框 ========================

// pickFolder 在**用户自己的电脑上**弹一个系统目录选择框。
//
// 为什么要弹框而不是接受服务端给的路径：工作区就是本地 agent 能触碰的
// 边界。如果服务端能直接改它，那么一个被攻破（或只是写错）的服务端
// 就可以先把工作区设成 C:\ 再读光整块盘——路径校验形同虚设。
// 弹框把这个决定权牢牢留在用户手里，而且顺便省掉了"手敲一长串路径"。
//
// 实现上借 PowerShell 的 FolderBrowserDialog：
//   - 不引入任何 GUI 依赖（cgo / Win32 绑定都不用）
//   - -STA 是 WinForms 弹窗所必需的
//   - 初始目录通过环境变量传，避免把 Windows 路径拼进脚本字符串时的转义地狱
func pickFolder(initial string) (string, error) {
	if runtime.GOOS != "windows" {
		return "", errors.New("当前平台没有内置的目录选择框，请用 -workspace 指定目录")
	}

	const script = `Add-Type -AssemblyName System.Windows.Forms
$d = New-Object System.Windows.Forms.FolderBrowserDialog
$d.Description = '选择 DeepTalk 的工作区：模型只能读这个目录里的文件'
$d.ShowNewFolderButton = $false
if ($env:DEEPTALK_INITIAL_DIR -and (Test-Path -LiteralPath $env:DEEPTALK_INITIAL_DIR)) {
    $d.SelectedPath = $env:DEEPTALK_INITIAL_DIR
}
if ($d.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK) { Write-Output $d.SelectedPath }`

	cmd := exec.Command("powershell", "-NoProfile", "-STA", "-Command", script)
	cmd.Env = append(os.Environ(), "DEEPTALK_INITIAL_DIR="+initial)

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("打开目录选择框失败: %w", err)
	}

	// PowerShell 的输出可能带 BOM 与 CRLF
	dir := strings.TrimSpace(strings.TrimPrefix(string(out), "\ufeff"))
	if dir == "" {
		return "", errors.New("没有选择目录")
	}
	return dir, nil
}

// chooseWorkspace 走一次"用户选目录"的完整流程：弹框 + 校验 + 解析真实路径。
func chooseWorkspace(initial string) (string, error) {
	dir, err := pickFolder(initial)
	if err != nil {
		return "", err
	}
	root, err := resolveWorkspace(dir)
	if err != nil {
		return "", fmt.Errorf("%s 不能作为工作区: %w", dir, err)
	}
	return root, nil
}
