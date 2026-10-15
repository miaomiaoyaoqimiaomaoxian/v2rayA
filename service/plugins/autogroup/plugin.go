// Package autogroup 是「自动分组」插件。
//
// 它把 AutoPilot 控制器包装成一个标准插件，因此可以：
//   - 随 v2rayA 启动自动启用；
//   - 从插件管理页随时停用（停用后完全不介入分组行为）；
//   - 删除配置数据后重新启用，回到默认状态。
//
// 对原功能的影响：停用该插件时，v2rayA 的分组行为与本插件出现前完全一致
// ——插件只在启用时才订阅事件、才在核心启动前后做决策。
package autogroup

import (
	"fmt"
	"sync"
	"time"

	"github.com/v2rayA/v2rayA/db"
	"github.com/v2rayA/v2rayA/db/configure"
	"github.com/v2rayA/v2rayA/pkg/util/log"
	"github.com/v2rayA/v2rayA/plugin"
)

// 决策日志的配置存在插件私有 bucket：复用框架分配的
// plugin.<name> 命名空间（由 plugin.Context 提供），这里直接用同名 bucket。
const logSettingsBucket = "plugin.autogroup"

// LoadLogLevel 读取持久化的决策日志级别，缺省为 info。
// 之所以默认 info 而不是 debug：debug 级别的量级比 info 大两个数量级，
// 而"每一步中间状态"在多数排查里用不到。真要追细节再临时开到 debug。
func LoadLogLevel() Level {
	var s LogLevelSetting
	if err := db.Get(logSettingsBucket, "logSettings", &s); err != nil {
		return LevelInfo
	}
	return ParseLogLevel(s.Level, LevelInfo)
}

// SaveLogLevel 持久化决策日志级别。
func SaveLogLevel(l Level) error {
	return db.Set(logSettingsBucket, "logSettings", LogLevelSetting{Level: string(l)})
}

// 插件元信息。
const (
	pluginName        = "autogroup"
	pluginVersion     = "1.0.0"
	pluginDescription = "自动分组：按分组模式自动挑选/切换节点，缓存节点健康状态"
)

// Plugin 实现 plugin.Plugin，把 AutoPilot 接入插件框架。
type Plugin struct {
	// mu 保护 pilot 与 unsubscribes 的替换。
	mu sync.Mutex
	// pilot 是启用态下的控制器；停用时为 nil，表示插件不介入。
	pilot *AutoPilot
	// watchdog 周期性验证各分组节点是否还能带流量，并驱动切换。
	watchdog *Watchdog
	// unsubscribes 保存事件订阅的取消函数，停用时逐一调用。
	unsubscribes []func()
	// logger 是决策日志记录器；停用时关闭文件句柄。
	logger *Logger
	// logDir 是决策日志目录，由装配层注入。
	logDir string
}

// New 构造插件实例。注册时调用一次。
func New() *Plugin { return &Plugin{} }

// NewWithLogDir 构造插件并指定决策日志目录。
// 目录随配置目录走，这样测试与生产各自隔离。
func NewWithLogDir(dir string) *Plugin { return &Plugin{logDir: dir} }

// 编译期断言：确认 Plugin 满足框架接口。
var _ plugin.Plugin = (*Plugin)(nil)

func (p *Plugin) Name() string        { return pluginName }
func (p *Plugin) Description() string { return pluginDescription }
func (p *Plugin) Version() string     { return pluginVersion }
func (p *Plugin) Builtin() bool       { return true }

// OnStart 启用插件：构造控制器、打开决策日志、订阅核心事件。
//
// 探测与应用的实现在这里注入——这是插件与核心唯一的耦合点，
// 因此换成别的实现（例如换成 mihomo 内核）只改这一个函数。
func (p *Plugin) OnStart(ctx *plugin.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pilot != nil {
		return nil // 已启动
	}

	// 决策日志：目录不可用时 NewLogger 返回 disabled 记录器，
	// 插件照常工作——记不了日志不该让自动分组失效。
	logger := NewLogger(p.logDir)
	logger.SetLevel(LoadLogLevel())
	p.logger = logger
	SetProbeLogger(logger)

	pilot := NewAutoPilot(ProbeNodes, ApplySelection)
	pilot.SetLogger(logger)

	// 巡检是自动切换的实际驱动方：没有它，失败计数永远停在 0。
	if WatchdogEnabled() {
		interval, probeURL := WatchdogConfig()
		w := NewWatchdog(pilot, interval, probeURL)
		w.Start()
		p.watchdog = w
	}

	p.unsubscribes = []func(){
		ctx.Events.Subscribe(plugin.EventCoreStarted, func(interface{}) {
			p.onCoreStarted()
		}),
		ctx.Events.Subscribe(plugin.EventServersChanged, func(interface{}) {
			p.onServersChanged()
		}),
		ctx.Events.Subscribe(plugin.EventCoreStopped, func(interface{}) {
			p.onCoreStopped()
		}),
	}
	p.pilot = pilot

	logger.Info(Event{
		Decision: "plugin-start",
		Msg:      "自动分组插件已启用",
		Reason:   fmt.Sprintf("版本 %s，决策日志 %s", pluginVersion, loggerPathOrNone(logger)),
	})
	ctx.Logger().Info("autogroup 已启用（版本 %s）", pluginVersion)
	return nil
}

// loggerPathOrNone 让"日志没打开"这件事在日志里也看得见。
func loggerPathOrNone(l *Logger) string {
	if l == nil {
		return "未启用"
	}
	if p := l.Path(); p != "" {
		return p
	}
	return "目录不可用，已降级为仅主日志"
}

// OnStop 停用插件：退订事件、记录停用、丢弃控制器并关闭日志文件。
// 之后再发生的事件插件一概不收，分组行为回到原生状态。
func (p *Plugin) OnStop(ctx *plugin.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	// 先停巡检：它可能正在驱动一次切换，先让它停下来再丢控制器。
	if p.watchdog != nil {
		p.watchdog.Stop()
		p.watchdog = nil
	}
	for _, unsub := range p.unsubscribes {
		unsub()
	}
	p.unsubscribes = nil
	if p.logger != nil {
		p.logger.Info(Event{
			Decision: "plugin-stop",
			Msg:      "自动分组插件已停用，后续不再介入分组选择",
		})
		SetProbeLogger(nil)
		_ = p.logger.Close()
		p.logger = nil
	}
	p.pilot = nil
	ctx.Logger().Info("autogroup 已停用")
	return nil
}

// Pilot 返回当前控制器；插件停用时为 nil。
// 供 HTTP API 层查询分组状态、主动重测。
func (p *Plugin) Pilot() *AutoPilot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pilot
}

// Logger 返回当前的决策日志记录器（可能为 nil）。
func (p *Plugin) Logger() *Logger {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.logger
}

// logEvent 是事件处理器的统一埋点入口。
// 记录"收到了什么事件"和"处理结果"，这样事后能看出
// 某个时刻的决策是被哪个事件触发的。
func (p *Plugin) logEvent(ev Event) {
	p.mu.Lock()
	l := p.logger
	p.mu.Unlock()
	if l != nil {
		l.Log(ev)
	}
}

// onCoreStarted 在核心启动后校正所有分组。
func (p *Plugin) onCoreStarted() {
	pilot := p.Pilot()
	if pilot == nil {
		return
	}
	groups := Groups()
	p.logEvent(Event{
		Decision: "event",
		Level:    LevelInfo,
		Msg:      fmt.Sprintf("收到事件 核心启动，需校正 %d 个分组", len(groups)),
	})
	for _, g := range groups {
		if _, err := pilot.EnsureGroup(g.Name, g.Candidates); err != nil {
			log.Warn("plugin/autogroup: 核心启动后校正分组 %q 失败: %v", g.Name, err)
		}
	}
}

// onServersChanged 在节点/订阅变化后重新评估。
// 这里刻意只重算、不强制切换：用户刚改完节点，插件不该立刻把他换走。
func (p *Plugin) onServersChanged() {
	pilot := p.Pilot()
	if pilot == nil {
		return
	}
	groups := Groups()
	total := 0
	for _, g := range groups {
		total += len(g.Candidates)
		pilot.RefreshCandidates(g.Name, g.Candidates)
	}
	p.logEvent(Event{
		Decision: "event",
		Level:    LevelInfo,
		Msg:      fmt.Sprintf("收到事件 节点变化，刷新 %d 个分组共 %d 个候选的缓存", len(groups), total),
		Reason:   "只清理已消失节点的缓存，不切换出口",
	})
}

// onCoreStopped 核心停止后清空连续切换计数：下次启动从零开始。
func (p *Plugin) onCoreStopped() {
	pilot := p.Pilot()
	if pilot == nil {
		return
	}
	pilot.ResetSwitchCounts()
	p.logEvent(Event{
		Decision: "event",
		Msg:      "收到事件 核心停止，连续切换计数已清零",
	})
}

// Group 描述一个分组及其候选节点。
type Group struct {
	Name       string
	Candidates []configure.NodeRef
}

// Groups 返回全部有已连接节点的分组。
// 没有启用中的分组时返回空切片。
func Groups() []Group {
	var out []Group
	for _, name := range configure.GetOutbounds() {
		refs := configure.GetConnectedServersByOutbound(name)
		if refs == nil || refs.Len() == 0 {
			continue
		}
		candidates := make([]configure.NodeRef, 0, refs.Len())
		for _, r := range refs.Get() {
			candidates = append(candidates, *r)
		}
		out = append(out, Group{Name: name, Candidates: candidates})
	}
	return out
}

// Watchdog 返回健康巡检器；未启用巡检时为 nil。
func (p *Plugin) Watchdog() *Watchdog {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.watchdog
}

// ReloadWatchdog 重启健康巡检，让改过的间隔与探测地址立即生效，
// 不必重启 v2rayA。插件停用时是空操作。
func (p *Plugin) ReloadWatchdog() {
	p.mu.Lock()
	pilot := p.pilot
	if p.watchdog != nil {
		p.watchdog.Stop()
		p.watchdog = nil
	}
	p.mu.Unlock()
	if pilot == nil || !WatchdogEnabled() {
		return
	}
	interval, probeURL := WatchdogConfig()
	w := NewWatchdog(pilot, interval, probeURL)
	w.Start()
	p.mu.Lock()
	p.watchdog = w
	p.mu.Unlock()
}

// WatchdogSettings 配置健康巡检。
type WatchdogSettings struct {
	// IntervalSeconds 是两轮巡检的间隔；0 表示用默认值。
	IntervalSeconds int `json:"intervalSeconds"`
	// ProbeURL 是探测地址；空表示用默认值。
	ProbeURL string `json:"probeUrl"`
	// Enabled 为 false 时插件照常挑选节点，但不做周期性巡检，
	// 也就不会在节点失效后自动切换。
	Enabled bool `json:"enabled"`
}

const watchdogSettingsKey = "watchdog"

func defaultWatchdogSettings() WatchdogSettings {
	return WatchdogSettings{Enabled: true, IntervalSeconds: int(defaultInterval / time.Second)}
}

// LoadWatchdogSettings 读取巡检配置，缺省启用。
func LoadWatchdogSettings() WatchdogSettings {
	s := defaultWatchdogSettings()
	_ = db.Get(logSettingsBucket, watchdogSettingsKey, &s)
	return s
}

// SaveWatchdogSettings 持久化巡检配置。
func SaveWatchdogSettings(s WatchdogSettings) error {
	return db.Set(logSettingsBucket, watchdogSettingsKey, s)
}

// WatchdogConfig 返回巡检要用的间隔与探测地址。调用方先看
// WatchdogSettings.Enabled 决定要不要启动循环，这样"停用"意味着
// 根本没有 goroutine、没有请求，而不是一个什么都不做的循环。
func WatchdogConfig() (interval time.Duration, probeURL string) {
	s := LoadWatchdogSettings()
	if s.IntervalSeconds <= 0 {
		s.IntervalSeconds = int(defaultInterval / time.Second)
	}
	return time.Duration(s.IntervalSeconds) * time.Second, s.ProbeURL
}

// WatchdogEnabled 报告周期性巡检是否开启。
func WatchdogEnabled() bool { return LoadWatchdogSettings().Enabled }
