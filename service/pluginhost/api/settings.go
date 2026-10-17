package api

import (
	"errors"
	"fmt"

	"github.com/gin-gonic/gin"
	"github.com/v2rayA/v2rayA/common"
	"github.com/v2rayA/v2rayA/pluginhost"
	"github.com/v2rayA/v2rayA/plugins/autogroup"
	"github.com/v2rayA/v2rayA/plugins/proxyimport"
	"github.com/v2rayA/v2rayA/plugins/routefeeder"
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

// GetPluginRouteFeed 返回路由学习器收集的观察（最新在前）。
// 只读：收集过程从不改配置，这里只是把数据给界面看。
func GetPluginRouteFeed(ctx *gin.Context) {
	p := pluginhost.RouteFeeder()
	if p == nil {
		common.ResponseError(ctx, logError(errors.New("the routefeeder plugin is not installed")))
		return
	}
	common.ResponseSuccess(ctx, gin.H{
		"observations": routefeeder.GetObservations(),
	})
}

// PostPluginRouteFeedApply 把学到的规则写入 RoutingA 并重载核心。
//
// 这是整个插件唯一会造成核心重启（短暂断网）的路径，因此必须是显式的
// POST 而不是收集路径的一部分：断网时机由用户决定。
func PostPluginRouteFeedApply(ctx *gin.Context) {
	release, ok := beginMutation(ctx)
	if !ok {
		return
	}
	defer release()

	p := pluginhost.RouteFeeder()
	if p == nil {
		common.ResponseError(ctx, logError(errors.New("the routefeeder plugin is not installed")))
		return
	}
	n, err := p.ApplyLearned()
	if err != nil {
		common.ResponseError(ctx, logError(err))
		return
	}
	common.ResponseSuccess(ctx, gin.H{
		"applied": n,
		"message": fmt.Sprintf("已写入 %d 条规则并重载核心", n),
	})
}