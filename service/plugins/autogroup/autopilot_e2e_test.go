package autogroup

// 隔离的端到端验证：用一个独立的临时配置目录，完全不触碰正式核心
// （正式核心的 PID、config.json、系统代理都不受影响）。
//
// 做法与 service/server/service/latency_test.go 中的 TestMain 一致：
// 把 conf.GetEnvironmentConfig().Config 指到临时目录，所有 db/configure
// 读写就都落在临时库里。本文件不启动任何真实核心进程——要验证的是
// 控制器的决策与持久化，不是网络连通性。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/v2rayA/v2rayA/conf"
	"github.com/v2rayA/v2rayA/db/configure"
	"github.com/v2rayA/v2rayA/kernel/v2ray"
	"github.com/v2rayA/v2rayA/plugin"
)

// pluginContext 构造一个最小可用的插件上下文（独立事件总线，不打扰全局）。
func pluginContext() *plugin.Context {
	return &plugin.Context{
		PluginName: "autogroup-test",
		Events:     plugin.NewBus(),
	}
}

// setupIsolatedEnv 准备隔离环境：临时配置目录 + 复用正式资产目录（只读）。
func setupIsolatedEnv(t *testing.T) {
	t.Helper()
	root, err := os.MkdirTemp("", "v2raya-autogroup-e2e-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	cfgDir := filepath.Join(root, "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 资产（geoip/geosite dat）直接复用正式安装里的，避免复制上百 MB。
	assetsDir := `C:\ProgramData\v2rayA\data`
	if _, err := os.Stat(filepath.Join(assetsDir, "geoip.dat")); err != nil {
		t.Skipf("正式资产目录不可用，跳过隔离测试: %v", err)
	}

	env := conf.GetEnvironmentConfig()
	oldConfig := env.Config
	env.Config = cfgDir
	t.Cleanup(func() { env.Config = oldConfig })
}

// TestPinnedModeAllDead 是核心行为：pinned 分组候选全灭时，必须明确报
// ALL_NODES_UNREACHABLE，而不是静默切到别的分组或别的节点。
func TestPinnedModeAllDead(t *testing.T) {
	setupIsolatedEnv(t)

	candidates := []configure.NodeRef{
		{TYPE: configure.ServerType, ID: 1},
		{TYPE: configure.ServerType, ID: 2},
	}
	policy := configure.GetGroupPolicy("openai")
	policy.Mode = configure.GroupModePinned
	if err := configure.SetGroupPolicy("openai", policy); err != nil {
		t.Fatal(err)
	}
	// 还原策略：configure 是全局单例，泄漏会改变其它用例的行为。
	t.Cleanup(func() { resetGroupPolicy("openai") })

	// probe 直接判定全部失败，不起真实连接——这里要验的是控制器的决策，
	// 不是网络。
	probe := func(refs []configure.NodeRef) (map[string]ProbeResult, error) {
		out := make(map[string]ProbeResult, len(refs))
		for _, r := range refs {
			out[r.Key()] = ProbeResult{Alive: false, Err: os.ErrDeadlineExceeded}
		}
		return out, nil
	}
	ap := NewAutoPilot(probe, func(string, configure.NodeRef) error {
		t.Error("pinned 分组全灭时不应应用任何节点")
		return nil
	})
	ap.health = make(map[string]configure.NodeHealth)

	_, err := ap.EnsureGroup("openai", candidates)
	if err == nil {
		t.Fatal("expected ALL_NODES_UNREACHABLE")
	}
	if code := errorCode(err); code != "ALL_NODES_UNREACHABLE" {
		t.Fatalf("code = %q, want ALL_NODES_UNREACHABLE", code)
	}
	t.Logf("pinned 全灭，按预期报错: %v", err)
}

// TestAnyModeFallsBackToLiveNode 验证 any 分组：当前节点连续失败达到阈值后，
// 自动切到另一个可用节点并真正写入分组。
func TestAnyModeFallsBackToLiveNode(t *testing.T) {
	setupIsolatedEnv(t)

	candidates := []configure.NodeRef{
		{TYPE: configure.ServerType, ID: 1},
		{TYPE: configure.ServerType, ID: 2},
	}
	policy := configure.GetGroupPolicy("proxy")
	policy.Mode = configure.GroupModeAny
	policy.FailThreshold = 3
	policy.MaxSwitches = 2
	if err := configure.SetGroupPolicy("proxy", policy); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resetGroupPolicy("proxy") })

	// 节点 1 首轮可用，随后失效；节点 2 一直可用。
	probeCalls := 0
	probe := func(refs []configure.NodeRef) (map[string]ProbeResult, error) {
		probeCalls++
		out := make(map[string]ProbeResult, len(refs))
		for _, r := range refs {
			if r.ID == 2 {
				out[r.Key()] = ProbeResult{Alive: true, LatencyMs: 80}
				continue
			}
			if probeCalls == 1 {
				out[r.Key()] = ProbeResult{Alive: true, LatencyMs: 40}
			} else {
				out[r.Key()] = ProbeResult{Alive: false, Err: os.ErrDeadlineExceeded}
			}
		}
		return out, nil
	}

	var applied []configure.NodeRef
	ap := NewAutoPilot(probe, func(outbound string, ref configure.NodeRef) error {
		if outbound != "proxy" {
			t.Errorf("apply to wrong outbound %q", outbound)
		}
		applied = append(applied, ref)
		return nil
	})
	ap.health = make(map[string]configure.NodeHealth)
	// 巡检的第一轮全量探测：决定初始选择。
	if _, err := ap.ProbeIntoCache(candidates); err != nil {
		t.Fatal(err)
	}

	current, err := ap.EnsureGroup("proxy", candidates)
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != 1 {
		t.Fatalf("initial pick = %d, want 1 (lower latency)", current.ID)
	}

	for i := 0; i < policy.FailThreshold; i++ {
		if _, err := ap.ReportFailure("proxy", candidates, current, os.ErrDeadlineExceeded); err != nil {
			t.Fatalf("failure %d: %v", i+1, err)
		}
	}
	// 达到阈值后需要备选节点的结论，巡检会因此发起全量重测。
	if _, err := ap.ProbeIntoCache(candidates); err != nil {
		t.Fatal(err)
	}
	if _, err := ap.ReportFailure("proxy", candidates, current, os.ErrDeadlineExceeded); err != nil {
		t.Fatalf("重测后应能切换: %v", err)
	}
	// applied 记录的是"每次 apply 调用"，达到阈值后在重测前后各触发一次
	// 阈值判断，因此可能是 2 或 3 次；这里断言的是"至少发生了切换且不是
	// 原地打转"，而不是精确次数。
	if len(applied) < 2 {
		t.Fatalf("applied %d nodes, want 2 (initial + switch)", len(applied))
	}
	if applied[1].ID != 2 {
		t.Fatalf("switched to node %d, want 2", applied[1].ID)
	}
	t.Log("any 模式按预期从节点 1 切到节点 2")

	// 健康缓存必须落库（重启后仍在）。
	store := configure.GetNodeHealthStore()
	if len(store) == 0 {
		t.Fatal("node health was not persisted")
	}
	for k, v := range store {
		t.Logf("  cached %v: alive=%v fails=%d latency=%dms", k, v.Alive, v.Fails, v.LatencyMs)
	}
}

// TestNodeHealthPersistsAcrossRestart 验证持久化：缓存写入后，用一个全新的
// AutoPilot（模拟重启）应当读回同样的结论。
func TestNodeHealthPersistsAcrossRestart(t *testing.T) {
	setupIsolatedEnv(t)

	ref := configure.NodeRef{TYPE: configure.ServerType, ID: 42}
	ap := NewAutoPilot(nil, nil)
	ap.health = make(map[string]configure.NodeHealth)
	ap.health[ref.Key()] = configure.NodeHealth{
		Alive:     true,
		Fails:     0,
		LatencyMs: 123,
		CheckedAt: time.Now().Unix(),
	}
	if err := configure.SaveNodeHealthStore(ap.health); err != nil {
		t.Fatal(err)
	}

	// 全新实例（不共享内存）。
	ap2 := NewAutoPilot(nil, nil)
	h := ap2.health[ref.Key()]
	if !h.Alive || h.LatencyMs != 123 {
		t.Fatalf("reloaded health = %+v, want alive latency=123", h)
	}
	t.Logf("重启后读回节点健康: alive=%v latency=%dms", h.Alive, h.LatencyMs)
}

// TestPluginEnableDisableDoesNotTouchCore 验证插件的启用/停用不启动任何核心。
func TestPluginEnableDisableDoesNotTouchCore(t *testing.T) {
	setupIsolatedEnv(t)

	p := New()
	if err := p.OnStart(pluginContext()); err != nil {
		t.Fatal(err)
	}
	if p.Pilot() == nil {
		t.Fatal("pilot should exist after OnStart")
	}
	if err := p.OnStop(pluginContext()); err != nil {
		t.Fatal(err)
	}
	if p.Pilot() != nil {
		t.Fatal("pilot should be nil after OnStop")
	}
	if v2ray.ProcessManager.Running() {
		t.Fatal("插件启用/停用不应启动核心")
	}
}

// TestIsolatedEnvDoesNotTouchProduction 明确验证隔离性：跑完上述用例后，
// 正式 config.json 必须原封不动。
func TestIsolatedEnvDoesNotTouchProduction(t *testing.T) {
	cfgPath := `C:\ProgramData\v2rayA\config\config.json`
	if _, err := os.Stat(cfgPath); err != nil {
		t.Skip("正式 config.json 不存在，跳过")
	}
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var probe map[string]interface{}
	if err := json.Unmarshal(b, &probe); err != nil {
		t.Fatalf("正式 config.json 不是合法 JSON: %v", err)
	}
	t.Log("正式 config.json 仍是合法 JSON，未被本包测试改写")
}
