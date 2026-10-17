package routefeeder

// 插件自己的 JSONL 日志 + 核心日志的域名提取。
//
// 为什么不用 autogroup 的 Logger：那是它的私有实现，跨插件复用会造成
// 耦合。这里按同样的思路实现一份轻量的（写 JSONL、轮转），事件结构
// 面向"线路学习"而不是"分组决策"。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/v2rayA/v2rayA/conf"
)

// fwLogger 是本插件的 JSONL 日志器（与 autogroup 的 Logger 同构、独立实现）。
type fwLogger struct {
	mu   sync.Mutex
	out  *os.File
	path string
	size int64
	seq  uint64
}

const rfLogMaxBytes = 8 << 20
const rfLogKeepFiles = 3

type rfEvent struct {
	Seq   uint64 `json:"seq"`
	Time  string `json:"time"`
	Level string `json:"level"`
	Msg   string `json:"msg"`
}

func (l *fwLogger) logf(level, format string, v ...interface{}) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.out == nil {
		return
	}
	l.seq++
	line, err := json.Marshal(rfEvent{
		Seq:   l.seq,
		Time:  time.Now().Format("2006-01-02T15:04:05.000Z07:00"),
		Level: level,
		Msg:   fmt.Sprintf(format, v...),
	})
	if err != nil {
		return
	}
	line = append(line, '\n')
	n, werr := l.out.Write(line)
	if werr != nil {
		l.closeLocked()
		return
	}
	l.size += int64(n)
	if l.size >= rfLogMaxBytes {
		l.rotateLocked()
	}
}

func (l *fwLogger) closeLocked() {
	if l.out != nil {
		_ = l.out.Close()
		l.out = nil
	}
}

func (l *fwLogger) rotateLocked() {
	_ = l.out.Close()
	l.out = nil
	stamp := time.Now().Format("20060102-150405")
	_ = os.Rename(l.path, l.path+"."+stamp)
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(l.path), "routefeeder.jsonl.*"))
	if len(matches) > rfLogKeepFiles {
		for _, p := range matches[:len(matches)-rfLogKeepFiles] {
			_ = os.Remove(p)
		}
	}
	l.openLocked()
}

func (l *fwLogger) openLocked() {
	out, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	if st, err := out.Stat(); err == nil {
		l.size = st.Size()
	}
	l.out = out
}

func (l *fwLogger) close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.out != nil {
		_ = l.out.Close()
		l.out = nil
	}
}

func (l *fwLogger) debug(format string, v ...interface{}) { l.logf("debug", format, v...) }
func (l *fwLogger) info(format string, v ...interface{})  { l.logf("info", format, v...) }
func (l *fwLogger) error(format string, v ...interface{}) { l.logf("error", format, v...) }

// newFWLogger 建立日志器，路径与决策日志同目录。
// 打不开文件只让日志沉默，不影响插件功能。
func newFWLogger(pluginName string) *fwLogger {
	dir := filepath.Join(conf.GetEnvironmentConfig().Config, "logs")
	_ = os.MkdirAll(dir, 0o755)
	l := &fwLogger{path: filepath.Join(dir, "routefeeder.jsonl")}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.openLocked()
	return l
}

// ---- 域名提取 ----

// logLineRe 从核心日志行提取被代理的域名：
//
//	taking detour [proxy] for [tcp:example.com:443]
//	sniffed domain: example.com
//
// 两组特征都收，去重后返回。
var (
	reDetour = regexp.MustCompile(`taking detour \[[^\]]+\] for \[tcp:([a-zA-Z0-9._-]+):\d+\]`)
	reSniff  = regexp.MustCompile(`sniffed domain: ([a-zA-Z0-9._-]+)`)
)

// recentProxyDomainsFromLog 从主日志读取最近 n 行，提取走代理的域名。
// 文件可能正被核心写入：读失败就返回空（观察窗口天然是尽力而为）。
func recentProxyDomainsFromLog(n int) []string {
	logPath := conf.GetEnvironmentConfig().LogFile
	if logPath == "" {
		return nil
	}
	lines, err := tailLines(logPath, n)
	if err != nil {
		return nil
	}
	seen := make(map[string]bool, len(lines))
	var out []string
	for _, ln := range lines {
		for _, m := range reDetour.FindAllStringSubmatch(ln, 1) {
			d := m[1]
			if !seen[d] {
				seen[d] = true
				out = append(out, d)
			}
		}
		for _, m := range reSniff.FindAllStringSubmatch(ln, 1) {
			d := m[1]
			if !seen[d] {
				seen[d] = true
				out = append(out, d)
			}
		}
	}
	// 新的在前更符合直觉，但顺序对学习逻辑不重要；这里保持出现顺序。
	return out
}

// tailLines 读文件末尾 n 行（不加载全文件）。
func tailLines(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	// 从末尾往前找 n 个换行；日志行通常 <500B，取 300B/行 的余量。
	chunk := int64(n * 300)
	if chunk > st.Size() {
		chunk = st.Size()
	}
	buf := make([]byte, chunk)
	if _, err := f.ReadAt(buf, int64(st.Size()-int64(len(buf)))); err != nil {
		return nil, err
	}
	all := strings.Split(string(buf), "\n")
	if len(all) > n {
		all = all[len(all)-n:]
	}
	return all, nil
}

var _ = sort.Strings
var _ = strconv.Itoa
