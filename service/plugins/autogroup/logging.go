package autogroup

// 决策日志：把插件的每一次判断都留下可复核的痕迹。
//
// 设计目标（按"事后能否复盘"排序）：
//
//  1. 能回答"为什么"。只记"切到了节点 B"没用——要同时记下当时的候选集、
//     每个节点的健康快照、缓存新鲜度、以及被排除的那一个为什么被排除。
//     有了这些，任何一次切换都能在事后被独立复算。
//  2. 机器可读。JSONL，一行一事件，便于用脚本聚合。
//  3. 不淹没主日志。独立文件，与 v2rayA 主日志分离；主日志里的插件输出
//     降为一行摘要，需要细节时再来看这个文件。
//  4. 有界增长。按大小轮转 + 保留份数，避免调试级日志把磁盘写满。
//
// 事件分级：
//   debug —— 每一步的中间状态（缓存读取、探测原始结果、排序候选）
//   info  —— 一次完整决策（挑选、切换、降级、报错）
//   warn  —— 值得注意但不是错误（回退到探测、跳过过期缓存、pinned 全灭）
//   error —— 决策失败或状态不一致
//
// 单独定义级别而不复用 v2rayA 的全局级别，是为了让本插件的调试日志
// 不受全局 log-level 影响：用户把全局调到 info，不代表自动分组的
// 决策痕迹也会消失——那些痕迹恰恰是排查问题时要看的。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/v2rayA/v2rayA/db/configure"
	"github.com/v2rayA/v2rayA/pkg/util/log"
)

// Level 是本插件日志的级别。
type Level string

const (
	LevelDebug Level = "debug"
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

var levelRank = map[Level]int{
	LevelDebug: 0,
	LevelInfo:  1,
	LevelWarn:  2,
	LevelError: 3,
}

// 轮转参数。调试级日志量比 info 大两个数量级，所以：
// 单文件 16 MB，保留 5 份 = 最多 80 MB。
const (
	logMaxBytes  = 16 << 20
	logKeepFiles = 5
	logFileName  = "autogroup-decisions.jsonl"
)

// Logger 是插件的决策日志记录器。
//
// 线程安全；所有写盘失败都只影响日志本身，不影响代理流程。
type Logger struct {
	mu sync.Mutex

	// minLevel 过滤掉低于此级别的记录。
	minLevel Level
	// out 是当前写入的文件。
	out *os.File
	// path 是当前文件路径，轮转时递增后缀。
	path string
	// dir 是日志目录。
	dir string
	// size 是当前已写字节数。
	size int64
	// seq 单调递增的事件序号，便于把一次决策的多条记录串起来。
	seq uint64
	// disabled 为 true 时所有写入都是空操作（用于测试与降级）。
	disabled bool
}

// NewLogger 在 dir 下创建一个决策日志记录器。
// dir 为空或无法创建时返回 disabled 的记录器——记不了日志不能让插件起不来。
func NewLogger(dir string) *Logger {
	l := &Logger{minLevel: LevelDebug, dir: dir, disabled: true}
	if dir == "" {
		return l
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return l
	}
	if err := l.open(filepath.Join(dir, logFileName), 0); err != nil {
		return l
	}
	l.disabled = false
	return l
}

// SetLevel 设置最低记录级别。
func (l *Logger) SetLevel(min Level) {
	if _, ok := levelRank[min]; !ok {
		return
	}
	l.mu.Lock()
	l.minLevel = min
	l.mu.Unlock()
}

// Path 返回当前日志文件路径，disabled 时返回空。
func (l *Logger) Path() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.disabled {
		return ""
	}
	return l.path
}

// open 打开（或创建）日志文件。suffix 用于轮转。
func (l *Logger) open(path string, suffix int) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	var size int64
	if st, err := f.Stat(); err == nil {
		size = st.Size()
	}
	l.out = f
	l.path = path
	l.size = size
	_ = suffix
	return nil
}

// rotate 轮转：关掉当前文件，把旧文件依次改名，写入新文件。
func (l *Logger) rotate() {
	if l.out != nil {
		_ = l.out.Close()
		l.out = nil
	}
	// 删掉最老的
	oldest := filepath.Join(l.dir, fmt.Sprintf("%s.%d", logFileName, logKeepFiles))
	_ = os.Remove(oldest)
	// 依次后移
	for i := logKeepFiles - 1; i >= 1; i-- {
		from := filepath.Join(l.dir, fmt.Sprintf("%s.%d", logFileName, i))
		to := filepath.Join(l.dir, fmt.Sprintf("%s.%d", logFileName, i+1))
		if _, err := os.Stat(from); err == nil {
			_ = os.Rename(from, to)
		}
	}
	_ = os.Rename(l.path, filepath.Join(l.dir, logFileName+".1"))
	_ = l.open(filepath.Join(l.dir, logFileName), 1)
}

// Event 是一条决策记录。
//
// 用扁平的结构而不是嵌套 map，是为了让人和 jq 都能直接读字段。
type Event struct {
	TS   string `json:"ts"`
	Seq  uint64 `json:"seq"`
	Msg  string `json:"msg"`
	// Outbound 是相关分组（若与分组相关）。
	Outbound string `json:"outbound,omitempty"`
	// Node / From / To 是涉及节点的稳定 key（TYPE/ID/Sub）。
	Node string `json:"node,omitempty"`
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	// Decision 是结论，例如 "pick" / "switch" / "fallback-to-probe" /
	// "all-unreachable" / "noop"。这是事后聚合的主键。
	Decision string `json:"decision,omitempty"`
	// Reason 是为什么（人类可读）。
	Reason string `json:"reason,omitempty"`
	// Level 是 debug/info/warn/error。
	Level Level `json:"level"`
	// 下面是决策的输入快照，缺省即不适用。
	Mode          string `json:"mode,omitempty"`
	FailThreshold int    `json:"failThreshold,omitempty"`
	MaxSwitches   int    `json:"maxSwitches,omitempty"`
	Fails         int    `json:"fails,omitempty"`
	SwitchCount   int    `json:"switchCount,omitempty"`
	// Candidates 是候选集及其当时的健康快照，按候选集顺序。
	Candidates []NodeSnapshot `json:"candidates,omitempty"`
	// CacheFresh 报告本次决策时有多少候选的健康记录是新鲜的。
	CacheFresh int `json:"cacheFresh,omitempty"`
	// Err 是失败原因。
	Err string `json:"err,omitempty"`
	// DurationMs 是本决策耗时。
	DurationMs int64 `json:"durationMs,omitempty"`
	// Caller 便于定位埋点位置。
	Caller string `json:"caller,omitempty"`
}

// NodeSnapshot 是单个节点在某一时刻的健康快照。
type NodeSnapshot struct {
	Node      string `json:"node"`
	Alive     bool   `json:"alive"`
	Fails     int    `json:"fails"`
	LatencyMs int64  `json:"latencyMs,omitempty"`
	AgeSec    int64  `json:"ageSec,omitempty"` // 距上次探测的秒数
	Fresh     bool   `json:"fresh"`
	Err       string `json:"err,omitempty"`
}

// SnapshotCandidates 生成候选集的健康快照。
// 这是复盘的关键：有了它，任何一次挑选都可以离线重算。
func SnapshotCandidates(refs []configure.NodeRef, health map[string]configure.NodeHealth, now int64) []NodeSnapshot {
	out := make([]NodeSnapshot, 0, len(refs))
	for _, r := range refs {
		h, ok := health[r.Key()]
		s := NodeSnapshot{Node: r.Key(), Alive: h.Alive, Fails: h.Fails, LatencyMs: h.LatencyMs, Err: h.LastError}
		if ok && h.CheckedAt > 0 {
			s.AgeSec = now - h.CheckedAt
			s.Fresh = h.Fresh(now)
		}
		out = append(out, s)
	}
	return out
}

// CountFresh 统计快照里有多少条是新鲜的。
func CountFresh(snaps []NodeSnapshot) int {
	n := 0
	for _, s := range snaps {
		if s.Fresh {
			n++
		}
	}
	return n
}

// Log 写入一条记录。level 低于 minLevel 时被丢弃。
func (l *Logger) Log(ev Event) {
	if l == nil {
		return
	}
	l.mu.Lock()
	if l.disabled || ev.Level == "" {
		l.mu.Unlock()
		return
	}
	if levelRank[ev.Level] < levelRank[l.minLevel] {
		l.mu.Unlock()
		return
	}
	l.seq++
	ev.Seq = l.seq
	ev.TS = time.Now().Format(time.RFC3339Nano)
	if ev.Caller == "" {
		ev.Caller = caller()
	}
	b, err := json.Marshal(ev)
	if err != nil {
		l.mu.Unlock()
		return
	}
	b = append(b, '\n')
	if l.out != nil {
		if l.size+int64(len(b)) > logMaxBytes {
			l.rotate()
		}
		if l.out != nil {
			if n, err := l.out.Write(b); err == nil {
				l.size += int64(n)
			}
		}
	}
	l.mu.Unlock()

	// 摘要同步进主日志：一眼能看见发生了什么，细节在专用文件里。
	summary := fmt.Sprintf("[%s] %s", ev.Decision, ev.Msg)
	switch ev.Level {
	case LevelError:
		pluginLogError(summary)
	case LevelWarn:
		pluginLogWarn(summary)
	default:
		pluginLogInfo(summary)
	}
}

// Debug / Info / Warn / Error 是 Level 的便捷包装。
func (l *Logger) Debug(ev Event)  { ev.Level = LevelDebug; l.Log(ev) }
func (l *Logger) Info(ev Event)   { ev.Level = LevelInfo; l.Log(ev) }
func (l *Logger) Warn(ev Event)   { ev.Level = LevelWarn; l.Log(ev) }
func (l *Logger) Error(ev Event)  { ev.Level = LevelError; l.Log(ev) }
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.out != nil {
		err := l.out.Close()
		l.out = nil
		return err
	}
	return nil
}

// caller 返回埋点位置，形如 "autopilot.go:123"。
func caller() string {
	_, file, line, ok := runtime.Caller(3)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%s:%d", filepath.Base(file), line)
}

// DescribeHealth 把健康缓存渲染成一行紧凑文本，用于主日志摘要。
// 形如：3/8 可用 (最快 88ms) 或 无缓存
func DescribeHealth(alive, total int, bestLatency int64) string {
	if total == 0 {
		return "无候选"
	}
	if alive == 0 {
		return fmt.Sprintf("0/%d 可用", total)
	}
	if bestLatency > 0 {
		return fmt.Sprintf("%d/%d 可用 (最快 %dms)", alive, total, bestLatency)
	}
	return fmt.Sprintf("%d/%d 可用", alive, total)
}

// SortedNodeKeys 返回排序后的节点 key，便于在日志里稳定输出。
func SortedNodeKeys(m map[string]configure.NodeHealth) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ParseLogLevel 从用户输入解析级别。
// 非法值返回 def；def 传空串时返回空串，用于"校验并拒绝"这类场景。
func ParseLogLevel(s string, def Level) Level {
	t := strings.ToLower(strings.TrimSpace(s))
	switch Level(t) {
	case LevelDebug, LevelInfo, LevelWarn, LevelError:
		return Level(t)
	default:
		return def
	}
}

// LogLevelSetting 是持久化的日志配置（存在插件私有 bucket）。
type LogLevelSetting struct {
	// Level 是决策日志的最低级别。
	Level string `json:"level"`
	// DecisionKeepDays 控制日志保留天数，0 表示只按份数。
	DecisionKeepDays int `json:"decisionKeepDays,omitempty"`
}

// 分隔主日志里插件输出的辅助函数。默认走 v2rayA 的包级日志器；
// 测试可替换它们，从而不污染真实日志。
var (
	pluginLogInfo  = func(s string) { log.Info("autogroup: %v", s) }
	pluginLogWarn  = func(s string) { log.Warn("autogroup: %v", s) }
	pluginLogError = func(s string) { log.Error("autogroup: %v", s) }
)

// SetMainLogHook 让测试捕获主日志摘要，不污染真实日志。
func SetMainLogHook(info, warn, err func(string)) {
	pluginLogInfo, pluginLogWarn, pluginLogError = info, warn, err
}