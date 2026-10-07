package tool

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"

	einotool "github.com/cloudwego/eino/components/tool"
)

// Source 是一个外部工具来源。
//
// 目前唯一的实现是 MCP 注册表；将来接入新的工具来源（本地脚本、远程服务）
// 只需实现这个接口并通过 AddSource 挂上，Agent 侧不需要改动。
type Source interface {
	// Name 用于日志与冲突排查
	Name() string
	// Tools 返回该来源当前可用的 eino 工具
	Tools(ctx context.Context) []einotool.BaseTool
}

// Registry 汇总多个来源的工具，对 Agent 暴露统一的工具视图。
//
// 分层意图：Agent 只认识"有一批工具"，不关心工具是本地代码注册的
// 还是从 MCP 服务端拉来的。
type Registry struct {
	mu sync.RWMutex

	specs map[string]ToolSpec
	order []string

	sources []Source
}

// NewRegistry 创建一个空注册表。
func NewRegistry() *Registry {
	return &Registry{specs: make(map[string]ToolSpec)}
}

// Register 注册一个本地工具。
//
// 重名时返回错误且保留先注册的那个：静默覆盖会让"工具没生效"变得极难排查。
func (r *Registry) Register(spec ToolSpec) error {
	name := strings.TrimSpace(spec.Name)
	if name == "" {
		return fmt.Errorf("tool name is required")
	}
	if spec.Handler == nil {
		return fmt.Errorf("tool %s: handler is required", name)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.specs[name]; dup {
		return fmt.Errorf("tool %s already registered", name)
	}
	spec.Name = name
	r.specs[name] = spec
	r.order = append(r.order, name)
	return nil
}

// MustRegister 便于在 init / 装配阶段注册内置工具，重名直接返回错误由调用方处理。
func (r *Registry) MustRegister(specs ...ToolSpec) error {
	for _, s := range specs {
		if err := r.Register(s); err != nil {
			return err
		}
	}
	return nil
}

// AddSource 挂载一个外部工具来源。
func (r *Registry) AddSource(s Source) {
	if s == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sources = append(r.sources, s)
}

// Get 按名称取本地工具。
func (r *Registry) Get(name string) (ToolSpec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.specs[name]
	return s, ok
}

// Specs 按注册顺序返回全部本地工具。
func (r *Registry) Specs() []ToolSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ToolSpec, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.specs[name])
	}
	return out
}

// Names 返回全部本地工具名，便于日志与断言。
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// Tools 返回交给 ReAct Agent 的完整工具集：本地工具 + 各来源的工具。
//
// 单个来源失败不影响其他来源（MCP 注册表内部已做降级，返回空列表）。
func (r *Registry) Tools(ctx context.Context) []einotool.BaseTool {
	r.mu.RLock()
	specs := make([]ToolSpec, 0, len(r.order))
	for _, name := range r.order {
		specs = append(specs, r.specs[name])
	}
	sources := make([]Source, len(r.sources))
	copy(sources, r.sources)
	r.mu.RUnlock()

	out := make([]einotool.BaseTool, 0, len(specs))
	for _, s := range specs {
		out = append(out, s.BaseTool())
	}

	for _, src := range sources {
		got := src.Tools(ctx)
		if len(got) == 0 {
			log.Printf("[tool] source %s contributed no tools", src.Name())
			continue
		}
		out = append(out, got...)
	}
	return out
}
