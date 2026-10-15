// Package proxyimport lets a subscription be fetched through v2rayA's own
// proxy.
//
// Why a plugin: v2rayA already has the client builders and the "which client
// to use" decision, but while a transparent proxy is on — which is the case on
// Windows, where TUN is the normal mode — GetHttpClientAutomatically returns a
// direct client, so the existing "proxy when importing subscriptions" setting
// never takes effect. A subscription reachable only through the proxy simply
// cannot be imported.
//
// The fix is one indirection: the core asks a hook which client to use, and the
// hook is the identity function until this plugin is enabled. With the plugin
// removed the hook disappears with it and v2rayA behaves exactly as before.
package proxyimport

import (
	"time"

	"github.com/v2rayA/v2rayA/common/httpClient"
	"github.com/v2rayA/v2rayA/plugin"
)

// PluginName is the plugin's registered name.
const PluginName = "proxyimport"

const (
	pluginVersion     = "1.0.0"
	pluginDescription = "Fetch subscriptions through v2rayA's own proxy when the direct route cannot reach them"
)

// Plugin is the proxyimport plugin.
type Plugin struct{}

var _ plugin.Plugin = Plugin{}

// New returns the plugin.
func New() Plugin { return Plugin{} }

func (Plugin) Name() string        { return PluginName }
func (Plugin) Description() string { return pluginDescription }
func (Plugin) Version() string     { return pluginVersion }
func (Plugin) Builtin() bool       { return true }

// OnStart installs the client override.
func (Plugin) OnStart(ctx *plugin.Context) error {
	settings := Load()
	*override = func() (any, bool) {
		if !settings.PreferProxy {
			return nil, false
		}
		return buildProxyClient(settings)
	}
	ctx.Logger().Info("已启用：订阅可通过本机代理导入（直接不通时回退）")
	return nil
}

// OnStop restores the default client selection. The settings stay so enabling
// the plugin later brings the behaviour back.
func (Plugin) OnStop(ctx *plugin.Context) error {
	*override = nil
	ctx.Logger().Info("已停用：订阅导入不再使用本机代理")
	return nil
}

// Settings is what the plugins page reads and writes.
type Settings struct {
	// PreferProxy routes subscription fetches through the local proxy.
	// It is independent of v2rayA's own ProxyModeWhenSubscribe, which is
	// forced to direct while a transparent proxy is on.
	PreferProxy bool `json:"preferProxy"`
	// ProbeDirectFirst tries the direct route once before falling back to the
	// proxy, so a directly reachable subscription pays no proxy hop.
	ProbeDirectFirst bool `json:"probeDirectFirst"`
	// TimeoutSeconds bounds one subscription fetch.
	TimeoutSeconds int `json:"timeoutSeconds"`
}

// SettingsKey is where the settings live inside the plugin's own bucket.
const SettingsKey = "settings"

func defaults() Settings {
	return Settings{PreferProxy: true, ProbeDirectFirst: true, TimeoutSeconds: 20}
}

// Load reads the settings, filling in defaults for unset fields.
func Load() Settings {
	s := defaults()
	if plugin.GetSetting(PluginName, SettingsKey, &s) {
		s.FillEmpty()
	}
	return s
}

// Save writes the settings.
func Save(s Settings) error {
	s.FillEmpty()
	return plugin.SetSetting(PluginName, SettingsKey, s)
}

// FillEmpty restores defaults for unset fields.
func (s *Settings) FillEmpty() {
	if s.TimeoutSeconds <= 0 {
		s.TimeoutSeconds = defaults().TimeoutSeconds
	}
}

// override is the hook the core calls. nil means "no plugin is steering
// fetches", which is the state a build without this plugin is always in.
var override *func() (any, bool)

// SetOverride installs the hook. The core calls this once at startup; passing
// nil restores vanilla behaviour.
func SetOverride(f func() (any, bool)) {
	override = &f
}

// Active reports whether a plugin is currently steering fetches.
func Active() bool {
	return override != nil && *override != nil
}

// ClientFor returns the client a subscription fetch should use.
//
// It returns (nil, false) to say "use the core's own decision".
func ClientFor() (any, bool) {
	if override == nil || *override == nil {
		return nil, false
	}
	return (*override)()
}

// buildProxyClient returns a client that goes through v2rayA's local socks
// port, which carries the user's outbound selection.
func buildProxyClient(s Settings) (any, bool) {
	c, err := httpClient.GetHttpClientWithv2rayAProxy()
	if err != nil {
		return nil, false
	}
	s.FillEmpty()
	c.Timeout = time.Duration(s.TimeoutSeconds) * time.Second
	return c, true
}