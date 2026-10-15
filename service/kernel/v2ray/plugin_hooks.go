package v2ray

// 核心与插件之间的唯一事件出口。
//
// 核心不知道任何具体插件，只发布三条生命周期事件；由 pluginhost 转发给
// 插件。删掉 pluginhost 之后，本文件连同 pre_run 里的两处调用一并移除，
// 核心即回到原生形态。

import (
	"sync"
	"time"

	"github.com/v2rayA/v2rayA/pkg/util/log"
)

// 核心发布的生命周期事件。名称与 plugin 包中的常量一致，但这里不 import
// plugin 包：kernel/v2ray 是底层包，不该依赖上层框架。
const (
	// EventCoreStarted 核心已就绪、开始转发流量。
	EventCoreStarted = "core.started"
	// EventCoreStopped 核心已停止。
	EventCoreStopped = "core.stopped"
	// EventServersChanged 一次会改动节点或订阅的请求已提交，插件看到的
	// 候选集可能已经变了。
	EventServersChanged = "servers.changed"
	// EventShutdown 进程即将退出。
	EventShutdown = "shutdown"
)

// notify 把事件交给宿主转发。默认是空操作，因此没有宿主时这条路径是免费的。
var notify = func(event string) {}

// SetPluginNotifier 由宿主在启动时注入；传 nil 关闭转发。
func SetPluginNotifier(f func(event string)) {
	if f == nil {
		notify = func(string) {}
		return
	}
	notify = f
}

// dedupWindow 是一次配置变更所产生回调簇的时间跨度。
const dedupWindow = 2 * time.Second

var (
	dedupMu  sync.Mutex
	lastSent = make(map[string]time.Time)
)

// shouldNotify 判断这条事件现在该不该发出去。
//
// started/stopped/shutdown 不去重，它们各自只发生一次。被反复触发的是
// servers.changed：一次保存会经过多个内部回调，而插件据此重算分组是昂贵
// 操作，让它收到三遍同一条消息没有意义。
func shouldNotify(event string) bool {
	switch event {
	case EventCoreStarted, EventCoreStopped, EventShutdown:
		return true
	}
	now := time.Now()
	dedupMu.Lock()
	defer dedupMu.Unlock()
	if last, seen := lastSent[event]; seen && now.Sub(last) < dedupWindow {
		return false
	}
	lastSent[event] = now
	return true
}

// NotifyPlugins 发出一条生命周期事件。它是核心调用宿主的唯一入口。
func NotifyPlugins(event string) {
	if !shouldNotify(event) {
		return
	}
	log.Debug("core: notifying plugins: %s", event)
	notify(event)
}