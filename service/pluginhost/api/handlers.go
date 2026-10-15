package api

import (
	"errors"
	"fmt"

	"github.com/gin-gonic/gin"
	"github.com/v2rayA/v2rayA/common"
	"github.com/v2rayA/v2rayA/db/configure"
	"github.com/v2rayA/v2rayA/pluginhost"
	"github.com/v2rayA/v2rayA/plugins/autogroup"
)

// 插件相关的固定错误。
var (
	pluginNotInstalled = errors.New("the autogroup plugin is not installed")
	pluginDisabled     = errors.New("the autogroup plugin is disabled; enable it on the plugins page")
	noCandidates       = errors.New("the outbound group has no connected nodes")
)

// pluginGroupCandidates 返回一个分组的全部已连接节点。
func pluginGroupCandidates(outbound string) []configure.NodeRef {
	refs := configure.GetConnectedServersByOutbound(outbound)
	if refs == nil || refs.Len() == 0 {
		return nil
	}
	out := make([]configure.NodeRef, 0, refs.Len())
	for _, r := range refs.Get() {
		out = append(out, *r)
	}
	return out
}

// pluginPolicyFromRequest 把请求体转成 GroupPolicy，并做基本校验。
func pluginPolicyFromRequest(mode string, failThreshold, maxSwitches int, autoTest bool) (configure.GroupPolicy, error) {
	p := configure.GroupPolicy{
		Mode:          configure.GroupMode(mode),
		FailThreshold: failThreshold,
		MaxSwitches:   maxSwitches,
		AutoTest:      autoTest,
	}
	// Mode 非法时 Normalize 会兜底成 any；这里显式报错更好，避免用户
	// 以为设置了 pinned 实际却是 any。
	switch configure.GroupMode(mode) {
	case configure.GroupModeAny, configure.GroupModePinned:
	default:
		return configure.GroupPolicy{}, errors.New(`"mode" must be "any" or "pinned"`)
	}
	if failThreshold < 1 {
		return configure.GroupPolicy{}, errors.New(`"failThreshold" must be at least 1`)
	}
	if maxSwitches < 1 {
		return configure.GroupPolicy{}, errors.New(`"maxSwitches" must be at least 1`)
	}
	return p, nil
}

// setGroupPolicy 写入分组策略。
func setGroupPolicy(outbound string, policy configure.GroupPolicy) error {
	return configure.SetGroupPolicy(outbound, policy)
}

// GetPluginLog 返回决策日志的状态：文件路径、当前级别、可用级别。
func GetPluginLog(ctx *gin.Context) {
	p := pluginhost.AutoGroup()
	if p == nil {
		common.ResponseError(ctx, logError(pluginNotInstalled))
		return
	}
	resp := gin.H{
		"level": string(autogroup.LoadLogLevel()),
		"path":  "",
		"levels": []string{
			string(autogroup.LevelDebug),
			string(autogroup.LevelInfo),
			string(autogroup.LevelWarn),
			string(autogroup.LevelError),
		},
	}
	if l := p.Logger(); l != nil {
		resp["path"] = l.Path()
		resp["running"] = l.Path() != ""
	}
	common.ResponseSuccess(ctx, resp)
}

// PutPluginLog 修改决策日志级别：立即生效并持久化，无需重启。
func PutPluginLog(ctx *gin.Context) {
	release, ok := beginMutation(ctx)
	if !ok {
		return
	}
	defer release()

	var data struct {
		Level string `json:"level"`
	}
	if err := ctx.ShouldBindJSON(&data); err != nil || data.Level == "" {
		common.ResponseError(ctx, badRequest("plugin log", `request body must be {"level": "debug"|"info"|"warn"|"error"}`))
		return
	}
	level := autogroup.ParseLogLevel(data.Level, "")
	if level == "" {
		common.ResponseError(ctx, logError(fmt.Errorf("invalid level %q; expected debug, info, warn or error", data.Level)))
		return
	}
	if err := autogroup.SaveLogLevel(level); err != nil {
		common.ResponseError(ctx, logError(err))
		return
	}
	if p := pluginhost.AutoGroup(); p != nil {
		if l := p.Logger(); l != nil {
			l.SetLevel(level)
		}
	}
	GetPluginLog(ctx)
}

// GetPlugins 返回全部插件的状态列表。
func GetPlugins(ctx *gin.Context) {
	common.ResponseSuccess(ctx, gin.H{
		"plugins": pluginhost.Manager().List(),
	})
}

// PutPlugin 启用或停用一个插件。
//
//   - {"name": "...", "enabled": true}   启用（调用 OnStart）
//   - {"name": "...", "enabled": false}  停用（调用 OnStop，保留数据）
func PutPlugin(ctx *gin.Context) {
	release, ok := beginMutation(ctx)
	if !ok {
		return
	}
	defer release()

	var data struct {
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
	}
	if err := ctx.ShouldBindJSON(&data); err != nil || data.Name == "" {
		common.ResponseError(ctx, badRequest("plugin", `request body must be {"name": string, "enabled": bool}`))
		return
	}

	var err error
	if data.Enabled {
		err = pluginhost.Manager().Enable(data.Name)
	} else {
		err = pluginhost.Manager().Disable(data.Name)
	}
	if err != nil {
		common.ResponseError(ctx, logError(err))
		return
	}
	GetPlugins(ctx)
}

// DeletePlugin 卸载一个插件：停用并删除其全部持久化数据。
// 内置插件不可卸载，只能停用。
func DeletePlugin(ctx *gin.Context) {
	release, ok := beginMutation(ctx)
	if !ok {
		return
	}
	defer release()

	var data struct {
		Name string `json:"name"`
	}
	if err := ctx.ShouldBindJSON(&data); err != nil || data.Name == "" {
		common.ResponseError(ctx, badRequest("plugin", `request body must be {"name": string}`))
		return
	}
	if err := pluginhost.Manager().Uninstall(data.Name); err != nil {
		common.ResponseError(ctx, logError(err))
		return
	}
	GetPlugins(ctx)
}

// PostPluginTest 对一个分组主动做全量节点重测，返回每个节点的结论。
// pinned 分组的用户想知道"到底是哪个节点挂了"时用它。
func PostPluginTest(ctx *gin.Context) {
	release, ok := beginMutation(ctx)
	if !ok {
		return
	}
	defer release()

	var data struct {
		Outbound string `json:"outbound"`
	}
	if err := ctx.ShouldBindJSON(&data); err != nil || data.Outbound == "" {
		common.ResponseError(ctx, badRequest("plugin test", `request body must be {"outbound": string}`))
		return
	}

	p := pluginhost.AutoGroup()
	if p == nil {
		common.ResponseError(ctx, logError(pluginNotInstalled))
		return
	}
	pilot := p.Pilot()
	if pilot == nil {
		common.ResponseError(ctx, logError(pluginDisabled))
		return
	}

	// 取该分组的候选节点。
	var candidates = pluginGroupCandidates(data.Outbound)
	if len(candidates) == 0 {
		common.ResponseError(ctx, logError(noCandidates))
		return
	}
	results, err := pilot.TestCandidates(candidates)
	if err != nil {
		common.ResponseError(ctx, logError(err))
		return
	}
	common.ResponseSuccess(ctx, gin.H{"results": results})
}

// GetPluginGroups 返回自动分组插件视角下的全部分组及其节点健康状态。
func GetPluginGroups(ctx *gin.Context) {
	p := pluginhost.AutoGroup()
	if p == nil {
		common.ResponseError(ctx, logError(pluginNotInstalled))
		return
	}
	pilot := p.Pilot()
	if pilot == nil {
		common.ResponseError(ctx, logError(pluginDisabled))
		return
	}
	groups := make([]gin.H, 0)
	for _, g := range autogroup.Groups() {
		groups = append(groups, gin.H{
			"outbound": g.Name,
			"status":   pilot.Status(g.Name, g.Candidates),
		})
	}
	// A group the watchdog gave up on is surfaced here so an outage shows on
	// the page instead of only in the log.
	alerts := map[string]string{}
	if wd := p.Watchdog(); wd != nil {
		for _, name := range wd.Alerts() {
			alerts[name] = "all nodes unreachable"
		}
	}
	if len(alerts) == 0 {
		alerts = nil
	}
	common.ResponseSuccess(ctx, gin.H{"groups": groups, "alerts": alerts})
}

// PutPluginPolicy 修改一个分组的自动化策略（模式与阈值）。
func PutPluginPolicy(ctx *gin.Context) {
	release, ok := beginMutation(ctx)
	if !ok {
		return
	}
	defer release()

	var data struct {
		Outbound string `json:"outbound"`
		Policy   struct {
			Mode          string `json:"mode"`
			FailThreshold int    `json:"failThreshold"`
			MaxSwitches   int    `json:"maxSwitches"`
			AutoTest      bool   `json:"autoTest"`
		} `json:"policy"`
	}
	if err := ctx.ShouldBindJSON(&data); err != nil || data.Outbound == "" {
		common.ResponseError(ctx, badRequest("plugin policy", `request body must be {"outbound": string, "policy": {...}}`))
		return
	}
	policy, err := pluginPolicyFromRequest(data.Policy.Mode, data.Policy.FailThreshold, data.Policy.MaxSwitches, data.Policy.AutoTest)
	if err != nil {
		common.ResponseError(ctx, logError(err))
		return
	}
	if err := setGroupPolicy(data.Outbound, policy); err != nil {
		common.ResponseError(ctx, logError(err))
		return
	}
	common.ResponseSuccess(ctx, gin.H{"policy": policy})
}
