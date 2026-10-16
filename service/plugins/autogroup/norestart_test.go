package autogroup

// 验证自动决策路径不再触发核心重启。
//
// 这是这次事故的核心回归测试。现场表现：核心每 9-10 秒重启一次，因为
// EnsureGroup 在缓存为空时会调 probeLocked，而 probeLocked 底层的
// TestHttpLatency 会 Start 一个探测模板再 UpdateV2RayConfig 恢复 ——
// 每次探测 = 核心重启两次。自动路径每轮都探测，于是成了永动机。
//
// 判据很简单：让探测函数计数，断言"只读缓存的决策"一次都不调它。

import (
	"errors"
	"testing"
	"time"

	"github.com/v2rayA/v2rayA/db/configure"
)

// countingProbe 记录被调用的次数，同时给出可配置的结论。
type countingProbe struct {
	calls int
	alive bool
}

func (c *countingProbe) probe(refs []configure.NodeRef) (map[string]ProbeResult, error) {
	c.calls++
	out := make(map[string]ProbeResult, len(refs))
	for _, r := range refs {
		out[r.Key()] = ProbeResult{Alive: c.alive, LatencyMs: 10}
	}
	return out, nil
}

// TestEnsureGroupDoesNotProbe 是本文件最重要的一条：
// 缓存为空时 EnsureGroup 必须直接返回，而不是自己去探测。
// 探测会重启核心，一旦发生在自动路径上就会形成重载环。
func TestEnsureGroupDoesNotProbe(t *testing.T) {
	resetGroupPolicy(testGroup)
	t.Cleanup(func() { resetGroupPolicy(testGroup) })

	cp := &countingProbe{alive: true}
	ap := NewAutoPilot(cp.probe, func(string, configure.NodeRef) error { return nil })
	ap.health = make(map[string]configure.NodeHealth)

	// 缓存是空的（新建的控制器），冷启动场景。
	_, err := ap.EnsureGroup(testGroup, wdCandidates())
	if err == nil {
		t.Log("缓存为空时返回了错误，符合预期（调用方会等巡检）")
	}
	if cp.calls != 0 {
		t.Errorf("EnsureGroup 不应探测（会重启核心），实际探测了 %d 次", cp.calls)
	}
}

// TestReportFailureDoesNotProbe 覆盖切换路径：达到阈值后只读缓存，
// 不自己探测。
func TestReportFailureDoesNotProbe(t *testing.T) {
	resetGroupPolicy(testGroup)
	t.Cleanup(func() { resetGroupPolicy(testGroup) })

	cp := &countingProbe{alive: true}
	applied := 0
	ap := NewAutoPilot(cp.probe, func(string, configure.NodeRef) error {
		applied++
		return nil
	})
	ap.health = make(map[string]configure.NodeHealth)
	ap.now = fakeClock(norestartNow)

	candidates := wdCandidates()
	// 预热缓存：模拟巡检已经探测过一轮。
	if _, err := ap.ProbeIntoCache(candidates); err != nil {
		t.Fatal(err)
	}
	probeCallsAfterRefresh := cp.calls
	if probeCallsAfterRefresh == 0 {
		t.Fatal("预热的 ProbeIntoCache 应该调用探测")
	}

	// 连续上报失败直到触发切换。
	for i := 0; i < 6; i++ {
		_, _ = ap.ReportFailure(testGroup, candidates, candidates[0], errors.New("boom"))
	}

	if cp.calls != probeCallsAfterRefresh {
		t.Errorf("ReportFailure 期间不应探测，实际多了 %d 次", cp.calls-probeCallsAfterRefresh)
	}
	_ = applied
}

// TestProbeIntoCacheIsTheOnlyProbePath 确认唯一允许探测的入口是
// ProbeIntoCache（巡检调用它），且它只写缓存、不切换。
func TestProbeIntoCacheIsTheOnlyProbePath(t *testing.T) {
	resetGroupPolicy(testGroup)
	t.Cleanup(func() { resetGroupPolicy(testGroup) })

	cp := &countingProbe{alive: true}
	applied := 0
	ap := NewAutoPilot(cp.probe, func(string, configure.NodeRef) error {
		applied++
		return nil
	})
	ap.health = make(map[string]configure.NodeHealth)

	n, err := ap.ProbeIntoCache(wdCandidates())
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("3 个候选应全部可用，实际 %d", n)
	}
	if cp.calls != 1 {
		t.Errorf("应只探测一次，实际 %d", cp.calls)
	}
	if applied != 0 {
		t.Errorf("ProbeIntoCache 只刷新缓存，不应触发切换，实际 %d 次", applied)
	}
}

// TestEnsureGroupUsesCacheAfterRefresh 验证闭环：巡检刷新缓存后，
// EnsureGroup 能从缓存里挑出节点，全程不再探测。
func TestEnsureGroupUsesCacheAfterRefresh(t *testing.T) {
	resetGroupPolicy(testGroup)
	t.Cleanup(func() { resetGroupPolicy(testGroup) })

	cp := &countingProbe{alive: true}
	ap := NewAutoPilot(cp.probe, func(string, configure.NodeRef) error { return nil })
	ap.health = make(map[string]configure.NodeHealth)
	ap.now = fakeClock(norestartNow)

	candidates := wdCandidates()
	if _, err := ap.ProbeIntoCache(candidates); err != nil {
		t.Fatal(err)
	}
	before := cp.calls

	chosen, err := ap.EnsureGroup(testGroup, candidates)
	if err != nil {
		t.Fatalf("缓存已预热，EnsureGroup 应成功: %v", err)
	}
	if chosen.Key() == "" {
		t.Error("应选出一个节点")
	}
	if cp.calls != before {
		t.Errorf("命中缓存时不应再探测，实际多了 %d 次", cp.calls-before)
	}
}

// norestartNow 是这些用例共用的固定时刻。
var norestartNow = time.Unix(1_760_000_000, 0)
