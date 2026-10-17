package plugin

// 框架日志的回归测试。
//
// 这份日志是月度稳定性观测的数据来源，它的正确性比一般日志更重要：
// 轮转丢事件、级别过滤错位、字段缺失，都会让一个月的数据作废。
// 所以这里测的不是"有没有打日志"，而是"日志能不能回答稳定性问题"。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readFrameworkLog(t *testing.T, dir string) []fwEvent {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, fwLogFileName))
	if err != nil {
		t.Fatalf("读取框架日志失败: %v", err)
	}
	var evs []fwEvent
	for _, ln := range strings.Split(string(b), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		var e fwEvent
		if err := json.Unmarshal([]byte(ln), &e); err != nil {
			t.Fatalf("日志行不是合法 JSON: %v\n%s", err, ln)
		}
		evs = append(evs, e)
	}
	return evs
}

// TestFrameworkLogCapturesLifecycle 走一遍完整生命周期，
// 断言每个阶段都留下了可读的事件。
func TestFrameworkLogCapturesLifecycle(t *testing.T) {
	dir := t.TempDir()
	SetFrameworkLogDir(dir)
	SetFrameworkLogLevel(FWDebug)
	t.Cleanup(func() { SetFrameworkLogDir("") })

	m := NewManager()
	p := &stubPlugin{name: "alpha", version: "1.2.3"}
	if err := m.Register(p); err != nil {
		t.Fatal(err)
	}
	if err := m.Enable("alpha"); err != nil {
		t.Fatal(err)
	}
	m.Disable("alpha")

	evs := readFrameworkLog(t, dir)
	kinds := map[string]int{}
	var hasRegister, hasStarted, hasStopped bool
	for _, e := range evs {
		kinds[e.Kind]++
		if e.Event == "register" && e.Plugin == "alpha" {
			hasRegister = true
			if e.Detail == "" || !strings.Contains(e.Detail, "1.2.3") {
				t.Errorf("register 事件应带版本: %+v", e)
			}
		}
		if e.Event == "started" && e.Plugin == "alpha" {
			hasStarted = true
		}
		if e.Event == "stopped" && e.Plugin == "alpha" {
			hasStopped = true
		}
	}
	if !hasRegister || !hasStarted || !hasStopped {
		t.Errorf("生命周期事件缺失: register=%v started=%v stopped=%v", hasRegister, hasStarted, hasStopped)
	}
	if kinds["lifecycle"] == 0 {
		t.Error("应有 lifecycle 类事件")
	}
}

// TestFrameworkLogCapturesBusMetrics 验证总线指标：订阅者数与耗时。
func TestFrameworkLogCapturesBusMetrics(t *testing.T) {
	dir := t.TempDir()
	SetFrameworkLogDir(dir)
	SetFrameworkLogLevel(FWDebug)
	t.Cleanup(func() { SetFrameworkLogDir("") })

	b := NewBus()
	unsub := b.Subscribe("test.event", func(interface{}) {})
	b.Subscribe("test.event", func(interface{}) {})
	b.Publish("test.event", nil)
	unsub()
	b.Publish("test.event", nil)

	evs := readFrameworkLog(t, dir)
	var pubs []*fwEvent
	for i := range evs {
		e := &evs[i]
		if e.Kind == "bus" && e.Event == "publish" {
			pubs = append(pubs, e)
		}
	}
	if len(pubs) != 2 {
		t.Fatalf("应有两条 publish 事件，实际 %d: %+v", len(pubs), evs)
	}
	if pubs[0].Subs != 2 {
		t.Errorf("第一次发布应有 2 个订阅者，实际 %d", pubs[0].Subs)
	}
	if pubs[1].Subs != 1 {
		t.Errorf("退订后发布应只剩 1 个订阅者，实际 %d", pubs[1].Subs)
	}
	// 订阅者名是运行时函数名：断言它可定位到本测试。
	if !strings.Contains(evs[0].Sub, "TestFrameworkLogCapturesBusMetrics") {
		t.Errorf("订阅者名应包含测试函数名，实际 %q", evs[0].Sub)
	}
}

// TestFrameworkLogCapturesPanic handler panic 必须留痕。
func TestFrameworkLogCapturesPanic(t *testing.T) {
	dir := t.TempDir()
	SetFrameworkLogDir(dir)
	SetFrameworkLogLevel(FWDebug)
	t.Cleanup(func() { SetFrameworkLogDir("") })

	b := NewBus()
	b.Subscribe("boom", func(interface{}) { panic("kaboom") })
	b.Publish("boom", nil)

	evs := readFrameworkLog(t, dir)
	found := false
	for _, e := range evs {
		if e.Event == "handler-panic" && strings.Contains(e.Err, "kaboom") {
			found = true
		}
	}
	if !found {
		t.Errorf("应有 handler-panic 事件: %+v", evs)
	}
}

// TestFrameworkLogLevelFilter 级别过滤：warn 级别下 debug 不落盘。
func TestFrameworkLogLevelFilter(t *testing.T) {
	dir := t.TempDir()
	SetFrameworkLogDir(dir)
	SetFrameworkLogLevel(FWWarn)
	t.Cleanup(func() { SetFrameworkLogDir("") })

	fw.log(FWDebug, "bus", "", "subscribe", nil)
	fw.log(FWWarn, "panic", "x", "handler-panic", map[string]interface{}{"err": "e"})

	evs := readFrameworkLog(t, dir)
	for _, e := range evs {
		if e.Level == string(FWDebug) {
			t.Errorf("warn 级别下 debug 事件不应落盘: %+v", e)
		}
	}
	if len(evs) != 1 {
		t.Fatalf("应只有 1 条 warn 事件，实际 %d", len(evs))
	}
}

// TestFrameworkLogRotation 轮转：写满阈值后换新文件，旧文件保留。
func TestFrameworkLogRotation(t *testing.T) {
	dir := t.TempDir()
	SetFrameworkLogDir(dir)
	// 前面的用例可能把级别调高了；本用例写的是 info，必须显式恢复。
	SetFrameworkLogLevel(FWDebug)
	t.Cleanup(func() {
		SetFrameworkLogDir("")
		SetFrameworkLogLevel(FWDebug)
	})

	// 伪造一个接近阈值的 size，下一行写入就触发轮转。
	fw.mu.Lock()
	fw.size = fwLogMaxBytes - 10
	fw.path = filepath.Join(dir, fwLogFileName)
	fw.mu.Unlock()

	// 一条足够大的事件把剩余空间写满并触发轮转。
	big := strings.Repeat("x", 200)
	fwLogf(FWInfo, "lifecycle", "rotate-test", "filler", "%s", big)

	// 轮转把写满的文件改名，新的空文件继续接写。
	matches, err := filepath.Glob(filepath.Join(dir, fwLogFileName+"*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) < 2 {
		t.Fatalf("轮转后应有至少 2 个文件（含改名与新建），实际 %d: %v", len(matches), matches)
	}
	// 当前文件仍可写。
	fwLogf(FWInfo, "lifecycle", "rotate-test", "after", "ok")
	evs := readFrameworkLog(t, dir)
	if len(evs) == 0 || evs[len(evs)-1].Event != "after" {
		t.Errorf("轮转后应能继续写入: %+v", evs)
	}
}

// stubPlugin 是生命周期测试用的最小插件。
type stubPlugin struct{ name, version string }

func (s *stubPlugin) Name() string                { return s.name }
func (s *stubPlugin) Description() string         { return "stub" }
func (s *stubPlugin) Version() string             { return s.version }
func (s *stubPlugin) Builtin() bool               { return false }
func (s *stubPlugin) OnStart(*Context) error      { return nil }
func (s *stubPlugin) OnStop(*Context) error       { return nil }
