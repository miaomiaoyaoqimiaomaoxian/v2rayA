package api

import (
	"errors"

	"github.com/gin-gonic/gin"
	"github.com/v2rayA/v2rayA/common"
	"github.com/v2rayA/v2rayA/pluginhost"
	"github.com/v2rayA/v2rayA/plugins/autogroup"
	"github.com/v2rayA/v2rayA/plugins/proxyimport"
)

var errNegativeInterval = errors.New("intervalSeconds must not be negative")

// GetPluginProxyImport reports the proxyimport switch and whether it is
// currently steering subscription fetches.
func GetPluginProxyImport(ctx *gin.Context) {
	common.ResponseSuccess(ctx, gin.H{
		"settings": proxyimport.Load(),
		"active":   proxyimport.Active(),
	})
}

// PutPluginProxyImport stores the switch. The plugin must be enabled for the
// settings to take effect, which is why the response echoes `active`
// separately from the stored value.
func PutPluginProxyImport(ctx *gin.Context) {
	release, ok := beginMutation(ctx)
	if !ok {
		return
	}
	defer release()

	var body proxyimport.Settings
	if err := ctx.ShouldBindJSON(&body); err != nil {
		common.ResponseError(ctx, badRequest("settings", err))
		return
	}
	body.FillEmpty()
	if err := proxyimport.Save(body); err != nil {
		common.ResponseError(ctx, logError(err))
		return
	}
	GetPluginProxyImport(ctx)
}

// GetPluginWatchdog reports how the health watchdog is configured.
func GetPluginWatchdog(ctx *gin.Context) {
	common.ResponseSuccess(ctx, gin.H{
		"settings": autogroup.LoadWatchdogSettings(),
		"running":  autogroup.WatchdogEnabled(),
	})
}

// PutPluginWatchdog stores the watchdog configuration.
//
// The watchdog is what makes automatic switching actually happen, so turning
// it off is reported back plainly rather than silently accepted: the user is
// left with node picking but no failover.
func PutPluginWatchdog(ctx *gin.Context) {
	release, ok := beginMutation(ctx)
	if !ok {
		return
	}
	defer release()

	var body autogroup.WatchdogSettings
	if err := ctx.ShouldBindJSON(&body); err != nil {
		common.ResponseError(ctx, badRequest("settings", err))
		return
	}
	if body.IntervalSeconds < 0 {
		common.ResponseError(ctx, badRequest("intervalSeconds", errNegativeInterval))
		return
	}
	if err := autogroup.SaveWatchdogSettings(body); err != nil {
		common.ResponseError(ctx, logError(err))
		return
	}
	// Re-applying without a restart: stop the old loop, start a new one when
	// it is still enabled, so the new interval takes effect now.
	if p := pluginhost.AutoGroup(); p != nil {
		p.ReloadWatchdog()
	}
	GetPluginWatchdog(ctx)
}