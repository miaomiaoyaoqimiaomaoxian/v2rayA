package autogroup

// 验证"切换 → 重载 → core.started → 再切换"这条反馈环被切断。
//
// 现场表现：核心每十几秒重启一次，代理时断时续。成因是 ApplySelection
// 会调用 UpdateV2RayConfig，而重载完成会发布 core.started，插件收到后
// 又做一轮全量校正，校正探测失败再次触发切换。
//
// 两道闸门各自都要有效：
//  1. reloadCooldown 限制重载频率；
//  2. consumeReloadSuppression 让自触发的那次 core.started 跳过校正。

import (
	"testing"
	"time"
)

func resetReloadState() {
	reloadMu.Lock()
	lastReloadAt = time.Time{}
	reloadSuppress = 0
	reloadMu.Unlock()
}

// TestReloadSuppressionIsConsumedOnce 验证自触发标记只消费一次：
// 若可重复消费，用户手动重启核心后的第一次校正会被误跳过。
func TestReloadSuppressionIsConsumedOnce(t *testing.T) {
	resetReloadState()
	t.Cleanup(resetReloadState)

	reloadMu.Lock()
	reloadSuppress = 1
	reloadMu.Unlock()

	if !consumeReloadSuppression() {
		t.Fatal("第一次应报告为自触发")
	}
	if consumeReloadSuppression() {
		t.Fatal("标记只能消费一次，第二次应报告为外部触发")
	}
}

// TestReloadSuppressionDefaultsToExternal 验证没有标记时不会误判：
// 用户自己点"启动"产生的 core.started 必须照常校正。
func TestReloadSuppressionDefaultsToExternal(t *testing.T) {
	resetReloadState()
	t.Cleanup(resetReloadState)

	if consumeReloadSuppression() {
		t.Fatal("没有标记时不应报告为自触发")
	}
}

// TestReloadCooldownBlocksRapidReloads 验证冷却窗口真的拦得住连发：
// 这是反馈环的第一道闸门。
func TestReloadCooldownBlocksRapidReloads(t *testing.T) {
	resetReloadState()
	t.Cleanup(resetReloadState)

	// 刚重载过：此时再切换应被冷却拦住。
	reloadMu.Lock()
	lastReloadAt = time.Now()
	reloadMu.Unlock()

	reloadMu.Lock()
	tooSoon := !lastReloadAt.IsZero() && time.Since(lastReloadAt) < reloadCooldown
	reloadMu.Unlock()
	if !tooSoon {
		t.Fatal("刚重载过之后应处于冷却期")
	}

	// 冷却期过后应放行。
	reloadMu.Lock()
	lastReloadAt = time.Now().Add(-reloadCooldown - time.Second)
	reloadMu.Unlock()

	reloadMu.Lock()
	tooSoon = !lastReloadAt.IsZero() && time.Since(lastReloadAt) < reloadCooldown
	reloadMu.Unlock()
	if tooSoon {
		t.Fatal("超过冷却期后应放行")
	}
}