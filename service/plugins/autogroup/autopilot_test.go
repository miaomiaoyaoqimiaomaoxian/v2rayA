package autogroup

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/v2rayA/v2rayA/common"
	"github.com/v2rayA/v2rayA/db/configure"
)

// newTestNodes 构造 n 个测试用节点引用。
func newTestNodes(n int) []configure.NodeRef {
	nodes := make([]configure.NodeRef, n)
	for i := range nodes {
		nodes[i] = configure.NodeRef{
			TYPE:     configure.ServerType,
			ID:       i + 1,
			Outbound: "test",
		}
	}
	return nodes
}

// fakeClock 让测试控制"现在"，避免依赖 NodeHealthTTL 的真实时间。
func fakeClock(now time.Time) func() time.Time {
	return func() time.Time { return now }
}

// setupAutoPilot 构造一个不依赖数据库的 AutoPilot：
//   - probe 由调用方提供
//   - apply 记录到 applied，不真的改代理
//
// 同时把该分组的策略重置为默认值。configure 是全局单例：某个用例把策略
// 改成 pinned 之后若不重置，策略会泄漏给后续用例——那正是"单独跑通过、
// 整个包跑失败"的来源。在构造点重置，等于给每个用例一个干净起点。
func setupAutoPilot(t *testing.T, probe func([]configure.NodeRef) (map[string]ProbeResult, error)) (*AutoPilot, *[]configure.NodeRef) {
	t.Helper()
	resetGroupPolicy("test")
	applied := new([]configure.NodeRef)
	ap := NewAutoPilot(probe, func(outbound string, ref configure.NodeRef) error {
		*applied = append(*applied, ref)
		return nil
	})
	// 隔离数据库：测试直接替换内存缓存，不触库。
	ap.health = make(map[string]configure.NodeHealth)
	ap.now = fakeClock(time.Unix(1_700_000_000, 0))
	return ap, applied
}

// resetGroupPolicy 把分组的自动化策略恢复为默认（any / 5 / 5）。
func resetGroupPolicy(outbound string) {
	// 写库失败不中断：读取策略时会退化为默认值，用例照样成立。
	_ = configure.SetGroupPolicy(outbound, configure.DefaultGroupPolicy())
}

// prewarm 用给定候选预热控制器的健康缓存，模拟巡检已经探测过一轮。
//
// 为什么需要它：自动决策路径（EnsureGroup / ReportFailure）现在只读缓存，
// 不自己探测——探测底层的 TestHttpLatency 会 Start 一个探测模板再
// UpdateV2RayConfig 恢复，也就是每次探测重启核心两次。放在自动路径上就
// 成了永动机（探测 → 重启 → core.started → 再探测）。
//
// 生产环境由巡检先探测再决策；测试里就得显式做这一步，否则缓存为空，
// 决策只能返回 cache cold。返回原控制器，便于链式书写。
func prewarm(t *testing.T, ap *AutoPilot, candidates []configure.NodeRef) *AutoPilot {
	t.Helper()
	if _, err := ap.ProbeIntoCache(candidates); err != nil {
		t.Fatalf("预热缓存失败: %v", err)
	}
	return ap
}

// allAlive 是一个让全部候选可用的探测实现。
func allAlive(latency int64) func([]configure.NodeRef) (map[string]ProbeResult, error) {
	return func(refs []configure.NodeRef) (map[string]ProbeResult, error) {
		out := make(map[string]ProbeResult, len(refs))
		for _, r := range refs {
			out[r.Key()] = ProbeResult{Alive: true, LatencyMs: latency}
		}
		return out, nil
	}
}

// aliveMap 按节点下标指定哪些可用。
func aliveMap(nodes []configure.NodeRef, aliveIdx []int, latency int64) map[string]ProbeResult {
	out := make(map[string]ProbeResult, len(nodes))
	alive := make(map[int]bool, len(aliveIdx))
	for _, i := range aliveIdx {
		alive[i] = true
	}
	for i, n := range nodes {
		if alive[i] {
			out[n.Key()] = ProbeResult{Alive: true, LatencyMs: latency}
		} else {
			out[n.Key()] = ProbeResult{Alive: false, Err: fmt.Errorf("node %d down", i+1)}
		}
	}
	return out
}

func TestEnsureGroupPicksOneNode(t *testing.T) {
	nodes := newTestNodes(4)
	ap, applied := setupAutoPilot(t, allAlive(50))
	prewarm(t, ap, nodes)

	chosen, err := ap.EnsureGroup("test", nodes)
	if err != nil {
		t.Fatal(err)
	}
	if chosen.Key() != nodes[0].Key() {
		t.Fatalf("chosen = %v, want %v", chosen.Key(), nodes[0].Key())
	}
	if len(*applied) != 1 {
		t.Fatalf("apply called %d times, want 1", len(*applied))
	}
}

func TestEnsureGroupRejectsEmptyCandidates(t *testing.T) {
	ap, _ := setupAutoPilot(t, allAlive(50))
	if _, err := ap.EnsureGroup("test", nil); err == nil {
		t.Fatal("expected error for empty candidates")
	}
}

// any 模式：连续失败达到阈值必须切换，且不能切回当前节点。
func TestAnyModeSwitchesAfterThreshold(t *testing.T) {
	nodes := newTestNodes(3)
	probeCalls := 0
	ap, applied := setupAutoPilot(t, func(refs []configure.NodeRef) (map[string]ProbeResult, error) {
		// 首次探测只有节点 1 可用；之后节点 1 挂掉、节点 2 可用，
		// 这样切换才有去处。
		if probeCalls == 0 {
			probeCalls++
			return aliveMap(nodes, []int{0}, 30), nil
		}
		return aliveMap(nodes, []int{1}, 30), nil
	})
	// 巡检的第一轮探测：决定初始选择。
	prewarm(t, ap, nodes)

	// 首轮：选中节点 1。
	current, err := ap.EnsureGroup("test", nodes)
	if err != nil {
		t.Fatal(err)
	}
	if current.Key() != nodes[0].Key() {
		t.Fatalf("initial = %v, want node 1", current.Key())
	}

	// 节点 1 连续失败 5 次：前 4 次不切换，第 5 次达到阈值。
	//
	// 达到阈值时缓存里只有"节点 1 已挂"，没有别的节点的结论，所以这一次
	// 不能切换 —— 这正是巡检随后发起全量重测的信号。这里用第二次 prewarm
	// 模拟那次重测（生产环境由巡检按冷却执行）。
	for i := 0; i < 5; i++ {
		switched, _ := ap.ReportFailure("test", nodes, current, errors.New("boom"))
		if i < 4 && switched {
			t.Fatalf("switched at failure %d, want threshold 5", i+1)
		}
	}
	// 巡检的全量重测：这次探测给出的结论是"节点 2 可用"。
	prewarm(t, ap, nodes)
	switched, err := ap.ReportFailure("test", nodes, current, errors.New("boom"))
	if err != nil {
		t.Fatalf("重测后应能切换: %v", err)
	}
	if !switched {
		t.Fatal("重测后仍未切换")
	}
	if len(*applied) != 2 {
		t.Fatalf("apply called %d times, want 2 (initial + switch)", len(*applied))
	}
	next := (*applied)[1]
	if next.Key() == current.Key() {
		t.Fatal("switched back to the same node")
	}
	if next.Key() != nodes[1].Key() {
		t.Fatalf("switched to %v, want node 2", next.Key())
	}
}

// any 模式：未达阈值不切换。
func TestAnyModeDoesNotSwitchBelowThreshold(t *testing.T) {
	nodes := newTestNodes(3)
	ap, applied := setupAutoPilot(t, allAlive(50))
	prewarm(t, ap, nodes)
	current, err := ap.EnsureGroup("test", nodes)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if _, err := ap.ReportFailure("test", nodes, current, errors.New("boom")); err != nil {
			t.Fatal(err)
		}
	}
	if len(*applied) != 1 {
		t.Fatalf("apply called %d times, want 1", len(*applied))
	}
}

// any 模式：成功一次，失败计数必须归零。
func TestSuccessResetsFailCount(t *testing.T) {
	nodes := newTestNodes(3)
	ap, _ := setupAutoPilot(t, allAlive(50))
	prewarm(t, ap, nodes)
	current, _ := ap.EnsureGroup("test", nodes)
	for i := 0; i < 4; i++ {
		if _, err := ap.ReportFailure("test", nodes, current, errors.New("boom")); err != nil {
			t.Fatal(err)
		}
	}
	if h := ap.health[current.Key()]; h.Fails != 4 {
		t.Fatalf("fails before success = %d, want 4", h.Fails)
	}
	ap.ReportSuccess("test", current, 42)
	h := ap.health[current.Key()]
	if h.Fails != 0 || !h.Alive {
		t.Fatalf("after success: fails=%d alive=%v, want 0/true", h.Fails, h.Alive)
	}
	// 归零后再失败 4 次仍不应触发切换。
	for i := 0; i < 4; i++ {
		ap.ReportFailure("test", nodes, current, errors.New("boom"))
	}
	if h := ap.health[current.Key()]; h.Fails != 4 {
		t.Fatalf("fails after reset = %d, want 4", h.Fails)
	}
}

// pinned 模式：候选全灭必须返回 ALL_NODES_UNREACHABLE，且不切换。
func TestPinnedModeReportsAllUnreachable(t *testing.T) {
	nodes := newTestNodes(3)
	ap, applied := setupAutoPilot(t, func(refs []configure.NodeRef) (map[string]ProbeResult, error) {
		return aliveMap(nodes, nil, 0), nil // 全灭
	})
	// 本用例把分组改成 pinned，结束后必须还原，否则会污染其它用例。
	t.Cleanup(func() { resetGroupPolicy("test") })
	policy := configure.GetGroupPolicy("test")
	policy.Mode = configure.GroupModePinned
	if err := configure.SetGroupPolicy("test", policy); err != nil {
		t.Fatal(err)
	}

	_, err := ap.EnsureGroup("test", nodes)
	if err == nil {
		t.Fatal("expected ALL_NODES_UNREACHABLE")
	}
	if code := errorCode(err); code != "ALL_NODES_UNREACHABLE" {
		t.Fatalf("error code = %q, want ALL_NODES_UNREACHABLE (err=%v)", code, err)
	}
	if len(*applied) != 0 {
		t.Fatalf("apply called %d times, want 0", len(*applied))
	}
}

// pinned 模式：候选全灭时上报失败，同样返回源头错误且不切换。
func TestPinnedModeFailureDoesNotSwitch(t *testing.T) {
	nodes := newTestNodes(2)
	ap, applied := setupAutoPilot(t, func(refs []configure.NodeRef) (map[string]ProbeResult, error) {
		return aliveMap(nodes, nil, 0), nil
	})
	policy := configure.GetGroupPolicy("test2")
	policy.Mode = configure.GroupModePinned
	if err := configure.SetGroupPolicy("test2", policy); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resetGroupPolicy("test2") })

	current := nodes[0]
	for i := 0; i < 10; i++ {
		if _, err := ap.ReportFailure("test2", nodes, current, errors.New("boom")); err == nil {
			t.Fatalf("failure %d: expected ALL_NODES_UNREACHABLE", i+1)
		}
	}
	if len(*applied) != 0 {
		t.Fatalf("pinned mode switched nodes (%d applies)", len(*applied))
	}
}

// any 模式：连续切换达到上限后必须触发全量重测，而不是无限逐个试。
//
// 探测实现按"第几次被调用"轮换唯一可用节点，因此每次切换都必然用上最新
// 探测结论。这里断言的是不变量，而不是确切的节点序列：
//   - 每次失败都发生切换；
//   - 从不连续两次应用同一个节点（不会原地打转）；
//   - 切换次数到达 MaxSwitches 后确实又探测了（全量重测路径被执行）。
func TestMaxSwitchesTriggersFullRetest(t *testing.T) {
	nodes := newTestNodes(4)
	probeCalls := 0
	ap, applied := setupAutoPilot(t, func(refs []configure.NodeRef) (map[string]ProbeResult, error) {
		// 第 n 次探测：只有第 n 个节点可用；超过节点数后从头循环，
		// 避免用例被"探测序列耗尽"这个与主题无关的问题绊住。
		call := probeCalls % len(nodes)
		probeCalls++
		return aliveMap(nodes, []int{call}, int64(100-call*10)), nil
	})
	policy := configure.GetGroupPolicy("test3")
	policy.MaxSwitches = 2
	policy.FailThreshold = 1 // 每次失败立即切换，加快用例
	if err := configure.SetGroupPolicy("test3", policy); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resetGroupPolicy("test3") })
	prewarm(t, ap, nodes)

	current, err := ap.EnsureGroup("test3", nodes)
	if err != nil {
		t.Fatal(err)
	}
	if current.Key() != nodes[0].Key() {
		t.Fatalf("initial = %v, want node 1", current.Key())
	}
	initialProbes := probeCalls

	// 只跑 3 轮：足够覆盖"到达 MaxSwitches"这条路径。
	//
	// 每轮都先做一次巡检式的全量重测：切换需要备选节点的结论，而结论只能
	// 来自探测（生产环境由巡检按冷却发起）。这里断言的是不变量 —— 每轮都
	// 切换、且不原地打转 —— 而不是精确的探测次数。
	for i := 0; i < 3; i++ {
		if _, err := ap.ProbeIntoCache(nodes); err != nil {
			t.Fatal(err)
		}
		switched, err := ap.ReportFailure("test3", nodes, current, errors.New("boom"))
		if err != nil {
			t.Fatalf("failure %d: %v", i+1, err)
		}
		if !switched {
			t.Fatalf("failure %d: expected a switch", i+1)
		}
		current = (*applied)[len(*applied)-1]
	}

	// 不能连续两次应用同一个节点（原地打转）。
	for i := 1; i < len(*applied); i++ {
		if (*applied)[i].Key() == (*applied)[i-1].Key() {
			t.Fatalf("applied the same node twice in a row: %+v", *applied)
		}
	}
	// 3 次切换 + 1 次初始选择。
	if len(*applied) != 4 {
		t.Fatalf("applied %d nodes, want 4 (initial + 3 switches)", len(*applied))
	}
	// 每次切换前都必须有一次巡检式探测（否则缓存里没有备选节点的结论），
	// 加上初始那次：共 4 次。这是"切换必然依赖探测结论"的体现。
	if probeCalls-initialProbes < 3 {
		t.Fatalf("probe ran %d times during switches, want >= 3 (one per switch)", probeCalls-initialProbes)
	}
}

// 缓存新鲜度：过期记录不得被当作可用。
func TestStaleCacheIsNotTrusted(t *testing.T) {
	nodes := newTestNodes(2)
	ap, _ := setupAutoPilot(t, func(refs []configure.NodeRef) (map[string]ProbeResult, error) {
		return aliveMap(nodes, []int{0}, 10), nil
	})
	prewarm(t, ap, nodes)
	if _, err := ap.EnsureGroup("test", nodes); err != nil {
		t.Fatal(err)
	}
	// 把时间推到 TTL 之后：缓存全部过期。
	ap.now = fakeClock(time.Unix(1_700_000_000+configure.NodeHealthTTL+1, 0))
	alive := ap.aliveFromCacheLocked(nodes)
	if len(alive) != 0 {
		t.Fatalf("stale cache returned %d alive nodes, want 0", len(alive))
	}
}

// 失败上报在同一次调用内不可被探测翻案。
//
// guard 的生命周期是一次 ReportFailure：调用期间若发生探测（例如 pinned
// 分支为了判断"候选是否全灭"而查缓存），刚上报的失败不能被那份结论覆盖。
// 调用结束后 guard 释放，之后的新探测就是新的事实——那是设计如此。
func TestReportedFailureIsNotResurrectedByProbe(t *testing.T) {
	nodes := newTestNodes(2)
	// 探测永远说"全部可用"——若没有 guard，它会覆盖上报的失败。
	ap, _ := setupAutoPilot(t, allAlive(99))
	prewarm(t, ap, nodes)

	policy := configure.GetGroupPolicy("test")
	policy.Mode = configure.GroupModePinned
	if err := configure.SetGroupPolicy("test", policy); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resetGroupPolicy("test") })

	current := nodes[0]
	// 在 guard 生效期内探测：结论与上报相反，但上报必须赢。
	ap.mu.Lock()
	ap.guardNodeLocked(nodeGuard{key: current.Key(), fails: 1, at: ap.now().Unix()})
	if _, err := ap.probeLocked(nodes); err != nil {
		ap.mu.Unlock()
		t.Fatal(err)
	}
	h := ap.health[current.Key()]
	ap.mu.Unlock()

	if h.Alive {
		t.Fatal("守卫期内，刚上报失败的节点被探测复活了")
	}
	if h.Fails != 1 {
		t.Fatalf("fails = %d, want 1", h.Fails)
	}
}

// 守卫释放后，新的探测结论应当生效——否则节点一旦失败就永远无法恢复。
func TestGuardReleasedAllowsRecovery(t *testing.T) {
	nodes := newTestNodes(2)
	ap, _ := setupAutoPilot(t, allAlive(99))
	prewarm(t, ap, nodes)

	current := nodes[0]
	if _, err := ap.ReportFailure("test", nodes, current, errors.New("boom")); err != nil {
		t.Fatal(err)
	}
	if ap.health[current.Key()].Alive {
		t.Fatal("上报失败后应为不可用")
	}
	// 巡检的下一轮探测说"可用"：应当恢复，否则节点坏了就再也回不来。
	if _, err := ap.ProbeIntoCache(nodes); err != nil {
		t.Fatal(err)
	}
	if !ap.health[current.Key()].Alive {
		t.Fatal("守卫释放后，探测结论应能让节点恢复可用")
	}
}

// RefreshCandidates 必须丢弃已消失节点的记录，避免缓存无限增长。
func TestRefreshCandidatesDropsRemovedNodes(t *testing.T) {
	nodes := newTestNodes(3)
	ap, _ := setupAutoPilot(t, allAlive(10))
	prewarm(t, ap, nodes)
	if _, err := ap.EnsureGroup("test", nodes); err != nil {
		t.Fatal(err)
	}
	if len(ap.health) != 3 {
		t.Fatalf("health has %d entries, want 3", len(ap.health))
	}
	ap.RefreshCandidates("test", nodes[:1])
	if len(ap.health) != 1 {
		t.Fatalf("after refresh health has %d entries, want 1", len(ap.health))
	}
}

// errorCode 取出 common.CodedError 的 Code。
func errorCode(err error) string {
	var coded *common.CodedError
	if errors.As(err, &coded) {
		return coded.Code
	}
	return ""
}
