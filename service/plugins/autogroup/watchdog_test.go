package autogroup

// 验证巡检确实驱动切换，而不只是"探测看起来能工作"。
//
// 这些用例回答一个具体问题：ReportFailure / ReportSuccess 到底是自动切换的
// 驱动方，还是一组没人调用的死接口。在加入巡检之前，它们只被测试调用，
// 生产路径上一次也没有出现——失败计数永远停在 0，节点挂了也不会切，
// 这正是 google 分组的节点宕机后没有任何动作的原因。
//
// 巡检就是为了接上这条链路，所以必须有测试证明链路是通的。

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/v2rayA/v2rayA/db/configure"
)

// newWatchdogPilot 构造一个带隔离健康缓存的控制器，并记录被应用的节点。
func newWatchdogPilot(t *testing.T) (*AutoPilot, *[]configure.NodeRef) {
	t.Helper()
	return setupAutoPilot(t, allAlive(10))
}

// testGroup 是本文件用到的分组名；configure 是全局单例，用固定名字避免
// 策略从一个用例泄漏到另一个。
const testGroup = "watchdog-test"

func wdCandidates() []configure.NodeRef {
	return newTestNodes(3)
}

func failures(t *testing.T, ap *AutoPilot, key string) int {
	t.Helper()
	ap.mu.Lock()
	defer ap.mu.Unlock()
	return ap.health[key].Fails
}

// TestWatchdogReportsFailureOnDeadNode 是这次修复的核心回归测试：
// 探测失败必须走到 ReportFailure，从而让失败计数增长。
//
// 加入巡检之前这条链路不存在，本用例描述的正是当时缺失的行为。
func TestWatchdogReportsFailureOnDeadNode(t *testing.T) {
	ap, _ := newWatchdogPilot(t)
	candidates := wdCandidates()
	resetGroupPolicy(testGroup)

	// 一个必定失败的探测地址。
	dead := &http.Client{Timeout: 150 * time.Millisecond}
	wd := NewWatchdog(ap, time.Second, "http://127.0.0.1:1/never")
	wd.clientForTest = func() *http.Client { return dead }

	before := failures(t, ap, candidates[0].Key())
	wd.reportOutcome(Group{Name: testGroup, Candidates: candidates}, candidates[0], dead)
	after := failures(t, ap, candidates[0].Key())

	if after <= before {
		t.Errorf("探测失败没有上报给控制器：失败计数 before=%d after=%d", before, after)
	}
}

// TestWatchdogReportsSuccessOnHealthyNode 验证反方向：探测成功会清零失败
// 计数。只测失败方向会漏掉"成功被当成失败"这类反向错误。
func TestWatchdogReportsSuccessOnHealthyNode(t *testing.T) {
	ap, _ := newWatchdogPilot(t)
	candidates := wdCandidates()
	resetGroupPolicy(testGroup)

	// 先制造一次失败计数作为前置条件。
	if _, err := ap.ReportFailure(testGroup, candidates, candidates[0], errors.New("boom")); err != nil {
		t.Fatal(err)
	}
	if got := failures(t, ap, candidates[0].Key()); got == 0 {
		t.Fatal("前置条件不成立：失败计数仍为 0")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	wd := NewWatchdog(ap, time.Second, srv.URL)
	wd.clientForTest = func() *http.Client { return srv.Client() }
	wd.reportOutcome(Group{Name: testGroup, Candidates: candidates}, candidates[0], srv.Client())

	if got := failures(t, ap, candidates[0].Key()); got != 0 {
		t.Errorf("探测成功后失败计数应清零，实际为 %d", got)
	}
}

// TestWatchdogSwitchesAfterRepeatedFailures 验证完整链路：节点持续不可用时，
// 达到阈值后真的换到另一个节点。
//
// 这是用户实际遇到的场景，所以这里断言的是"换了吗"，而不只是"计数加了吗"。
func TestWatchdogSwitchesAfterRepeatedFailures(t *testing.T) {
	ap, applied := setupAutoPilot(t, allAlive(10))
	candidates := wdCandidates()
	resetGroupPolicy(testGroup)

	dead := &http.Client{Timeout: 100 * time.Millisecond}
	wd := NewWatchdog(ap, time.Second, "http://127.0.0.1:1/never")
	wd.clientForTest = func() *http.Client { return dead }

	// 跑满一个完整的失败阈值。
	for i := 0; i < 5; i++ {
		wd.reportOutcome(Group{Name: testGroup, Candidates: candidates}, candidates[0], dead)
	}

	if len(*applied) == 0 {
		t.Fatal("节点连续失败后没有任何切换发生")
	}
}

// TestWatchdogAlertRaisedOncePerOutage 验证"全部不可用"只报一次，
// 否则每 30 秒重复同一条错误会把日志淹掉。
func TestWatchdogAlertRaisedOncePerOutage(t *testing.T) {
	ap, _ := newWatchdogPilot(t)
	wd := NewWatchdog(ap, time.Second, "http://127.0.0.1:1/never")

	wd.raise("google", errors.New("no route"))
	wd.raise("google", errors.New("no route"))
	if got := wd.Alerts(); len(got) != 1 || got[0] != "google" {
		t.Errorf("同一故障应只登记一次 google，实际 %v", got)
	}

	// 恢复后应能再次上报。
	wd.clearAlert("google")
	wd.raise("google", errors.New("no route again"))
	if got := wd.Alerts(); len(got) != 1 {
		t.Errorf("恢复后重新故障应能再次上报，实际 %v", got)
	}
}

// TestWatchdogStopIsIdempotent 验证重复 Stop 不会 panic：
// 停用插件与退出流程都可能调它。
func TestWatchdogStopIsIdempotent(t *testing.T) {
	ap, _ := newWatchdogPilot(t)
	wd := NewWatchdog(ap, 10*time.Millisecond, "http://127.0.0.1:1/never")
	wd.clientForTest = func() *http.Client { return &http.Client{Timeout: 20 * time.Millisecond} }
	wd.Start()
	time.Sleep(40 * time.Millisecond)
	wd.Stop()
	wd.Stop() // 重复调用不应 panic
}

// TestWatchdogProbeGoesThroughLocalProxy 验证探测确实经本机 socks 端口，
// 而不是直连——直连探测在 TUN 环境下会测出错误结论。
func TestWatchdogProbeGoesThroughLocalProxy(t *testing.T) {
	c := buildProbeClient()
	if c == nil {
		t.Skip("本机未配置 socks 端口")
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr.Proxy == nil {
		t.Fatal("探测客户端必须经代理")
	}
	req, _ := http.NewRequest(http.MethodGet, "http://example.test", nil)
	got, err := tr.Proxy(req)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("探测客户端的代理为空，等同于直连")
	}
	if got.Port() != "20170" {
		t.Errorf("探测应走本机 socks5 端口 20170，实际 %v", got)
	}
}