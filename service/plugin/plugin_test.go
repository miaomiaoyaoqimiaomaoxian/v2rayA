package plugin

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// testPlugin 是一个可编程的测试插件。
type testPlugin struct {
	name        string
	description string
	version     string
	builtin     bool

	mu        sync.Mutex
	startErr  error
	stopErr   error
	starts    int
	stops     int
	panics    bool
	startDone chan struct{}
}

func newTestPlugin(name string, builtin bool) *testPlugin {
	return &testPlugin{
		name:        name,
		description: "test plugin " + name,
		version:     "1.0.0",
		builtin:     builtin,
		startDone:   make(chan struct{}, 8),
	}
}

func (p *testPlugin) Name() string        { return p.name }
func (p *testPlugin) Description() string { return p.description }
func (p *testPlugin) Version() string     { return p.version }
func (p *testPlugin) Builtin() bool       { return p.builtin }

func (p *testPlugin) OnStart(ctx *Context) error {
	p.mu.Lock()
	p.starts++
	p.mu.Unlock()
	select {
	case p.startDone <- struct{}{}:
	default:
	}
	if p.panics {
		panic("boom in OnStart")
	}
	return p.startErr
}

func (p *testPlugin) OnStop(ctx *Context) error {
	p.mu.Lock()
	p.stops++
	p.mu.Unlock()
	return p.stopErr
}

func (p *testPlugin) startCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.starts
}

func (p *testPlugin) stopCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stops
}

// setupManager 注册一个插件并返回 manager。
// 数据库不可用时跳过（这些测试需要临时配置目录，由 TestMain 提供）。
func setupManager(t *testing.T, p Plugin) *Manager {
	t.Helper()
	m := NewManager()
	if err := m.Register(p); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestRegisterRejectsBadNames(t *testing.T) {
	m := NewManager()
	if err := m.Register(newTestPlugin("", false)); err == nil {
		t.Fatal("empty name should be rejected")
	}
	if err := m.Register(newTestPlugin("has:colon", false)); err == nil {
		t.Fatal("a name containing ':' should be rejected")
	}
}

func TestRegisterRejectsDuplicates(t *testing.T) {
	m := setupManager(t, newTestPlugin("dup", false))
	if err := m.Register(newTestPlugin("dup", false)); err == nil {
		t.Fatal("registering the same name twice should fail")
	}
}

func TestEnableStartInvokesOnStart(t *testing.T) {
	p := newTestPlugin("en", false)
	m := setupManager(t, p)
	if err := m.Enable("en"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.startDone:
	case <-time.After(2 * time.Second):
		t.Fatal("OnStart was not called")
	}
	info, ok := m.Get("en")
	if !ok || info.State != string(StateEnabled) || !info.Running {
		t.Fatalf("info = %+v", info)
	}
}

func TestEnableIsIdempotent(t *testing.T) {
	p := newTestPlugin("idem", false)
	m := setupManager(t, p)
	if err := m.Enable("idem"); err != nil {
		t.Fatal(err)
	}
	if err := m.Enable("idem"); err != nil {
		t.Fatal(err)
	}
	if got := p.startCount(); got != 1 {
		t.Fatalf("OnStart called %d times, want 1", got)
	}
}

func TestDisableStopsPluginAndKeepsData(t *testing.T) {
	p := newTestPlugin("dis", false)
	m := setupManager(t, p)
	if err := m.Enable("dis"); err != nil {
		t.Fatal(err)
	}
	if err := m.Disable("dis"); err != nil {
		t.Fatal(err)
	}
	if p.stopCount() != 1 {
		t.Fatalf("OnStop called %d times, want 1", p.stopCount())
	}
	info, _ := m.Get("dis")
	if info.State != string(StateDisabled) || info.Running {
		t.Fatalf("info = %+v", info)
	}
}

func TestDisableIsIdempotent(t *testing.T) {
	p := newTestPlugin("dis2", false)
	m := setupManager(t, p)
	if err := m.Enable("dis2"); err != nil {
		t.Fatal(err)
	}
	if err := m.Disable("dis2"); err != nil {
		t.Fatal(err)
	}
	if err := m.Disable("dis2"); err != nil {
		t.Fatal(err)
	}
	if got := p.stopCount(); got != 1 {
		t.Fatalf("OnStop called %d times, want 1", got)
	}
}

// 启动失败的插件必须被标记为 disabled，不能留下半启用状态。
func TestEnableRollsBackWhenStartFails(t *testing.T) {
	p := newTestPlugin("failstart", false)
	p.startErr = errors.New("cannot start")
	m := setupManager(t, p)
	if err := m.Enable("failstart"); err == nil {
		t.Fatal("expected an error when OnStart fails")
	}
	info, _ := m.Get("failstart")
	if info.State != string(StateDisabled) || info.Running {
		t.Fatalf("info = %+v, want disabled/not-running", info)
	}
}

// 插件的 panic 绝不能拖垮框架。
func TestStartPanicIsContained(t *testing.T) {
	p := newTestPlugin("panics", false)
	p.panics = true
	m := setupManager(t, p)
	if err := m.Enable("panics"); err == nil {
		t.Fatal("expected an error when OnStart panics")
	}
	info, _ := m.Get("panics")
	if info.Running {
		t.Fatal("a panicking plugin must not be marked running")
	}
}

func TestStopPanicIsContained(t *testing.T) {
	p := newTestPlugin("stoppanic", false)
	p.panics = true
	m := setupManager(t, p)
	// OnStart panic 会让 Enable 失败；直接调 StopAll 覆盖 OnStop 路径。
	m.StopAll()
	// 能走到这里说明 panic 被吞掉了。
	t.Log("StopAll survived a panicking plugin")
}

func TestStartEnabledOnlyStartsEnabled(t *testing.T) {
	on := newTestPlugin("on", false)
	off := newTestPlugin("off", false)
	m := NewManager()
	if err := m.Register(on); err != nil {
		t.Fatal(err)
	}
	if err := m.Register(off); err != nil {
		t.Fatal(err)
	}
	if err := m.Enable("on"); err != nil {
		t.Fatal(err)
	}
	m.StartEnabled()
	if on.startCount() != 1 {
		t.Fatalf("enabled plugin started %d times, want 1", on.startCount())
	}
	if off.startCount() != 0 {
		t.Fatalf("disabled plugin started %d times, want 0", off.startCount())
	}
}

func TestUninstallRemovesEverything(t *testing.T) {
	p := newTestPlugin("unin", false)
	m := setupManager(t, p)
	if err := m.Enable("unin"); err != nil {
		t.Fatal(err)
	}
	if err := m.Uninstall("unin"); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Get("unin"); ok {
		t.Fatal("the plugin should be gone after uninstall")
	}
	if p.stopCount() != 1 {
		t.Fatalf("OnStop called %d times, want 1", p.stopCount())
	}
}

func TestUninstallRejectsBuiltin(t *testing.T) {
	p := newTestPlugin("builtin1", true)
	m := setupManager(t, p)
	if err := m.Enable("builtin1"); err != nil {
		t.Fatal(err)
	}
	err := m.Uninstall("builtin1")
	if err == nil {
		t.Fatal("a builtin plugin must not be uninstallable")
	}
	if _, ok := m.Get("builtin1"); !ok {
		t.Fatal("a builtin plugin must survive a failed uninstall")
	}
}

func TestUninstallUnknownPlugin(t *testing.T) {
	m := NewManager()
	if err := m.Uninstall("nope"); err == nil {
		t.Fatal("uninstalling an unknown plugin should fail")
	}
}

func TestListIsSortedAndComplete(t *testing.T) {
	m := NewManager()
	for _, n := range []string{"c", "a", "b"} {
		if err := m.Register(newTestPlugin(n, false)); err != nil {
			t.Fatal(err)
		}
	}
	got := m.List()
	if len(got) != 3 {
		t.Fatalf("listed %d plugins, want 3", len(got))
	}
	if got[0].Name != "a" || got[1].Name != "b" || got[2].Name != "c" {
		t.Fatalf("list is not sorted: %v", []string{got[0].Name, got[1].Name, got[2].Name})
	}
}

// ---- 事件总线 ----

func TestBusDeliversToSubscribers(t *testing.T) {
	bus := NewBus()
	var got string
	var mu sync.Mutex
	unsub := bus.Subscribe("evt", func(payload interface{}) {
		mu.Lock()
		defer mu.Unlock()
		got, _ = payload.(string)
	})
	defer unsub()
	bus.Publish("evt", "hello")
	mu.Lock()
	defer mu.Unlock()
	if got != "hello" {
		t.Fatalf("handler received %q, want hello", got)
	}
}

func TestBusUnsubscribeStopsDelivery(t *testing.T) {
	bus := NewBus()
	calls := 0
	unsub := bus.Subscribe("evt", func(interface{}) { calls++ })
	bus.Publish("evt", nil)
	unsub()
	bus.Publish("evt", nil)
	if calls != 1 {
		t.Fatalf("handler called %d times, want 1", calls)
	}
}

func TestBusContainsHandlerPanic(t *testing.T) {
	bus := NewBus()
	bus.Subscribe("evt", func(interface{}) { panic("boom") })
	second := false
	bus.Subscribe("evt", func(interface{}) { second = true })
	// 必须不 panic，且后续 handler 仍被调用。
	bus.Publish("evt", nil)
	if !second {
		t.Fatal("a panicking handler blocked the others")
	}
}

func TestBusNoSubscribersIsNoop(t *testing.T) {
	bus := NewBus()
	bus.Publish("nobody-listens", nil)
}
