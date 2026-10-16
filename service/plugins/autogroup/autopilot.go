// Package autogroup 实现自动分组控制器。
//
// 它被 service/plugins/autogroup 包装成插件；这个文件只包含纯决策逻辑，
// 不依赖 gin、不依赖核心进程，因此可以在测试里完整跑通。
package autogroup

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/v2rayA/v2rayA/common"
	"github.com/v2rayA/v2rayA/db/configure"
	"github.com/v2rayA/v2rayA/pkg/util/log"
)

// AutoPilot 是自动分组控制器。
//
// 它实现两种行为模式：
//
//   - GroupModeAny（PROXY 类）：目标是"有节点能用就行"。分组始终只配一个
//     节点；该节点连续失败 FailThreshold 次后切到下一个可用节点；连续切换
//     MaxSwitches 次仍失败时，对候选集做一次全量重测并重选。全程不需要用户
//     介入，也不向用户报"源头错误"——换一个节点本就是这类分组的解法。
//
//   - GroupModePinned（OPENAI / GMINI 类）：只用候选集内的节点，且要求出口
//     地区等属性正确。这类分组同样只配一个节点，但候选集之外的节点永不启用；
//     候选集全部失败时不切换、不兜底，而是返回一个明确的错误让用户知道
//     "所有节点均无法访问"——那是源头上的错误，换分组只会掩盖它。
//
// 控制器只做决策，不自己发探测请求：探测由 probe 函数注入，生产环境走
// TestHttpLatency，测试走假实现。
type AutoPilot struct {
	mu sync.Mutex

	// now 便于测试注入假时钟。
	now func() time.Time

	// probe 对一组节点做可用性探测，返回每个节点的探测结果。
	// 实现方负责并发与超时；控制器只消费结论。
	probe func(refs []configure.NodeRef) (map[string]ProbeResult, error)

	// apply 把一个节点写入某个分组并让代理生效。
	// 生产环境走 ReplaceOutboundConnections + UpdateV2RayConfig。
	apply func(outbound string, ref configure.NodeRef) error

	// health 是节点健康状态缓存，内存态 + 持久化。
	health map[string]configure.NodeHealth

	// switchCount 记录每个分组本次连续切换的次数，达到 MaxSwitches 后
	// 触发全量重测。重启后归零——重测一次比记住一次更可靠。
	switchCount map[string]int

	// guarded 记录"调用方刚上报失败、本轮探测不得翻案"的节点。
	// 只由 ReportFailure 在持锁期间设置与清除。
	guarded map[string]nodeGuard

	// log 是决策日志记录器，负责回答"当时为什么这么决定"。
	// 为 nil 时全部日志调用都是空操作（测试可省略）。
	log *Logger
}

// SetLogger 注入决策日志记录器。
func (a *AutoPilot) SetLogger(l *Logger) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.log = l
}

// nodeGuard 标记一次"已上报失败"的节点，阻止同轮探测把它复活。
// key 冗余于 map 的键，是为了让调用点写起来更直白。
type nodeGuard struct {
	key   string
	fails int
	at    int64
}

// ProbeResult 是一次节点探测的结论。
type ProbeResult struct {
	Alive     bool
	LatencyMs int64
	Err       error
}

// NewAutoPilot 构造控制器。probe/apply 为 nil 时退化为"永远未知/不生效"，
// 仅用于测试。
func NewAutoPilot(
	probe func([]configure.NodeRef) (map[string]ProbeResult, error),
	apply func(string, configure.NodeRef) error,
) *AutoPilot {
	return &AutoPilot{
		now:         time.Now,
		probe:       probe,
		apply:       apply,
		health:      configure.GetNodeHealthStore(),
		switchCount: make(map[string]int),
		guarded:     make(map[string]nodeGuard),
	}
}

// AllNodesUnreachable 是 pinned 分组候选集全部失败时返回的错误。
// 它的语义是"源头错误"，调用方应把它直接呈现给用户，而不是尝试别的分组。
//
// 它包装 common.CodedError，因此 HTTP 层能透出稳定的错误码
// ALL_NODES_UNREACHABLE，前端可以据此显示专门的提示，而不是笼统的失败。
func AllNodesUnreachable(outbound string, candidates []configure.NodeRef, health map[string]configure.NodeHealth) error {
	reasons := make([]string, 0, len(candidates))
	for _, c := range candidates {
		h, ok := health[c.Key()]
		if !ok || h.LastError == "" {
			continue
		}
		reasons = append(reasons, fmt.Sprintf("%v: %v", c.Key(), h.LastError))
	}
	sort.Strings(reasons)
	return common.Coded("ALL_NODES_UNREACHABLE", fmt.Errorf(
		"outbound %q: all %d candidate nodes are unreachable (%s)",
		outbound, len(candidates), strings.Join(reasons, "; ")), map[string]interface{}{
		"outbound": outbound,
		"total":    len(candidates),
		"reasons":  reasons,
	})
}

// EnsureGroup 确保一个分组当前正用一个可用节点。
//
// 这是控制器的主入口，在核心启动前和节点变更后调用：
//   - pinned 模式：候选集为空 -> 报错；有可用节点 -> 挑最优的应用；
//     全灭 -> 返回 AllNodesUnreachable。
//   - any 模式：同样挑最优应用；没有可用节点且从未探测过 -> 先探测再挑；
//     仍无 -> 返回错误（但语义是"暂时没有可用节点"，不是源头错误）。
func (a *AutoPilot) EnsureGroup(outbound string, candidates []configure.NodeRef) (chosen configure.NodeRef, err error) {
	if len(candidates) == 0 {
		return configure.NodeRef{}, fmt.Errorf("outbound %q has no candidate nodes", outbound)
	}
	policy := configure.GetGroupPolicy(outbound)
	started := time.Now()

	a.mu.Lock()
	defer a.mu.Unlock()

	now := a.now().Unix()
	snaps := SnapshotCandidates(candidates, a.health, now)
	base := Event{
		Outbound:      outbound,
		Mode:          policy.Mode.String(),
		FailThreshold: policy.FailThreshold,
		MaxSwitches:   policy.MaxSwitches,
		SwitchCount:   a.switchCount[outbound],
		Candidates:    snaps,
		CacheFresh:    CountFresh(snaps),
	}

	// 1) 只从缓存里挑。
	//
	// 这里刻意不探测。探测走的是 v2rayA 的 TestHttpLatency，而它为了给每个
	// 节点插入临时 inbound，会先 Start 一个探测模板再 UpdateV2RayConfig 恢复
	// —— 也就是每次探测都完整重启核心两次。那是"用户点一次测试延迟"的场景，
	// 一次性开销可以接受；放在自动路径上就成了永动机：探测 → 重启 → 收到
	// core.started → 再协调 → 再探测。
	//
	// 所以分工是：巡检（watchdog）负责探测并刷新缓存，这里只读缓存做决定。
	// 缓存不新鲜就等下一轮巡检，而不是在这里自己发起一次探测。
	alive := a.aliveFromCacheLocked(candidates)
	if len(alive) == 0 {
		ev := base
		ev.Reason = fmt.Sprintf("候选 %d 个，缓存新鲜的可用节点 0 个", len(candidates))
		ev.Candidates = SnapshotCandidates(candidates, a.health, a.now().Unix())
		ev.DurationMs = time.Since(started).Milliseconds()
		// pinned 分组的前提就是"只用这一组固定节点"，全灭必须报源头错误，
		// 不能像 any 模式那样只说一句"没得选"——两者对用户的含义不同。
		if policy.Mode.IsPinned() {
			ev.Decision = "all-unreachable"
			ev.Msg = "pinned 分组候选全部不可用，这是源头错误"
			ev.Err = "所有候选节点均无法访问"
			a.logError(ev)
			return configure.NodeRef{}, AllNodesUnreachable(outbound, candidates, a.health)
		}
		ev.Decision = "no-reachable"
		ev.Msg = "any 模式：缓存里没有可用节点，等巡检刷新"
		a.logWarn(ev)
		return configure.NodeRef{}, fmt.Errorf("outbound %q: no reachable node among %d candidates (cache cold)",
			outbound, len(candidates))
	}

	// 2) 多个可用时挑延迟最低的；延迟相同保持候选集顺序，保证行为可预期。
	chosen = pickBest(alive, a.health)

	ev := base
	ev.Decision = "pick"
	ev.Node = chosen.Key()
	ev.Msg = fmt.Sprintf("从 %d 个可用节点中选中 %s", len(alive), chosen.Key())
	ev.Reason = pickReason(chosen, alive, a.health)
	ev.Candidates = SnapshotCandidates(candidates, a.health, a.now().Unix())
	ev.DurationMs = time.Since(started).Milliseconds()
	if len(alive) > 1 {
		a.logInfo(ev)
	} else {
		a.logDebug(ev)
	}

	if a.apply == nil {
		return chosen, nil
	}
	if err = a.apply(outbound, chosen); err != nil {
		ev = base
		ev.Decision = "apply-failed"
		ev.Node = chosen.Key()
		ev.Msg = "应用选择失败（配置可能未生效）"
		ev.Err = err.Error()
		ev.DurationMs = time.Since(started).Milliseconds()
		a.logError(ev)
		return configure.NodeRef{}, err
	}
	a.switchCount[outbound] = 0
	ev = base
	ev.Decision = "apply"
	ev.Node = chosen.Key()
	ev.Msg = "已应用到分组并生效"
	ev.DurationMs = time.Since(started).Milliseconds()
	a.logDebug(ev)
	return chosen, nil
}

// pickReason 解释"为什么选它"，让日志能回答因果而不只是结论。
func pickReason(chosen configure.NodeRef, alive []configure.NodeRef, health map[string]configure.NodeHealth) string {
	if len(alive) == 1 {
		return "唯一可用节点，无需比较"
	}
	best := health[chosen.Key()].LatencyMs
	if best <= 0 {
		return fmt.Sprintf("在 %d 个可用节点中延迟数据缺失，按候选集顺序取第一个", len(alive))
	}
	others := make([]string, 0, len(alive)-1)
	for _, c := range alive {
		if c.Key() == chosen.Key() {
			continue
		}
		if l := health[c.Key()].LatencyMs; l > 0 {
			others = append(others, fmt.Sprintf("%s=%dms", c.Key(), l))
		}
	}
	if len(others) == 0 {
		return fmt.Sprintf("延迟最低：%dms（其余 %d 个无延迟数据）", best, len(alive)-1)
	}
	return fmt.Sprintf("延迟最低：%dms，其余 %v", best, others)
}

// logDebug / logInfo / logWarn / logError 是安全的日志调用：logger 为 nil 时空操作。
func (a *AutoPilot) logDebug(ev Event) {
	if a.log == nil {
		return
	}
	a.log.Debug(ev)
}

func (a *AutoPilot) logInfo(ev Event) {
	if a.log == nil {
		return
	}
	a.log.Info(ev)
}

func (a *AutoPilot) logWarn(ev Event) {
	if a.log == nil {
		return
	}
	a.log.Warn(ev)
}

func (a *AutoPilot) logError(ev Event) {
	if a.log == nil {
		return
	}
	a.log.Error(ev)
}

// ReportFailure 上报一次"当前节点失败"。
//
// 调用方是探测/观测路径：任何时候确认当前节点不可用，就调这里。
//   - any 模式：累计连续失败，达到 FailThreshold 就换到下一个可用节点；
//     连续切换达到 MaxSwitches 就触发全量重测再选。
//   - pinned 模式：只累计失败并记录原因；不切换。候选集全灭时返回
//     AllNodesUnreachable，让用户看到源头错误。
func (a *AutoPilot) ReportFailure(outbound string, candidates []configure.NodeRef, current configure.NodeRef, reason error) (switched bool, err error) {
	if len(candidates) == 0 {
		return false, fmt.Errorf("outbound %q has no candidate nodes", outbound)
	}
	policy := configure.GetGroupPolicy(outbound)
	started := time.Now()

	a.mu.Lock()
	defer a.mu.Unlock()

	key := current.Key()
	h := a.health[key]
	h.Alive = false
	h.Fails++
	h.CheckedAt = a.now().Unix()
	if reason != nil {
		h.LastError = reason.Error()
	}
	a.health[key] = h
	a.persistLocked()

	base := Event{
		Outbound:      outbound,
		Node:          key,
		Fails:         h.Fails,
		Mode:          policy.Mode.String(),
		FailThreshold: policy.FailThreshold,
		MaxSwitches:   policy.MaxSwitches,
		SwitchCount:   a.switchCount[outbound],
	}
	if reason != nil {
		base.Err = reason.Error()
	}

	// 调用方明确上报了这个节点失败，这一轮的任何探测结果都不能覆盖它：
	// 否则 pinned 模式会在"刚失败的节点"上被探测结果复活，从而既不报错
	// 也不切换，用户看到的是无限期静默失败。
	a.guardNodeLocked(nodeGuard{key: key, fails: h.Fails, at: h.CheckedAt})
	defer a.unguardNodeLocked()

	log.Info("autopilot: outbound %q node %v failed (%d/%d): %v",
		outbound, key, h.Fails, policy.FailThreshold, reason)

	if policy.Mode.IsPinned() {
		// pinned 不切换节点，只在候选集确实全灭时告知源头错误。
		// 只读缓存：探测会重启核心，见 EnsureGroup 里的说明。
		alive := a.aliveFromCacheLocked(candidates)
		if len(alive) == 0 {
			ev := base
			ev.Decision = "all-unreachable"
			ev.Msg = "pinned 分组候选全部不可用，这是源头错误"
			ev.Candidates = SnapshotCandidates(candidates, a.health, a.now().Unix())
			ev.DurationMs = time.Since(started).Milliseconds()
			a.logError(ev)
			return false, AllNodesUnreachable(outbound, candidates, a.health)
		}
		// 还有别的候选活着：pinned 也不换，因为分组语义是固定节点集，
		// 用户显式固定了走谁。真正需要换的时候由 EnsureGroup 处理。
		ev := base
		ev.Decision = "pinned-hold"
		ev.Msg = "pinned 分组不切换节点，仅累计失败"
		ev.Reason = fmt.Sprintf("候选里还有 %d 个可用，pinned 语义要求固定节点集", len(alive))
		ev.Candidates = SnapshotCandidates(candidates, a.health, a.now().Unix())
		ev.DurationMs = time.Since(started).Milliseconds()
		a.logWarn(ev)
		return false, nil
	}

	// any 模式：未达阈值先攒着。
	if h.Fails < policy.FailThreshold {
		ev := base
		ev.Decision = "below-threshold"
		ev.Msg = fmt.Sprintf("失败 %d/%d，未达切换阈值，保持当前节点", h.Fails, policy.FailThreshold)
		ev.DurationMs = time.Since(started).Milliseconds()
		a.logDebug(ev)
		return false, nil
	}

	// 达到阈值。只读缓存取可用集合。
	//
	// 这里同样不探测：切换是自动路径上最频繁的动作，而探测会重启核心
	// （见 EnsureGroup 里的说明）。缓存由巡检刷新，不新鲜就等下一轮，
	// 宁可晚 30 秒切换，也不要让核心每几秒重启一次。
	alive := a.aliveFromCacheLocked(candidates)
	if len(alive) == 0 {
		ev := base
		ev.Decision = "no-alternative"
		ev.Msg = "达到切换阈值，但缓存里没有可用节点"
		ev.Reason = fmt.Sprintf("候选 %d 个，缓存新鲜的可用节点 0 个，等巡检刷新", len(candidates))
		ev.Candidates = SnapshotCandidates(candidates, a.health, a.now().Unix())
		ev.DurationMs = time.Since(started).Milliseconds()
		a.logError(ev)
		return false, fmt.Errorf("outbound %q: every candidate node failed; no node to switch to", outbound)
	}

	a.switchCount[outbound]++
	if a.switchCount[outbound] >= policy.MaxSwitches {
		// 连续切换太多次，说明逐个试没有意义。这里不清缓存、不探测，
		// 只是把计数归零，让下一轮从缓存里重新挑；真正的全量重测由巡检
		// 负责（它是唯一有权探测的路径）。
		log.Info("autopilot: outbound %q reached %d consecutive switches; resetting the counter, the watchdog will re-test",
			outbound, a.switchCount[outbound])
		ev := base
		ev.Decision = "switch-limit"
		ev.Msg = fmt.Sprintf("连续切换 %d 次达到上限，交由巡检重测", a.switchCount[outbound])
		ev.Reason = "逐个试已无意义，重置计数等待巡检给出新结论"
		ev.DurationMs = time.Since(started).Milliseconds()
		a.logWarn(ev)
		a.switchCount[outbound] = 0
	}

	// 从可用集合里挑一个不同于当前节点的。
	next, ok := pickBestExcluding(alive, current, a.health)
	if !ok {
		// 一个能用的都没有。any 模式下这通常意味着候选集整体故障或本地
		// 网络中断，直接报错，让上层知道"现在没有任何出口"。
		ev := base
		ev.Decision = "no-alternative"
		ev.Msg = "达到切换阈值，但没有任何可切换的节点"
		ev.Reason = fmt.Sprintf("候选 %d 个，可用 0 个", len(candidates))
		ev.Candidates = SnapshotCandidates(candidates, a.health, a.now().Unix())
		ev.DurationMs = time.Since(started).Milliseconds()
		a.logError(ev)
		return false, fmt.Errorf("outbound %q: every candidate node failed; no node to switch to", outbound)
	}

	if a.apply == nil {
		return true, nil
	}
	if err = a.apply(outbound, next); err != nil {
		ev := base
		ev.Decision = "apply-failed"
		ev.To = next.Key()
		ev.Msg = "切换失败（配置可能未生效）"
		ev.Err = err.Error()
		ev.DurationMs = time.Since(started).Milliseconds()
		a.logError(ev)
		return false, err
	}

	ev := base
	ev.Decision = "switch"
	ev.From = key
	ev.To = next.Key()
	ev.Msg = fmt.Sprintf("连续失败 %d 次后从 %s 切换到 %s", h.Fails, key, next.Key())
	ev.Reason = pickReason(next, alive, a.health)
	ev.Candidates = SnapshotCandidates(candidates, a.health, a.now().Unix())
	ev.DurationMs = time.Since(started).Milliseconds()
	a.logInfo(ev)

	log.Info("autopilot: outbound %q switched from %v to %v after %d failures",
		outbound, key, next.Key(), h.Fails)
	return true, nil
}

// ReportSuccess 上报一次成功，清零该节点的失败计数。
func (a *AutoPilot) ReportSuccess(outbound string, current configure.NodeRef, latencyMs int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := current.Key()
	h := a.health[key]
	if h.Fails == 0 && h.Alive && h.LatencyMs == latencyMs {
		return // 无变化，省一次写库
	}
	h.Alive = true
	h.Fails = 0
	h.LatencyMs = latencyMs
	h.CheckedAt = a.now().Unix()
	h.LastError = ""
	a.health[key] = h
	a.switchCount[outbound] = 0
	a.persistLocked()
}

// TestCandidates 对一组候选节点做全量探测，返回每个节点的结论。
// pinned 分组的用户主动"重测"走这里。
//
// 注意开销：底层 TestHttpLatency 会重启核心两次。所以只应由显式请求
// （用户点重测）或巡检在冷却后调用，不能放在自动决策路径上。
func (a *AutoPilot) TestCandidates(candidates []configure.NodeRef) (map[string]ProbeResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.runProbeLocked(candidates)
}

// ProbeIntoCache 执行一次全量探测并把结果写进缓存，返回可用节点数。
//
// 这是自动路径上唯一允许探测的入口，由巡检按冷却调用。它不切换、不落库，
// 只刷新缓存，让随后只读缓存的 EnsureGroup / ReportFailure 有据可依。
func (a *AutoPilot) ProbeIntoCache(candidates []configure.NodeRef) (int, error) {
	if len(candidates) == 0 {
		return 0, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	alive, err := a.probeLocked(candidates)
	if err != nil {
		return 0, err
	}
	return len(alive), nil
}

// RefreshCandidates 在节点集合变化后刷新控制器的内部视图。
// 它不 apply、不切换——用户刚改完节点，插件不该立刻把他换走。
func (a *AutoPilot) RefreshCandidates(outbound string, candidates []configure.NodeRef) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(candidates) == 0 {
		return
	}
	// 丢弃已不存在节点的记录，避免缓存无限增长。
	valid := make(map[string]struct{}, len(candidates))
	for _, c := range candidates {
		valid[c.Key()] = struct{}{}
	}
	for k := range a.health {
		if _, ok := valid[k]; !ok {
			delete(a.health, k)
		}
	}
	a.switchCount[outbound] = 0
}

// ResetSwitchCounts 清空所有分组的连续切换计数。
// 核心停止后调用，让下次启动从零开始。
func (a *AutoPilot) ResetSwitchCounts() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.switchCount = make(map[string]int)
}

// Status 返回一个分组的当前状态快照，供前端展示。
func (a *AutoPilot) Status(outbound string, candidates []configure.NodeRef) GroupStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	policy := configure.GetGroupPolicy(outbound)
	st := GroupStatus{
		Outbound:      outbound,
		Mode:          policy.Mode.String(),
		FailThreshold: policy.FailThreshold,
		MaxSwitches:   policy.MaxSwitches,
		Nodes:         make([]NodeStatus, 0, len(candidates)),
	}
	for _, c := range candidates {
		h := a.health[c.Key()]
		st.Nodes = append(st.Nodes, NodeStatus{
			Node:      c,
			Alive:     h.Alive,
			Fails:     h.Fails,
			LatencyMs: h.LatencyMs,
			CheckedAt: h.CheckedAt,
			LastError: h.LastError,
		})
	}
	return st
}

// GroupStatus 是分组状态快照。
type GroupStatus struct {
	Outbound      string       `json:"outbound"`
	Mode          string       `json:"mode"`
	FailThreshold int          `json:"failThreshold"`
	MaxSwitches   int          `json:"maxSwitches"`
	Nodes         []NodeStatus `json:"nodes"`
}

// NodeStatus 是单个节点的状态快照。
type NodeStatus struct {
	Node      configure.NodeRef `json:"node"`
	Alive     bool              `json:"alive"`
	Fails     int               `json:"fails"`
	LatencyMs int64             `json:"latencyMs"`
	CheckedAt int64             `json:"checkedAt"`
	LastError string            `json:"lastError,omitempty"`
}

// ---- 内部方法（调用方持有 a.mu） ----

// aliveFromCacheLocked 从缓存挑出新鲜的可用节点。
// 没有任何新鲜记录时返回空，让调用方去探测而不是猜。
func (a *AutoPilot) aliveFromCacheLocked(candidates []configure.NodeRef) []configure.NodeRef {
	now := a.now().Unix()
	var alive []configure.NodeRef
	for _, c := range candidates {
		h, ok := a.health[c.Key()]
		if !ok || !h.Fresh(now) {
			continue
		}
		if h.Alive {
			alive = append(alive, c)
		}
	}
	return alive
}

// runProbeLocked 执行探测，把结果写回缓存，返回每个节点的原始结论。
// 需要逐个节点原因（如前端展示）的调用方用这个。
func (a *AutoPilot) runProbeLocked(candidates []configure.NodeRef) (map[string]ProbeResult, error) {
	if a.probe == nil {
		return nil, errors.New("autopilot: no probe function configured")
	}
	results, err := a.probe(candidates)
	if err != nil {
		return nil, err
	}
	a.recordProbeLocked(candidates, results)
	return results, nil
}

// probeLocked 执行探测并返回可用节点子集。
func (a *AutoPilot) probeLocked(candidates []configure.NodeRef) ([]configure.NodeRef, error) {
	if a.probe == nil {
		return nil, errors.New("autopilot: no probe function configured")
	}
	results, err := a.probe(candidates)
	if err != nil {
		return nil, err
	}
	a.recordProbeLocked(candidates, results)
	var alive []configure.NodeRef
	for _, c := range candidates {
		if r, ok := results[c.Key()]; ok && r.Alive {
			alive = append(alive, c)
		}
	}
	return alive, nil
}

// guardNodeLocked 标记一个节点为"已上报失败"，让本轮后续探测不能翻案。
func (a *AutoPilot) guardNodeLocked(g nodeGuard) {
	if a.guarded == nil {
		a.guarded = make(map[string]nodeGuard)
	}
	a.guarded[g.key] = g
}

// unguardNodeLocked 解除 ReportFailure 开始时设下的守卫。
func (a *AutoPilot) unguardNodeLocked() {
	for k := range a.guarded {
		delete(a.guarded, k)
	}
}

// recordProbeLocked 把探测结果合并进缓存。
// 探测实现漏掉某个节点时，不动它已有的记录——那是一次未发生的探测，
// 不是一次失败。被 guard 的节点（本轮刚由调用方上报失败）跳过：
// 调用方的事实优先于探测。
func (a *AutoPilot) recordProbeLocked(candidates []configure.NodeRef, results map[string]ProbeResult) {
	now := a.now().Unix()
	for _, c := range candidates {
		r, ok := results[c.Key()]
		if !ok {
			continue
		}
		if g, guarded := a.guarded[c.Key()]; guarded {
			// 守卫期内：只更新延迟这类附带信息，"是否可用"与失败计数
			// 保持调用方上报的结论。
			h := a.health[c.Key()]
			if h.CheckedAt < g.at {
				h.CheckedAt = g.at
			}
			if r.Alive {
				h.LatencyMs = r.LatencyMs
			}
			h.Alive = false
			h.Fails = g.fails
			a.health[c.Key()] = h
			continue
		}
		h := a.health[c.Key()]
		h.Alive = r.Alive
		h.CheckedAt = now
		if r.Alive {
			h.Fails = 0
			h.LatencyMs = r.LatencyMs
			h.LastError = ""
		} else {
			h.Fails++
			if r.Err != nil {
				h.LastError = r.Err.Error()
			}
		}
		a.health[c.Key()] = h
	}
	a.persistLocked()
}

// persistLocked 把内存态健康缓存写库。失败只记日志：缓存失效的代价是
// 重启后重新探测一次，不值得让调用链失败。
func (a *AutoPilot) persistLocked() {
	if err := configure.SaveNodeHealthStore(a.health); err != nil {
		log.Warn("autopilot: cannot persist node health: %v", err)
	}
}

// pickBestExcluding 与 pickBest 相同，但跳过 exclude 指定的节点。
// 切换节点时必须调用它，否则"下一个节点"可能还是当前这个。
//
// alive 的元素必须已经在缓存里有记录（probeLocked / aliveFromCacheLocked
// 都会保证这一点），否则视为无延迟信息，按候选集顺序取第一个。
func pickBestExcluding(alive []configure.NodeRef, exclude configure.NodeRef, health map[string]configure.NodeHealth) (configure.NodeRef, bool) {
	best := configure.NodeRef{}
	bestLatency := int64(0)
	found := false
	for _, c := range alive {
		if exclude.Key() != "" && c.Key() == exclude.Key() {
			continue
		}
		latency := int64(0)
		if health != nil {
			latency = health[c.Key()].LatencyMs
		}
		if !found || (latency > 0 && (bestLatency == 0 || latency < bestLatency)) {
			best, bestLatency, found = c, latency, true
			if latency == 0 {
				break // 没有延迟信息，保持候选集顺序取第一个
			}
		}
	}
	return best, found
}

// pickBest 从可用节点里挑延迟最低的；全部无延迟记录时取第一个，
// 保证结果稳定可预期。
func pickBest(alive []configure.NodeRef, health map[string]configure.NodeHealth) configure.NodeRef {
	if len(alive) == 0 {
		return configure.NodeRef{}
	}
	best, _ := pickBestExcluding(alive, configure.NodeRef{}, health)
	return best
}
