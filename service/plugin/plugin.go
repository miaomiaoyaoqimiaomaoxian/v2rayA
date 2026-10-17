// Package plugin 是 v2rayA 的插件框架。
//
// 设计目标：
//
//  1. 衔接器：插件与核心通过窄接口交互，核心不认识任何具体插件。
//  2. 随装随用：启用即注册，卸载即从所有索引移除，无残留状态。
//  3. 随时可卸载：卸载只依赖插件自己声明的清理动作，不影响原功能。
//  4. 不影响原功能：核心侧的调用点全部是"有插件就通知，没有就跳过"，
//     删掉本包的所有调用方，v2rayA 的行为与加插件前完全一致。
//
// 生命周期：
//
//	register(编译期内置) -> enable(写库标记启用) -> start(调用 OnStart)
//	                    -> disable(调用 OnStop，保留数据) -> uninstall(删数据)
//
// 并发：Manager 内部用 RWMutex 保护索引；插件自身若持有状态，由其自行
// 保证并发安全，框架不替插件加锁。
package plugin

import (
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/v2rayA/v2rayA/db"
	"github.com/v2rayA/v2rayA/pkg/util/log"
)

// PluginState 是插件的持久化状态。
type PluginState string

const (
	// StateEnabled 表示插件已启用（可能尚未 start，见下）。
	StateEnabled PluginState = "enabled"
	// StateDisabled 表示插件已安装但停用：不接收事件，但配置数据保留。
	StateDisabled PluginState = "disabled"
)

// Builtin 表示该插件编译进 v2rayA 主程序，不可从界面上"卸载"——
// 只能停用。这是内置插件与外部插件的区别。
const Builtin = true

// Plugin 是一个插件必须实现的接口。
//
// 除 Name/Description 外，其余方法都可以是空实现：框架对 nil 方法做判空，
// 因此一个只想来"看一眼事件"的插件只需实现 Name。
type Plugin interface {
	// Name 是插件的唯一标识，也是它在数据库中存储状态用的键。
	// 必须非空且不含 ':'（与 db bucket 的键分隔符冲突）。
	Name() string
	// Description 是一句人类可读的说明，展示在插件管理页。
	Description() string
	// Version 是插件版本号，展示用。
	Version() string
	// Builtin 报告该插件是否编译进主程序。内置插件不允许卸载。
	Builtin() bool

	// OnStart 在插件被启用、或 v2rayA 启动时若插件处于启用态，各调用一次。
	// 返回错误会导致启用失败，但不会影响其他插件。
	OnStart(ctx *Context) error
	// OnStop 在插件停用、v2rayA 退出、或插件被卸载前调用。
	// 必须能安全地重复调用。
	OnStop(ctx *Context) error
}

// Context 是框架递给插件的上下文，提供插件所需的最小能力集。
//
// 刻意保持窄：插件拿不到 ProcessManager、拿不到 gin engine，只能通过
// 这里的方法与核心交互。这样插件的破坏半径被限制在框架给出的能力内。
type Context struct {
	// PluginName 是插件名，日志与错误信息里带上它便于定位。
	PluginName string
	// Events 是事件总线，插件可订阅核心事件。
	Events *Bus
	// settingsBucket 是插件私有配置存储（db 中独立 bucket）。
	settingsBucket string
}

// Logger 返回带插件名前缀的日志器。
// v2rayA 的 log 包是包级函数，这里包一层让插件的日志可被 grep 出来。
func (c *Context) Logger() *PluginLogger {
	return &PluginLogger{Prefix: "plugin/" + c.PluginName}
}

// PluginLogger 是带固定前缀的日志器。
// Prefix 导出，便于插件侧直接构造（例如控制器想用自己的前缀）。
type PluginLogger struct {
	Prefix string
}

func (l *PluginLogger) Alert(format string, v ...interface{}) { log.Alert(l.Prefix+": "+format, v...) }
func (l *PluginLogger) Error(format string, v ...interface{}) { log.Error(l.Prefix+": "+format, v...) }
func (l *PluginLogger) Warn(format string, v ...interface{})  { log.Warn(l.Prefix+": "+format, v...) }
func (l *PluginLogger) Info(format string, v ...interface{})  { log.Info(l.Prefix+": "+format, v...) }
func (l *PluginLogger) Debug(format string, v ...interface{}) { log.Debug(l.Prefix+": "+format, v...) }
func (l *PluginLogger) Trace(format string, v ...interface{}) { log.Trace(l.Prefix+": "+format, v...) }

// Emit 让插件向总线发布事件（例如"我已切换了节点"）。
func (c *Context) Emit(name string, payload interface{}) {
	if c.Events == nil {
		return
	}
	c.Events.Publish(name, payload)
}

// ---- 事件总线 ----

// EventHandler 处理一个事件。handler 的 panic 会被 Bus 捕获并记录，
// 不会导致 v2rayA 崩溃——一个插件的 bug 不该拖垮整个服务。
type EventHandler func(payload interface{})

// Bus 是进程内同步事件总线。
type Bus struct {
	mu       sync.RWMutex
	handlers map[string][]EventHandler
}

// NewBus 构造一个空总线。
func NewBus() *Bus {
	return &Bus{handlers: make(map[string][]EventHandler)}
}

// Subscribe 注册一个事件处理器。同一事件可有多个处理器，按注册顺序调用。
// 返回取消订阅函数。
func (b *Bus) Subscribe(name string, h EventHandler) func() {
	if b == nil || h == nil {
		return func() {}
	}
	// 记订阅者名字，日志里才看得见"谁在听"。handler 是闭包，直接打印
	// 没有意义，所以用调用点传入的名字；没传就用函数名的兜底。
	sub := handlerName(h)
	b.mu.Lock()
	b.handlers[name] = append(b.handlers[name], h)
	idx := len(b.handlers[name]) - 1
	subs := len(b.handlers[name])
	b.mu.Unlock()
	fw.log(FWDebug, "bus", "", "subscribe", map[string]interface{}{
		"event": name, "sub": sub, "subs": subs,
	})
	return func() {
		b.mu.Lock()
		if idx < len(b.handlers[name]) {
			b.handlers[name] = append(b.handlers[name][:idx], b.handlers[name][idx+1:]...)
		}
		subs := len(b.handlers[name])
		b.mu.Unlock()
		fw.log(FWDebug, "bus", "", "unsubscribe", map[string]interface{}{
			"event": name, "sub": sub, "subs": subs,
		})
	}
}

// Publish 同步调用该事件的所有处理器。
// 单个 handler 的 panic 被捕获并记录，不影响其他 handler。
//
// 每次发布记一条框架日志：事件名、订阅者数、总耗时。事件转发的时延是
// 月度稳定性观测的核心指标——巡检周期 30 秒，如果一次 servers.changed
// 的处理耗时接近它，就说明有 handler 在做不该做的事。
func (b *Bus) Publish(name string, payload interface{}) {
	if b == nil {
		return
	}
	b.mu.RLock()
	handlers := make([]EventHandler, len(b.handlers[name]))
	copy(handlers, b.handlers[name])
	b.mu.RUnlock()
	if len(handlers) == 0 {
		// 没有订阅者：这条对"有没有人错过事件"很重要，但每条都记会淹日志，
		// 所以只记 debug。
		fw.log(FWDebug, "bus", "", "publish", map[string]interface{}{
			"event": name, "subs": 0, "detail": "no subscribers",
		})
		return
	}
	started := time.Now()
	panics := 0
	for _, h := range handlers {
		if callHandler(name, h, payload) {
			panics++
		}
	}
	fw.log(FWInfo, "bus", "", "publish", map[string]interface{}{
		"event":  name,
		"subs":   len(handlers),
		"ms":     time.Since(started).Milliseconds(),
		"panics": panics,
	})
}

func callHandler(name string, h EventHandler, payload interface{}) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			log.Warn("plugin: event %q handler panicked: %v", name, r)
			fw.log(FWWarn, "panic", handlerName(h), "handler-panic", map[string]interface{}{
				"event": name,
				"err":   fmt.Sprintf("%v", r),
			})
		}
	}()
	h(payload)
	return false
}

// handlerName 从运行时提取 handler 的可读名字（闭包所在函数），用于日志。
func handlerName(h EventHandler) string {
	v := runtime.FuncForPC(reflect.ValueOf(h).Pointer())
	if v == nil {
		return "unknown"
	}
	n := v.Name()
	// 去掉包路径，保留 plugin 包名 + 函数名，足够定位。
	if i := strings.LastIndex(n, "/"); i >= 0 {
		n = n[i+1:]
	}
	return n
}

// 核心事件名。插件订阅这些事件来响应核心状态变化。
const (
	// EventCoreStarted 在核心成功启动后发布，payload 为 nil。
	EventCoreStarted = "core.started"
	// EventCoreStopped 在核心停止后发布，payload 为 nil。
	EventCoreStopped = "core.stopped"
	// EventServersChanged 在节点/订阅发生变化后发布，payload 为 nil。
	EventServersChanged = "servers.changed"
	// EventShutdown 在 v2rayA 退出前发布，payload 为 nil。
	EventShutdown = "shutdown"
)

// ---- 管理器 ----

// Manager 维护全部已注册插件的状态。
//
// 线程安全；所有导出方法都可在并发下调用。
type Manager struct {
	mu       sync.RWMutex
	bus      *Bus
	plugins  map[string]Plugin  // name -> 插件实例
	states   map[string]PluginState
	contexts map[string]*Context
	// started 记录哪些插件的 OnStart 已成功调用过，
	// 避免重复 start（重复调用 OnStop 是安全的，但没必要）。
	started map[string]bool
}

// NewManager 构造空的管理器。
func NewManager() *Manager {
	return &Manager{
		bus:      NewBus(),
		plugins:  make(map[string]Plugin),
		states:   make(map[string]PluginState),
		contexts: make(map[string]*Context),
		started:  make(map[string]bool),
	}
}

// Bus 暴露事件总线，供核心发布事件、也供测试订阅。
func (m *Manager) Bus() *Bus { return m.bus }

// Register 注册一个插件。已存在同名插件时返回错误。
//
// 新注册插件的状态从数据库读：没有记录时，内置插件默认启用、
// 外部插件默认停用（避免装了不用却突然改变行为）。
func (m *Manager) Register(p Plugin) error {
	if p == nil {
		return errors.New("plugin: cannot register a nil plugin")
	}
	name := p.Name()
	if name == "" {
		return errors.New("plugin: plugin name must not be empty")
	}
	if !validName(name) {
		return fmt.Errorf("plugin: plugin name %q must not contain ':'", name)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.plugins[name]; ok {
		return fmt.Errorf("plugin: %q is already registered", name)
	}
	m.plugins[name] = p

	state, err := loadState(name, p.Builtin())
	if err != nil {
		// 读不到状态不让注册失败：退回到默认值，服务仍可用。
		log.Warn("plugin: cannot load state of %q: %v", name, err)
		fwLogf(FWWarn, "registry", name, "register", "state load failed, using default: %v", err)
	}
	m.states[name] = state
	m.contexts[name] = &Context{
		PluginName:     name,
		Events:         m.bus,
		settingsBucket: "plugin." + name,
	}
	fwLogf(FWInfo, "registry", name, "register",
		"version=%s builtin=%v state=%s", p.Version(), p.Builtin(), state)
	return nil
}

// Enable 启用并启动一个插件。已启用的插件重复调用是空操作。
func (m *Manager) Enable(name string) error {
	m.mu.Lock()
	p, ok := m.plugins[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("plugin: %q is not registered", name)
	}
	if m.states[name] == StateEnabled && m.started[name] {
		m.mu.Unlock()
		return nil
	}
	ctx := m.contexts[name]
	m.states[name] = StateEnabled
	if err := saveState(name, StateEnabled); err != nil {
		m.mu.Unlock()
		return err
	}
	m.mu.Unlock()

	if err := m.safeStart(name, p, ctx); err != nil {
		// start 失败：回滚状态，让用户看到失败而不是留下半启用状态。
		m.mu.Lock()
		m.states[name] = StateDisabled
		_ = saveState(name, StateDisabled)
		m.mu.Unlock()
		return fmt.Errorf("plugin: cannot start %q: %w", name, err)
	}
	return nil
}

// Disable 停用一个插件。停用后不接收事件，但配置数据保留，可再启用。
func (m *Manager) Disable(name string) error {
	m.mu.Lock()
	p, ok := m.plugins[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("plugin: %q is not registered", name)
	}
	if m.states[name] == StateDisabled {
		m.mu.Unlock()
		return nil
	}
	ctx := m.contexts[name]
	m.states[name] = StateDisabled
	if err := saveState(name, StateDisabled); err != nil {
		m.mu.Unlock()
		return err
	}
	m.mu.Unlock()
	m.safeStop(name, p, ctx)
	return nil
}

// Uninstall 卸载一个插件：先停用，再删除其全部持久化数据。
// 内置插件不可卸载——它的代码还在二进制里，删数据只会留下一个
// 永远空跑的注册项。
func (m *Manager) Uninstall(name string) error {
	m.mu.Lock()
	p, ok := m.plugins[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("plugin: %q is not registered", name)
	}
	if p.Builtin() {
		m.mu.Unlock()
		return fmt.Errorf("plugin: %q is built into v2rayA and cannot be uninstalled; disable it instead", name)
	}
	ctx := m.contexts[name]
	m.mu.Unlock()

	m.safeStop(name, p, ctx)

	m.mu.Lock()
	defer m.mu.Unlock()
	if err := db.BucketClear(settingsBucket(name)); err != nil {
		return fmt.Errorf("plugin: cannot clear data of %q: %w", name, err)
	}
	_ = db.Delete("plugins", name)
	delete(m.plugins, name)
	delete(m.states, name)
	delete(m.contexts, name)
	delete(m.started, name)
	return nil
}

// StartEnabled 启动全部处于启用态的插件。v2rayA 启动时调用一次。
// 单个插件失败只记日志，不阻塞其他插件，也不让 v2rayA 启动失败。
//
// 已经启动过的插件跳过：Enable 本身就会启动插件，这里只负责"上次退出前
// 是启用态、这次启动要恢复"的那一批。没有这个判断，Enable 之后紧跟一次
// StartEnabled 会让 OnStart 被调用两次。
func (m *Manager) StartEnabled() {
	m.mu.RLock()
	names := make([]string, 0, len(m.plugins))
	for n := range m.plugins {
		names = append(names, n)
	}
	m.mu.RUnlock()
	sort.Strings(names)
	started := time.Now()
	startedCount := 0
	for _, n := range names {
		m.mu.RLock()
		state, p, ctx, already := m.states[n], m.plugins[n], m.contexts[n], m.started[n]
		m.mu.RUnlock()
		if state != StateEnabled || already {
			continue
		}
		if err := m.safeStart(n, p, ctx); err != nil {
			log.Warn("plugin: %v", err)
			continue
		}
		startedCount++
	}
	fwLogf(FWInfo, "lifecycle", "", "start-enabled",
		"registered=%d started=%d took=%s", len(names), startedCount, time.Since(started).Round(time.Millisecond))
}

// StopAll 停掉全部插件。v2rayA 退出时调用。
func (m *Manager) StopAll() {
	m.mu.RLock()
	names := make([]string, 0, len(m.plugins))
	for n := range m.plugins {
		names = append(names, n)
	}
	m.mu.RUnlock()
	// 逆序停，若插件间有依赖则后启动的先停。
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for _, n := range names {
		m.mu.RLock()
		p, ctx, wasStarted := m.plugins[n], m.contexts[n], m.started[n]
		m.mu.RUnlock()
		if !wasStarted {
			continue
		}
		m.safeStop(n, p, ctx)
	}
}

// List 返回全部插件的状态快照，供管理页展示。
func (m *Manager) List() []Info {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Info, 0, len(m.plugins))
	for name, p := range m.plugins {
		out = append(out, Info{
			Name:        name,
			Description: p.Description(),
			Version:     p.Version(),
			Builtin:     p.Builtin(),
			State:       string(m.states[name]),
			Running:     m.started[name],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get 返回单个插件的信息。未注册时返回 false。
func (m *Manager) Get(name string) (Info, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.plugins[name]
	if !ok {
		return Info{}, false
	}
	return Info{
		Name:        name,
		Description: p.Description(),
		Version:     p.Version(),
		Builtin:     p.Builtin(),
		State:       string(m.states[name]),
		Running:     m.started[name],
	}, true
}

// Info 是插件的状态快照。
type Info struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Version     string `json:"version"`
	Builtin     bool   `json:"builtin"`
	State       string `json:"state"`
	Running     bool   `json:"running"`
}

// safeStart 调用插件的 OnStart，捕获 panic。
// 耗时记录进框架日志：OnStart 里做探测或打开大文件都会拖慢启用流程，
// 这个数字是月度观测里判断"启用卡了"的第一现场。
func (m *Manager) safeStart(name string, p Plugin, ctx *Context) error {
	started := time.Now()
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("plugin %q panicked in OnStart: %v", name, r)
			}
		}()
		err = p.OnStart(ctx)
	}()
	elapsed := time.Since(started)
	if err != nil {
		fw.log(FWWarn, "lifecycle", name, "start-failed", map[string]interface{}{
			"ms":  elapsed.Milliseconds(),
			"err": err.Error(),
		})
		return err
	}
	m.mu.Lock()
	m.started[name] = true
	m.mu.Unlock()
	fw.log(FWInfo, "lifecycle", name, "started", map[string]interface{}{
		"ms": elapsed.Milliseconds(),
	})
	log.Info("plugin: %q started", name)
	return nil
}

// safeStop 调用插件的 OnStop，捕获 panic。stop 失败只记日志。
func (m *Manager) safeStop(name string, p Plugin, ctx *Context) {
	started := time.Now()
	panicked := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
				log.Warn("plugin: %q panicked in OnStop: %v", name, r)
			}
		}()
		if err := p.OnStop(ctx); err != nil {
			log.Warn("plugin: %q OnStop: %v", name, err)
		}
	}()
	elapsed := time.Since(started)
	m.mu.Lock()
	m.started[name] = false
	m.mu.Unlock()
	if panicked {
		fw.log(FWWarn, "panic", name, "stop-panic", map[string]interface{}{
			"ms": elapsed.Milliseconds(),
		})
	} else {
		fw.log(FWInfo, "lifecycle", name, "stopped", map[string]interface{}{
			"ms": elapsed.Milliseconds(),
		})
	}
	log.Info("plugin: %q stopped", name)
}

// ---- 持久化 ----

const pluginStateBucket = "plugins"

func loadState(name string, builtin bool) (PluginState, error) {
	var s string
	if err := db.Get(pluginStateBucket, name, &s); err != nil {
		// 没有记录：内置插件默认启用，外部插件默认停用。
		if builtin {
			return StateEnabled, nil
		}
		return StateDisabled, nil
	}
	switch PluginState(s) {
	case StateEnabled, StateDisabled:
		return PluginState(s), nil
	default:
		// 脏值兜底。
		if builtin {
			return StateEnabled, nil
		}
		return StateDisabled, nil
	}
}

func saveState(name string, state PluginState) error {
	return db.Set(pluginStateBucket, name, string(state))
}

func validName(name string) bool {
	for _, r := range name {
		if r == ':' {
			return false
		}
	}
	return name != ""
}

// ---- 插件私有配置 ----
//
// 每个插件有一个属于自己的 bucket（plugin.<name>），卸载时整体清除。
// 放在框架里而不是让插件各自找地方存，是为了让"卸载会删干净所有数据"
// 成为框架的一条保证，而不是每个插件都要自己记得做。

func settingsBucket(name string) string { return "plugin." + name }

// GetSetting 读取插件自己的一个配置项。未设置时返回 false。
func GetSetting(name, key string, out interface{}) bool {
	if err := db.Get(settingsBucket(name), key, out); err != nil {
		return false
	}
	return true
}

// SetSetting 写入插件自己的一个配置项。
func SetSetting(name, key string, value interface{}) error {
	return db.Set(settingsBucket(name), key, value)
}
