package api

import (
	"github.com/gin-gonic/gin"
)

// RegisterRoutes adds every plugin endpoint to the authenticated group.
//
// Keeping the whole set behind one call means the core's router has a single
// line to remove: the plugin surface is added in one place and taken away in
// one place, so a vanilla build cannot end up with half of it.
func RegisterRoutes(auth *gin.RouterGroup) {
	// Which plugins exist, and whether each is on and running.
	auth.GET("plugins", GetPlugins)
	auth.PUT("plugin", PutPlugin)
	auth.DELETE("plugin", DeletePlugin)

	// The autogroup plugin's own surface: group policy, node health, and a
	// manual re-test.
	auth.GET("pluginGroups", GetPluginGroups)
	auth.PUT("pluginPolicy", PutPluginPolicy)
	auth.POST("pluginTest", PostPluginTest)

	// The decision log and its verbosity.
	auth.GET("pluginLog", GetPluginLog)
	auth.PUT("pluginLog", PutPluginLog)

	// The proxyimport plugin's switch: fetch subscriptions through v2rayA.
	auth.GET("pluginProxyImport", GetPluginProxyImport)
	auth.PUT("pluginProxyImport", PutPluginProxyImport)

	// The autogroup watchdog: interval, probe URL and whether it runs.
	auth.GET("pluginWatchdog", GetPluginWatchdog)
	auth.PUT("pluginWatchdog", PutPluginWatchdog)

	// The routefeeder plugin: collected dual-path observations, and the
	// explicit apply action. Apply is the only plugin path that writes rules
	// and reloads the core, so it is user-triggered by design -- collecting
	// never touches the running config.
	auth.GET("pluginRouteFeed", GetPluginRouteFeed)
	auth.POST("pluginRouteFeedApply", PostPluginRouteFeedApply)
}