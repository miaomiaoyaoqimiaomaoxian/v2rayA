package autogroup

import (
	"fmt"
	"sync"
	"time"

	"github.com/v2rayA/v2rayA/common/httpClient"
	"github.com/v2rayA/v2rayA/db/configure"
	"github.com/v2rayA/v2rayA/kernel/v2ray"
	"github.com/v2rayA/v2rayA/pkg/util/log"
)

// 本文件是插件与核心的耦合层：探测节点、应用选择、重载核心。
// 换内核（例如 mihomo）时只需改这三个函数，控制器逻辑不用动。

// probeTimeout 是单个节点的探测超时。
const probeTimeout = 8 * time.Second

// probeConcurrency 是同时探测的节点数上限。
const probeConcurrency = 8

// ProbeNodes 对一组节点做可用性探测。
//
// 实现方式与 v2rayA 自带的 httpLatency 一致：为每个节点临时插入一个
// 独立 inbound + outbound，经本地 socks 端口请求一个 204 地址，从而
// 真正走一遍该节点的完整链路，而不是只做 TCP 握手。
func ProbeNodes(refs []configure.NodeRef) (map[string]ProbeResult, error) {
	if len(refs) == 0 {
		return map[string]ProbeResult{}, nil
	}
	out := make(map[string]ProbeResult, len(refs))

	// 先把节点解析成 ServerObj；解析失败的节点直接记为不可用，
	// 不需要为它起探测。
	loc := configure.NewLocator()
	objs := make([]map[int]configure.NodeRef, len(refs))
	ok := make([]bool, len(refs))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, probeConcurrency)
	for i, ref := range refs {
		wg.Add(1)
		go func(i int, ref configure.NodeRef) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			sr, err := loc.Locate(&ref)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				out[ref.Key()] = ProbeResult{Alive: false, Err: err}
				return
			}
			objs[i] = map[int]configure.NodeRef{i: ref}
			ok[i] = true
			_ = sr
		}(i, ref)
	}
	wg.Wait()

	// 收集可探测的节点。
	var probeRefs []configure.NodeRef
	for i := range refs {
		if ok[i] {
			probeRefs = append(probeRefs, refs[i])
		}
	}
	if len(probeRefs) == 0 {
		return out, nil
	}

	results, err := httpLatencyViaCore(probeRefs)
	if err != nil {
		// 整批探测失败（例如核心起不来）：把每个节点都记为失败，
		// 让控制器走"没有可用节点"的分支，而不是把错误吞掉。
		for _, ref := range probeRefs {
			out[ref.Key()] = ProbeResult{Alive: false, Err: err}
		}
		return out, nil
	}
	for k, v := range results {
		out[k] = v
	}
	return out, nil
}

// httpLatencyViaCore 复用 v2rayA 的延迟测试实现：它会把每个节点插入一个
// 临时 inbound，经 socks 请求测试地址，返回延迟字符串。
//
// 这里把它包装成"可用性 + 毫秒数"，与 AutoPilot 的 ProbeResult 对齐。
func httpLatencyViaCore(refs []configure.NodeRef) (map[string]ProbeResult, error) {
	started := time.Now()
	whiches := make([]*configure.Which, 0, len(refs))
	for i := range refs {
		w := configure.Which{NodeRef: refs[i]}
		whiches = append(whiches, &w)
	}
	// latencyTestURL 用默认的 gstatic 204；它足够快且不带地区语义。
	tested, err := TestHTTPLatency(whiches, probeTimeout, probeConcurrency, false, "")
	if err != nil {
		return nil, err
	}
	out := make(map[string]ProbeResult, len(tested))
	alive := 0
	var sumMs, maxMs int64
	for _, w := range tested {
		ok, ms := parseLatency(w.Latency)
		if ok {
			alive++
			sumMs += ms
			if ms > maxMs {
				maxMs = ms
			}
		}
		out[w.Key()] = ProbeResult{Alive: ok, LatencyMs: ms}
	}
	// 探测汇总：一次探测只写一条，便于按时间轴看"当时有多少节点活着"。
	avgMs := int64(0)
	if alive > 0 {
		avgMs = sumMs / int64(alive)
	}
	if probeLogger != nil {
		ev := Event{
			Decision:  "probe",
			Level:     LevelInfo,
			Msg:       fmt.Sprintf("探测 %d 个候选：%d 可用", len(refs), alive),
			Reason:    fmt.Sprintf("平均 %dms, 最慢 %dms", avgMs, maxMs),
			DurationMs: time.Since(started).Milliseconds(),
		}
		if alive == 0 {
			ev.Level = LevelWarn
			ev.Msg = fmt.Sprintf("探测 %d 个候选：全部不可用", len(refs))
		}
		probeLogger.Log(ev)
	}
	return out, nil
}

// probeLogger 是探测层的决策日志记录器，由插件启用时注入。
var probeLogger *Logger

// SetProbeLogger 注入探测层的日志记录器。
func SetProbeLogger(l *Logger) { probeLogger = l }

// parseLatency 把 v2rayA 的延迟字符串解析成"是否可用 + 毫秒数"。
// 可用形态："512ms"；不可用形态："TIMEOUT" / "BAD RESPONSE" /
// "INVALID" / "NOT STABLE" / "PROXY ERROR" / "SYSTEM ERROR" / 其它错误文本。
func parseLatency(s string) (alive bool, ms int64) {
	if s == "" {
		return false, 0
	}
	var v int64
	if _, err := fmt.Sscanf(s, "%dms", &v); err == nil && v > 0 {
		return true, v
	}
	return false, 0
}

// ApplySelection 把某个节点设为分组的唯一出口，并让核心生效。
//
// 走的是与 web 界面「选择单个节点」完全相同的路径
// （OutboundSetting.Selected + UpdateV2RayConfig），因此：
//   - 失败时由 UpdateV2RayConfig 负责回滚；
//   - 用户在界面上看到的"当前节点"与插件选中的始终一致。
func ApplySelection(outbound string, ref configure.NodeRef) error {
	sr, err := ref.LocateServerRaw()
	if err != nil {
		return err
	}
	setting := configure.GetOutboundSetting(outbound)
	setting.Selected = sr.ServerObj.ExportToURL()
	if err := configure.SetOutboundSetting(outbound, setting); err != nil {
		return err
	}
	if !v2ray.ProcessManager.Running() {
		// 核心没运行：只落库，等它启动时 EnsureGroup 会再校正一次。
		return nil
	}
	return v2ray.UpdateV2RayConfig()
}

// TestHTTPLatency 与 httpLatencyClient 是对 service.TestHttpLatency 的
// 间接引用，拆开写是为了让本包不直接依赖 server/service（避免循环导入）。
var (
	testHTTPLatencyFunc   func([]*configure.Which, time.Duration, int, bool, string) ([]*configure.Which, error)
	httpLatencyClientOnce sync.Once
)

// SetProbeHooks 由装配代码（main 包）注入真实的延迟测试实现。
// 用注入而不是直接 import，是为了打破 plugin -> server/service -> plugin
// 的循环依赖。
func SetProbeHooks(fn func([]*configure.Which, time.Duration, int, bool, string) ([]*configure.Which, error)) {
	testHTTPLatencyFunc = fn
}

// TestHTTPLatency 调用注入的延迟测试实现。
func TestHTTPLatency(w []*configure.Which, timeout time.Duration, parallel int, showLog bool, url string) ([]*configure.Which, error) {
	if testHTTPLatencyFunc == nil {
		return nil, fmt.Errorf("autogroup: probe hooks are not installed")
	}
	return testHTTPLatencyFunc(w, timeout, parallel, showLog, url)
}

// httpLatencyClient 保留给未来扩展（例如直接用 socks 客户端探测），
// 当前未使用。
var _ = httpClient.GetHttpClientWithProxy

// logWarn 是插件内的日志辅助。
func logWarn(format string, v ...interface{}) { log.Warn("plugin/autogroup: "+format, v...) }
