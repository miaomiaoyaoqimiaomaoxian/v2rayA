package plugin

// 框架级调试日志：记录插件系统本身的运行轨迹，与插件自己的决策日志分开。
//
// 为什么需要它：autogroup 的决策日志回答"为什么切到节点 B"，但月度稳定性
// 观测还需要回答另一类问题——事件从发布到处理完花了多久、哪个 handler 最慢、
// 插件启停的时刻和顺序、总线上谁在订阅。这些只有框架看得见，插件自己看不见。
//
// 设计约束：
//
//  1. 独立文件（plugin-system.jsonl），与 autogroup-decisions.jsonl 分开。
//     两份日志的受众不同：决策日志看"判断对不对"，这份看"系统稳不稳"。
//  2. 级别独立于全局 log-level，也独立于各插件的日志级别——观测窗口期内
//     用户调别的开关，不该影响这里的数据完整性。
//  3. 有界：16 MB × 5 份轮转，与决策日志同参数。debug 级别常开时事件量
//     仍然可控（每轮巡检约 20 条，正常使用一天几千条）。
//  4. 写失败绝不影响插件系统：日志器所有错误都吞掉，只降级。
//
// 事件分级：
//
//	debug —— 每个细节（订阅/退订、每次 handler 调用、状态持久化）
//	info  —— 生命周期事件（注册、启用、停用、事件发布摘要）
//	warn  —— 值得注意（handler panic、OnStart/OnStop 返回错误）
//	error —— 框架自身异常（日志器打不开文件不算，那只是降级）

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// 框架日志的轮转参数，与决策日志一致。
const (
	fwLogMaxBytes  = 16 << 20
	fwLogKeepFiles = 5
	fwLogFileName  = "plugin-system.jsonl"
)

// FWLevel 是框架日志的级别。
type FWLevel string

const (
	FWDebug FWLevel = "debug"
	FWInfo  FWLevel = "info"
	FWWarn  FWLevel = "warn"
	FWError FWLevel = "error"
)

var fwLevelRank = map[FWLevel]int{
	FWDebug: 0, FWInfo: 1, FWWarn: 2, FWError: 3,
}

// framework 记录器。包级单例，SetFrameworkLogDir 初始化。
type framework struct {
	mu       sync.Mutex
	minLevel FWLevel
	out      *os.File
	path     string
	size     int64
	seq      uint64
	disabled bool
}

var fw = &framework{minLevel: FWDebug}

// fwEvent 是一行 JSONL 事件。
type fwEvent struct {
	Seq   uint64 `json:"seq"`
	Time  string `json:"time"`
	Level string `json:"level"`
	// Kind 是事件类别，便于按类过滤：
	//   registry  注册/状态持久化
	//   lifecycle 启用/停用/启动/退出
	//   bus       订阅/退订/发布
	//   panic     handler panic
	//   probe     巡检相关的框架侧视角（轮次、重测调度）
	Kind    string `json:"kind"`
	Plugin  string `json:"plugin,omitempty"`
	Event   string `json:"event,omitempty"`
	Detail  string `json:"detail,omitempty"`
	Sub     string `json:"sub,omitempty"`
	Subs    int    `json:"subs,omitempty"`
	MS      int64  `json:"ms,omitempty"`
	Panics  int    `json:"panics,omitempty"`
	Err     string `json:"err,omitempty"`
}

func (f *framework) log(level FWLevel, kind, pluginName, event string, extra map[string]interface{}) {
	if f.disabled {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if fwLevelRank[level] < fwLevelRank[f.minLevel] {
		return
	}
	if f.out == nil {
		return
	}
	f.seq++
	e := fwEvent{
		Seq:   f.seq,
		Time:  time.Now().Format("2006-01-02T15:04:05.000Z07:00"),
		Level: string(level),
		Kind:  kind,
		Plugin: pluginName,
		Event: event,
	}
	// extra 里的已知键解构到列，其余丢进 detail（保持一行一事的可解析性）。
	if v, ok := extra["detail"]; ok {
		if s, ok := v.(string); ok {
			e.Detail = s
		}
	}
	if v, ok := extra["sub"]; ok {
		if s, ok := v.(string); ok {
			e.Sub = s
		}
	}
	if v, ok := extra["subs"]; ok {
		if n, ok := v.(int); ok {
			e.Subs = n
		}
	}
	if v, ok := extra["ms"]; ok {
		if n, ok := v.(int64); ok {
			e.MS = n
		}
	}
	if v, ok := extra["panics"]; ok {
		if n, ok := v.(int); ok {
			e.Panics = n
		}
	}
	if v, ok := extra["err"]; ok {
		if s, ok := v.(string); ok {
			e.Err = s
		}
	}
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	line = append(line, '\n')
	n, werr := f.out.Write(line)
	if werr != nil {
		// 写失败：关掉日志器降级，绝不反复重试拖慢主流程。
		f.closeLocked()
		f.disabled = true
		return
	}
	f.size += int64(n)
	if f.size >= fwLogMaxBytes {
		f.rotateLocked()
	}
}

func (f *framework) closeLocked() {
	if f.out != nil {
		_ = f.out.Close()
		f.out = nil
	}
}

func (f *framework) rotateLocked() {
	_ = f.out.Close()
	f.out = nil
	stamp := time.Now().Format("20060102-150405")
	_ = os.Rename(f.path, f.path+"."+stamp)
	_ = pruneFrameworkLogs(filepath.Dir(f.path))
	f.openLocked()
}

func (f *framework) openLocked() {
	_ = os.MkdirAll(filepath.Dir(f.path), 0o755)
	out, err := os.OpenFile(f.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		f.disabled = true
		return
	}
	if st, err := out.Stat(); err == nil {
		f.size = st.Size()
	}
	f.out = out
	f.disabled = false
}

// pruneFrameworkLogs 删除超出保留份数的旧轮转文件。
func pruneFrameworkLogs(dir string) error {
	matches, err := filepath.Glob(filepath.Join(dir, fwLogFileName+".*"))
	if err != nil {
		return err
	}
	if len(matches) <= fwLogKeepFiles {
		return nil
	}
	// 文件名带时间戳，字典序即时间序。
	for _, p := range matches[:len(matches)-fwLogKeepFiles] {
		_ = os.Remove(p)
	}
	return nil
}

// SetFrameworkLogDir 设置框架日志目录并打开文件。
// dir 为空或打开失败都只是禁用框架日志，不影响插件系统。
// 重复调用以最后一次为准（便于测试隔离）。
func SetFrameworkLogDir(dir string) {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	fw.closeLocked()
	if dir == "" {
		fw.disabled = true
		return
	}
	fw.path = filepath.Join(dir, fwLogFileName)
	fw.openLocked()
}

// SetFrameworkLogLevel 设置框架日志的最低级别。默认 debug。
func SetFrameworkLogLevel(l FWLevel) {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	fw.minLevel = l
}

// fwLogf 是框架内部的记录入口，fmt 风格的 detail 便捷封装。
func fwLogf(level FWLevel, kind, pluginName, event, format string, v ...interface{}) {
	fw.log(level, kind, pluginName, event, map[string]interface{}{
		"detail": fmt.Sprintf(format, v...),
	})
}

// Snapshot 在启动时记录一份环境快照，便于对齐时间轴。
func Snapshot(detail string) {
	fw.log(FWInfo, "lifecycle", "", "snapshot", map[string]interface{}{"detail": detail})
}
