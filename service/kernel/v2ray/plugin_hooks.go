package v2ray

import (
	"sync"
	"time"

	"github.com/v2rayA/v2rayA/plugin"
)

// 插件事件通知。
//
// 核心通过这里把生命周期事件发给插件。设计约束：
//
//   - 没有注册任何插件时，这个调用是零成本空操作（一个 RLock + 判空）；
//   - 通知失败绝不影响核心——插件出错不该让代理挂掉；
//   - 同步调用，因此插件在事件里的耗时直接算在核心启动/停止的耗时里。
//     这也是插件必须自己控制 OnStart/事件处理耗时的原因；
//   - 高频事件在极短时间内会被去重：一次用户操作往往触发多个变更请求
//     （连接、导入、清理旧数据），而插件对它们的反应是同一个。不去重的话，
//     插件日志里会出现一串内容完全相同的记录，把真正的事件淹没。

var (
	pluginMu      sync.RWMutex
	pluginManager *plugin.Manager

	// lastEventAt 记录每种事件上次发出的时刻，用于去重窗口。
	lastEventMu sync.Mutex
	lastEventAt map[string]time.Time
)

// dedupWindow 是高频事件的去重窗口。
// 取 2 秒：够覆盖一次操作引发的多个请求，又不至于把相邻两次真实操作
// （用户手快点了两下）合并成一条。
const dedupWindow = 2 * time.Second

// SetPluginManager 注入插件管理器。由 main 在启动早期调用一次。
// 传 nil 表示禁用插件通知。
func SetPluginManager(m *plugin.Manager) {
	pluginMu.Lock()
	defer pluginMu.Unlock()
	pluginManager = m
}

// NotifyPlugins 向全部插件广播一个事件。
// 没有注入 manager 时是空操作。
//
// dedup 为 true 时，dedupWindow 内同名的后续通知会被丢弃——用于节点变化
// 这类一次操作会触发多次的事件。
func NotifyPlugins(name string, dedup bool) {
	if dedup && !shouldNotify(name) {
		return
	}
	pluginMu.RLock()
	m := pluginManager
	pluginMu.RUnlock()
	if m == nil {
		return
	}
	m.Bus().Publish(name, nil)
}

// shouldNotify 判断该事件是否应真的发出；返回 false 表示被去重合并。
func shouldNotify(name string) bool {
	lastEventMu.Lock()
	defer lastEventMu.Unlock()
	if lastEventAt == nil {
		lastEventAt = make(map[string]time.Time)
	}
	now := time.Now()
	if last, ok := lastEventAt[name]; ok && now.Sub(last) < dedupWindow {
		return false
	}
	lastEventAt[name] = now
	return true
}

// 事件名从 plugin 包重新导出，让核心代码不必直接依赖 plugin 包。
const (
	EventCoreStarted    = plugin.EventCoreStarted
	EventCoreStopped    = plugin.EventCoreStopped
	EventServersChanged = plugin.EventServersChanged
	EventShutdown       = plugin.EventShutdown
)

// notifyPlugins 是核心内部用的便捷包装。
// 核心生命周期事件（启动/停止）天然低频且每次都重要，不做去重。
func notifyPlugins(name string) { NotifyPlugins(name, false) }

// notifyPluginsDeduplicated 用于节点变化这类高频事件。
func notifyPluginsDeduplicated(name string) { NotifyPlugins(name, true) }