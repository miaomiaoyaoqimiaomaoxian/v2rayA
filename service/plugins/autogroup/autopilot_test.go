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

	// 首轮：选中节点 1。
	current, err := ap.EnsureGroup("test", nodes)
	if err != nil {
		t.Fatal(err)
	}
	if current.Key() != nodes[0].Key() {
		t.Fatalf("initial = %v, want node 1", current.Key())
	}

	// 节点 1 连续失败 5 次：前 4 次不切换，第 5 次切换。
	for i := 0; i < 5; i++ {
		switched, err := ap.ReportFailure("test", nodes, current, errors.New("boom"))
		if err != nil {
			t.Fatalf("failure %d: %v", i+1, err)
		}
		if i < 4 && switched {
			t.Fatalf("switched at failure %d, want threshold 5", i+1)
		}
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

	current, err := ap.EnsureGroup("test3", nodes)
	if err != nil {
		t.Fatal(err)
	}
	if current.Key() != nodes[0].Key() {
		t.Fatalf("initial = %v, want node 1", current.Key())
	}
	initialProbes := probeCalls

	// 只跑 3 轮：足够覆盖"到达 MaxSwitches 触发重测"这条路径。
	for i := 0; i < 3; i++ {
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
	if len(*applied) != 4 {
		t.Fatalf("applied %d nodes, want 4 (initial + 3 switches)", len(*applied))
	}
	// 3 次切换 + 1 次初始 = 至少 4 次探测；到达 MaxSwitches 的那一轮会多探一次。
	if probeCalls-initialProbes < 4 {
		t.Fatalf("probe ran %d times during switches, want >= 4", probeCalls-initialProbes)
	}
}

// 缓存新鲜度：过期记录不得被当作可用。
func TestStaleCacheIsNotTrusted(t *testing.T) {
	nodes := newTestNodes(2)
	ap, _ := setupAutoPilot(t, func(refs []configure.NodeRef) (map[string]ProbeResult, error) {
		return aliveMap(nodes, []int{0}, 10), nil
	})
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

// 失败上报优先于探测：刚被上报失败的节点不能在同轮被探测结果复活。
// 这是 pinned 模式不出现"静默失败"的关键保证。
func TestReportedFailureIsNotResurrectedByProbe(t *testing.T) {
	nodes := newTestNodes(2)
	// 探测永远说"全部可用"——若没有 guard，它会覆盖上报的失败。
	ap, _ := setupAutoPilot(t, allAlive(99))
	policy := configure.GetGroupPolicy("test")
	policy.Mode = configure.GroupModePinned
	if err := configure.SetGroupPolicy("test", policy); err != nil {
		t.Fatal(err)
	}

	current := nodes[0]
	if _, err := ap.ReportFailure("test", nodes, current, errors.New("boom")); err != nil {
		t.Fatal(err)
	}
	h := ap.health[current.Key()]
	if h.Alive {
		t.Fatal("a node reported as failed was resurrected by the probe")
	}
	if h.Fails != 1 {
		t.Fatalf("fails = %d, want 1", h.Fails)
	}
}

// RefreshCandidates 必须丢弃已消失节点的记录，避免缓存无限增长。
func TestRefreshCandidatesDropsRemovedNodes(t *testing.T) {
	nodes := newTestNodes(3)
	ap, _ := setupAutoPilot(t, allAlive(10))
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
