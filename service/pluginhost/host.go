// Package pluginhost is the single seam between v2rayA and its plugins.
//
// Everything plugin-related lives behind this package: the framework, the
// built-in plugins, and the HTTP endpoints the plugins page needs. The core
// keeps exactly one import of it (in main.go) and one call that forwards
// lifecycle events, so removing this package plus those two lines returns a
// vanilla v2rayA.
//
// Why a host rather than letting the core import plugins directly: plugins
// need to react to core events and the GUI needs to list them, and both would
// otherwise spread `plugin.` references across the core.
package pluginhost

import (
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/v2rayA/v2rayA/conf"
	"github.com/v2rayA/v2rayA/kernel/v2ray"
	"github.com/v2rayA/v2rayA/pkg/util/log"
	"github.com/v2rayA/v2rayA/plugin"
	"github.com/v2rayA/v2rayA/plugins/autogroup"
	"github.com/v2rayA/v2rayA/plugins/proxyimport"
	"github.com/v2rayA/v2rayA/plugins/routefeeder"
	"github.com/v2rayA/v2rayA/server/service"
)

// autogroupLogDir is where autogroup writes its decision log: next to the
// configuration, so a test instance and a production one never share it.
func autogroupLogDir() string {
	return filepath.Join(conf.GetEnvironmentConfig().Config, "logs")
}

// Events the core publishes. Plugins subscribe; the core never imports a
// plugin package to do so.
const (
	EventCoreStarted    = plugin.EventCoreStarted
	EventCoreStopped    = plugin.EventCoreStopped
	EventServersChanged = plugin.EventServersChanged
	EventShutdown       = plugin.EventShutdown
)

var (
	initOnce sync.Once
	manager  *plugin.Manager

	// autoGroup is kept for the plugins page to query group state through.
	autoGroup *autogroup.Plugin
)

func mgr() *plugin.Manager {
	initOnce.Do(func() {
		// Framework log before anything registers: the register events should
		// land in the log, so the file must exist first. Same directory as the
		// decision logs; a broken file only silences the log, never the system.
		plugin.SetFrameworkLogDir(autogroupLogDir())
		plugin.Snapshot(fmt.Sprintf("v2rayA plugin host starting; config dir=%s", conf.GetEnvironmentConfig().Config))

		m := plugin.NewManager()

		// autogroup needs the latency test, which lives in server/service.
		// Injecting it here breaks the plugin -> service -> plugin cycle.
		autogroup.SetProbeHooks(service.TestHttpLatency)

		autoGroup = autogroup.NewWithLogDir(autogroupLogDir())
		_ = m.Register(autoGroup)         //nolint:errcheck // 注册失败在 Start 里报
		_ = m.Register(proxyimport.New()) //nolint:errcheck
		_ = m.Register(routefeeder.New()) //nolint:errcheck
		manager = m

		// The core publishes its lifecycle events here. Wiring it in this
		// package rather than in main.go keeps the dependency running one
		// way: core -> host -> plugins, never the reverse.
		//
		// The publish must not run on the caller's goroutine: the core emits
		// these from inside the process manager, and a plugin handler that
		// probes (autogroup does, for every group) would otherwise block core
		// startup for as long as the probes take. That is what left the API
		// listener unbound while the process sat in onCoreStarted.
		//
		// The queue is one-deep on purpose: events arrive far faster than
		// handlers can process them during a reload, and coalescing is fine —
		// every handler re-reads the current state rather than a delta, so
		// collapsing bursts loses nothing. The dropped counter in the log is
		// itself a stability metric: if it climbs, handlers are too slow for
		// the event rate and the design needs revisiting.
		v2ray.SetPluginNotifier(func(event string) {
			select {
			case notifyQueue <- event:
			default:
				// The previous event of this kind has not been consumed yet;
				// a newer one is already queued or being processed, which
				// makes this one redundant.
				fwDropped.Add(1)
			}
		})

		// Single consumer: keeps handler execution serial, so plugin state
		// never races with itself across events.
		go func() {
			for event := range notifyQueue {
				t0 := time.Now()
				manager.Bus().Publish(event, nil)
				plugin.Snapshot(fmt.Sprintf("event %s handled in %s", event, time.Since(t0).Round(time.Millisecond)))
			}
		}()
	})
	return manager
}

// notifyQueue serialises event delivery to the plugins. Buffered one deep:
// a second event while the first is being handled is coalesced away.
var notifyQueue = make(chan string, 1)

// fwDropped counts coalesced (dropped) notifications. Logged periodically;
// a rising number means handlers cannot keep up.
var fwDropped atomic.Uint64

// Manager returns the plugin manager, building it on first use.
func Manager() *plugin.Manager { return mgr() }

// AutoGroup returns the autogroup plugin, or nil before initialisation.
func AutoGroup() *autogroup.Plugin {
	mgr()
	return autoGroup
}

// Start brings up every plugin that is enabled. Called once from run(),
// after the database is open and before the core starts.
//
// A failure here is never fatal: a broken plugin must not stop v2rayA from
// proxying traffic.
func Start() {
	defer func() {
		if r := recover(); r != nil {
			log.Warn("pluginhost: a plugin panicked during Start: %v", r)
		}
	}()
	mgr().StartEnabled()
}

// StopAll shuts every plugin down. Called on exit, before the core stops.
func StopAll() {
	defer func() {
		if r := recover(); r != nil {
			log.Warn("pluginhost: a plugin panicked during StopAll: %v", r)
		}
	}()
	if manager == nil {
		return
	}
	manager.Bus().Publish(EventShutdown, nil)
	manager.StopAll()
}

// Notify forwards a core lifecycle event to the plugins.
//
// bus returns nil until the manager exists, which makes this a no-op for a
// build with the plugin host removed.
func Notify(event string) {
	if manager == nil {
		return
	}
	manager.Bus().Publish(event, nil)
}

// Installed reports whether any plugin is registered and running. The GUI
// uses it to hide the plugins destination entirely.
func Installed() bool {
	return manager != nil && len(manager.List()) > 0
}