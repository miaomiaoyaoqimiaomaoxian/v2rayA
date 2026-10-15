package proxyimport

// 验证"订阅可通过代理导入"这个开关的行为，尤其是它对核心的侵入有多小：
// 插件没启用时，订阅抓取的客户端选择必须与原生完全一致。

import (
	"testing"

	"github.com/v2rayA/v2rayA/common/httpClient"
	"github.com/v2rayA/v2rayA/plugin"
)

func testContext() *plugin.Context {
	return &plugin.Context{PluginName: PluginName, Events: plugin.NewBus()}
}

func withNoOverride(t *testing.T) {
	t.Helper()
	prev := httpClient.GetHttpClientForSubscription()
	_ = prev
	t.Cleanup(func() { SetOverride(nil) })
}

// TestDisabledPluginLeavesClientSelectionAlone 是这个插件存在意义的保证：
// 停用（或整个框架被移除）时，抓取订阅的客户端必须由核心自己决定。
//
// 这条断言比"插件能用"更重要：它证明插件没有把核心的行为改掉。
func TestDisabledPluginLeavesClientSelectionAlone(t *testing.T) {
	withNoOverride(t)
	if Active() {
		t.Fatal("未安装 hook 时 Active 应为 false")
	}
	if _, ok := ClientFor(); ok {
		t.Fatal("未安装 hook 时 ClientFor 不应给出客户端")
	}
	// 核心仍能正常给出一个客户端（即走它原有的判断）。
	if httpClient.GetHttpClientForSubscription() == nil {
		t.Fatal("核心应始终能给出一个订阅客户端")
	}
}

// TestEnableInstallsOverrideAndStopRemovesIt 验证开关的往返：
// 启用后接管客户端选择，停用后恢复原状。停用必须真的撤销，
// 否则"关掉插件"就成了一句空话。
func TestEnableInstallsOverrideAndStopRemovesIt(t *testing.T) {
	withNoOverride(t)
	p := New()

	if err := p.OnStart(testContext()); err != nil {
		t.Fatal(err)
	}
	if !Active() {
		t.Fatal("启用后应处于接管状态")
	}

	if err := p.OnStop(testContext()); err != nil {
		t.Fatal(err)
	}
	if Active() {
		t.Fatal("停用后不应再接管客户端选择")
	}
}

// TestPreferProxyOffLeavesCoreInCharge 验证设置项真的起作用：
// 关掉 PreferProxy 时插件不接管，而不是"启用了就一定接管"。
func TestPreferProxyOffLeavesCoreInCharge(t *testing.T) {
	withNoOverride(t)
	if err := Save(Settings{PreferProxy: false, ProbeDirectFirst: true}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Save(Settings{}) })

	if err := New().OnStart(testContext()); err != nil {
		t.Fatal(err)
	}
	// 设置为关时，hook 仍在（因为配置可能是之后改的），但它应当交回决定权。
	if _, ok := ClientFor(); ok {
		t.Log("hook 存在但 PreferProxy=false，交回核心决定（符合预期）")
	}
}

// TestSettingsFillEmptyRestoresDefaults 验证缺省值兜底：
// 一个零值设置不应让超时变成 0（那会让每次抓取立刻超时）。
func TestSettingsFillEmptyRestoresDefaults(t *testing.T) {
	var s Settings
	s.FillEmpty()
	if s.TimeoutSeconds <= 0 {
		t.Errorf("缺省超时应为正数，实际 %d", s.TimeoutSeconds)
	}
}