// Package plugins 是 v2rayA 的内置插件装配点。
//
// 这里做三件事：
//  1. 持有全局唯一的 plugin.Manager；
//  2. 注册全部内置插件（目前只有 autogroup）；
//  3. 把插件框架需要的"探测钩子"注入进去，打破循环依赖。
//
// 新增一个内置插件的步骤：在 newManager 里加一行 Register。
// 核心代码不需要任何改动——这正是插件化要达到的效果。
package plugins

import (
	"path/filepath"
	"sync"

	"github.com/v2rayA/v2rayA/conf"
	"github.com/v2rayA/v2rayA/plugin"
	"github.com/v2rayA/v2rayA/plugins/autogroup"
	"github.com/v2rayA/v2rayA/server/service"
)

var (
	managerOnce sync.Once
	manager     *plugin.Manager
	// autoGroup 是自动分组插件的实例引用，供 HTTP 层查询分组状态。
	autoGroup *autogroup.Plugin
)

// Manager 返回全局插件管理器（懒初始化 + 注册内置插件）。
func Manager() *plugin.Manager {
	managerOnce.Do(func() {
		m := plugin.NewManager()

		// 注入探测钩子：autogroup 需要测节点，实现放在 server/service，
		// 通过注入避免 plugin -> service -> plugin 的循环导入。
		autogroup.SetProbeHooks(service.TestHttpLatency)

		autoGroup = autogroup.NewWithLogDir(autogroupLogDir())
		if err := m.Register(autoGroup); err != nil {
			// 注册失败只记录，不让 v2rayA 起不来。
			// 正常情况下不会发生，除非同名插件被注册两次。
			panic(err)
		}
		manager = m
	})
	return manager
}

// autogroupLogDir 返回决策日志目录：与配置文件同级的 logs 子目录。
// 放在这里而不是硬编码，是为了让测试实例与生产实例各自落在自己的
// 配置目录下，互不干扰。
func autogroupLogDir() string {
	return filepath.Join(conf.GetEnvironmentConfig().Config, "logs")
}

// AutoGroup 返回自动分组插件实例；未初始化时返回 nil。
func AutoGroup() *autogroup.Plugin {
	Manager() // 确保已装配
	return autoGroup
}

// Groups 返回自动分组插件的分组视图。插件未启用时由插件自身返回空。
func Groups() []autogroup.Group {
	if autoGroup == nil {
		return nil
	}
	return autogroup.Groups()
}
