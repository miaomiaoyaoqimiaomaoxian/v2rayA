package autogroup

// 健康巡检：周期性验证各分组的出口节点是否还能真正带流量，节点失效时
// 驱动切换，全部候选失效时把原因告诉用户。
//
// 为什么必须有它：AutoPilot.ReportFailure / ReportSuccess 是一对决策接口，
// 但"谁告诉它这个节点刚失败了一次"没有答案——核心只负责转发，不判断一次
// 转发是否成功。少了这个环节，失败计数永远停在 0，自动切换也就永远不会
// 触发。这正是 google 分组的节点挂了却一直没人切换的原因。
//
// 检测方式：用该分组的出口节点，经本机 socks 端口访问一个探测地址。这走
// 完整真实链路（节点 -> 协议握手 -> 出口），因此节点被墙、凭据失效、机场
// 下线都能测出来；只测 TCP 端口通不通测不出来。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/v2rayA/v2rayA/db/configure"
	"github.com/v2rayA/v2rayA/pkg/util/log"
)

const (
	// defaultProbeURL 是一个稳定的 204 地址，不依赖任何需要登录的服务。
	defaultProbeURL = "https://www.gstatic.com/generate_204"

	// defaultInterval 是两轮巡检的间隔。30 秒是"节点坏了多久用户察觉"
	// 与"巡检开销"之间的折中。
	defaultInterval = 30 * time.Second

	// probeTimeout 限制单次探测。一轮要遍历多个分组，不能让单个分组
	// 占满整轮时间。
	watchdogProbeTimeout = 10 * time.Second

	// firstRoundDelay 给核心一点时间起端口，避免第一轮全部失败。
	firstRoundDelay = 3 * time.Second
)

// ErrGroupPinnedToNothing is reported when a group has no single node to
// judge: either it is balancing by itself, or its pinned node is not among
// the candidates any more.
var ErrGroupPinnedToNothing = errors.New("group has no single pinned node to judge")

// Watchdog keeps each group's outbound node honestly accounted for.
type Watchdog struct {
	pilot    *AutoPilot
	interval time.Duration
	probeURL string

	// clientForTest lets a test substitute the probe client. Only tests set
	// it, so the decision path can be exercised without a live proxy.
	clientForTest func() *http.Client

	// refreshForTest lets a test supply the full re-test used to seed the
	// cache. nil means the production one, which restarts the core.
	refreshForTest func() error

	mu          sync.Mutex
	stop        chan struct{}
	stopped     bool
	wg          sync.WaitGroup
	alerting    map[string]bool
	lastRefresh time.Time
}

// cacheRefreshInterval bounds how often the watchdog runs a full re-test.
//
// A full re-test goes through v2rayA's TestHttpLatency, which restarts the
// core twice (it starts a probe template, then rebuilds the real config). That
// is fine as a one-off, but running it every round is what made the core
// restart continuously. Rounds in between only probe the current node, which
// is a plain request through the local proxy port and costs the core nothing.
const cacheRefreshInterval = 10 * time.Minute

// NewWatchdog builds a watchdog for the given controller. A non-positive
// interval or an empty URL falls back to the defaults.
func NewWatchdog(p *AutoPilot, interval time.Duration, probeURL string) *Watchdog {
	if interval <= 0 {
		interval = defaultInterval
	}
	if probeURL == "" {
		probeURL = defaultProbeURL
	}
	return &Watchdog{
		pilot:    p,
		interval: interval,
		probeURL: probeURL,
		stop:     make(chan struct{}),
		alerting: make(map[string]bool),
	}
}

// Start begins probing in the background.
func (w *Watchdog) Start() {
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		return
	}
	w.mu.Unlock()

	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		timer := time.NewTimer(firstRoundDelay)
		defer timer.Stop()
		select {
		case <-w.stop:
			return
		case <-timer.C:
		}
		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()
		for {
			w.Round()
			select {
			case <-w.stop:
				return
			case <-ticker.C:
			}
		}
	}()
}

// Stop halts probing and waits for the goroutine to finish. It is safe to
// call more than once.
func (w *Watchdog) Stop() {
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		return
	}
	w.stopped = true
	w.mu.Unlock()
	close(w.stop)
	w.wg.Wait()
}

// Round runs one pass over every group. Exported so a test can drive it
// without waiting for the ticker.
//
// Two kinds of work, with very different costs:
//
//   - Probing a group's current node goes through the local proxy port. Cheap,
//     touches the core not at all, runs every round.
//   - A full re-test of every candidate goes through v2rayA's TestHttpLatency,
//     which restarts the core twice (it starts a probe template, then rebuilds
//     the real config). That is the only way to learn whether an alternative
//     works — testing node X means routing through node X — so it cannot be
//     avoided, only made rare. It runs when a switch actually needs it, and no
//     more often than cacheRefreshInterval.
func (w *Watchdog) Round() {
	if w.pilot == nil {
		return
	}
	groups := Groups()
	if len(groups) == 0 {
		return
	}
	client := w.client()
	if client == nil {
		return // the core is not listening yet
	}

	needAlternative := false
	for _, g := range groups {
		if w.checkGroup(client, g) {
			needAlternative = true
		}
	}
	if needAlternative {
		w.maybeRefreshCache(groups)
	}
}

// maybeRefreshCache runs the full re-test when its interval has elapsed.
func (w *Watchdog) maybeRefreshCache(groups []Group) {
	w.mu.Lock()
	due := w.lastRefresh.IsZero() || time.Since(w.lastRefresh) >= cacheRefreshInterval
	if due {
		// Claim the slot before releasing the lock so overlapping rounds
		// cannot both start a re-test.
		w.lastRefresh = time.Now()
	}
	w.mu.Unlock()
	if !due {
		log.Debug("plugin/autogroup: 需要备选节点，但距上次全量重测不足 %s，等下一轮", cacheRefreshInterval)
		return
	}
	if err := w.refreshAll(groups); err != nil {
		log.Warn("plugin/autogroup: 全量重测失败: %v", err)
	}
}

// refreshAll re-tests every candidate of every group, seeding the cache that
// the decision paths read.
func (w *Watchdog) refreshAll(groups []Group) error {
	if w.refreshForTest != nil {
		return w.refreshForTest()
	}
	total := 0
	for _, g := range groups {
		n, err := w.pilot.ProbeIntoCache(g.Candidates)
		if err != nil {
			return err
		}
		total += n
	}
	log.Info("plugin/autogroup: 全量重测完成，%d 个分组共 %d 个节点可用", len(groups), total)
	return nil
}

// client returns the probe client. Split out from buildProbeClient so a test
// can substitute one and exercise the decision path without a live proxy.
func (w *Watchdog) client() *http.Client {
	if w.clientForTest != nil {
		return w.clientForTest()
	}
	return buildProbeClient()
}

// buildProbeClient returns a client that reaches the nodes through the local
// proxy port.
//
// Which port: the plain socks port (Ports.Socks5) is the natural choice, but
// it is not guaranteed to carry traffic — in system_proxy mode the transparent
// inbound is what serves requests, and the plain socks inbound can accept a
// connection and then drop it. Probing through a dead port reports every group
// as unreachable, which is exactly the false alarm this watchdog must not
// raise.
//
// So the port is chosen by reachability, in order:
//
//  1. the transparent socks port, which exists precisely to serve a local
//     client and is bound to the loopback interface;
//  2. the plain socks port;
//  3. the plain http port.
//
// The first one that answers a probe wins, and the choice is remembered so
// later rounds do not repeat the search.
func buildProbeClient() *http.Client {
	ports := configure.GetPortsNotNil()
	if scheme, port, ok := watchdogPort(); ok {
		return probeClientFor(scheme, port)
	}
	candidates := []struct {
		port   int
		scheme string
	}{
		{ports.Socks5WithPac, "socks5"}, // transparent socks
		{ports.Socks5, "socks5"},        // plain socks
		{ports.Http, "http"},            // plain http
	}
	probeURL := defaultProbeURL
	for _, c := range candidates {
		if c.port == 0 {
			continue
		}
		client := probeClientFor(c.scheme, c.port)
		if probeReachable(client, probeURL) {
			setWatchdogPort(c.scheme, c.port)
			log.Info("plugin/autogroup: 巡检使用本机代理端口 %s://127.0.0.1:%d", c.scheme, c.port)
			return client
		}
	}
	// Nothing answered: fall back to the plain socks port so the failure is
	// reported as a probe failure rather than silently skipping the round.
	if ports.Socks5 != 0 {
		return probeClientFor("socks5", ports.Socks5)
	}
	return nil
}

// probeReachable reports whether a client can complete one request.
func probeReachable(client *http.Client, probeURL string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), watchdogProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode < 500
}

// watchdogPort remembers which local port the probe settled on, so later rounds
// do not repeat the search. The zero value means "not chosen yet".
var (
	watchdogPortMu     sync.Mutex
	watchdogPortScheme string
	watchdogPortNumber int
)

func watchdogPort() (string, int, bool) {
	watchdogPortMu.Lock()
	defer watchdogPortMu.Unlock()
	if watchdogPortNumber == 0 {
		return "", 0, false
	}
	return watchdogPortScheme, watchdogPortNumber, true
}

func setWatchdogPort(scheme string, port int) {
	watchdogPortMu.Lock()
	watchdogPortScheme, watchdogPortNumber = scheme, port
	watchdogPortMu.Unlock()
}

// resetWatchdogPort forgets the chosen port so the next build searches again.
// A test uses this to keep cases independent.
func resetWatchdogPort() {
	watchdogPortMu.Lock()
	watchdogPortScheme, watchdogPortNumber = "", 0
	watchdogPortMu.Unlock()
}

func probeClientFor(scheme string, port int) *http.Client {
	proxy := url.URL{
		Scheme: scheme,
		Host:   net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
	}
	return &http.Client{
		Timeout:   watchdogProbeTimeout,
		Transport: &http.Transport{Proxy: http.ProxyURL(&proxy)},
		// Only the status matters; following a redirect only adds latency.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// checkGroup probes the group's current node through the local proxy and
// reports the verdict back to the controller.
//
// It returns true when the group could not switch for lack of a fresh verdict
// on some other candidate — the signal that a full re-test is worth its cost.
//
// Only the current node is probed here. Alternatives cannot be tested without
// routing through them, which means changing the core's config and restarting
// it (that is what TestHttpLatency does); that cost is paid only when a switch
// actually needs a verdict, not on every round.
func (w *Watchdog) checkGroup(client *http.Client, g Group) (needAlternative bool) {
	current, err := CurrentNode(g)
	if err != nil {
		// Either the group balances on its own, or its pinned node left the
		// candidate set. Neither is a node failure, so there is nothing to
		// attribute and nothing to switch.
		return false
	}
	return w.reportOutcome(g, *current, client)
}

// reportOutcome probes through the client's proxy and feeds the verdict to the
// controller. Split out from checkGroup so a test can supply the node and the
// group directly, without needing the node to be pinned in the database.
//
// It returns true when the switch could not be made because no other candidate
// has a fresh verdict, which is the caller's cue that a full re-test is due.
func (w *Watchdog) reportOutcome(g Group, current configure.NodeRef, client *http.Client) bool {
	latency, err := w.probe(client)
	if err == nil {
		w.pilot.ReportSuccess(g.Name, current, latency.Milliseconds())
		w.clearAlert(g.Name)
		return false
	}
	switched, perr := w.pilot.ReportFailure(g.Name, g.Candidates, current, err)
	if perr != nil {
		// The controller could not find an alternative: either every cached
		// verdict is stale, or all of them say "down". Either way it needs a
		// fresh full re-test, which is the caller's decision to pay for.
		w.raise(g.Name, perr)
		return true
	}
	if switched {
		log.Info("plugin/autogroup: 分组 %q 的节点 %v 不可用（%v），已切换", g.Name, current.Key(), err)
	}
	return false
}

// probe performs one request through the local proxy.
func (w *Watchdog) probe(client *http.Client) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), watchdogProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.probeURL, nil)
	if err != nil {
		return 0, err
	}
	started := time.Now()
	resp, err := client.Do(req)
	latency := time.Since(started)
	if err != nil {
		return latency, err
	}
	defer resp.Body.Close()
	// Drain so the connection goes back to the pool.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 500 {
		return latency, fmt.Errorf("探测返回 %s", resp.Status)
	}
	return latency, nil
}

// CurrentNode returns the single node the group is pinned to.
//
// It returns an error when the group is not pinned to one node: such a group
// balances by itself, and a failure there belongs to no particular node.
func CurrentNode(g Group) (*configure.NodeRef, error) {
	setting := configure.GetOutboundSetting(g.Name)
	if setting.Selected == "" {
		return nil, fmt.Errorf("%w: 分组 %q 未固定节点", ErrGroupPinnedToNothing, g.Name)
	}
	for i := range g.Candidates {
		c := g.Candidates[i]
		raw, err := c.LocateServerRaw()
		if err != nil {
			continue
		}
		if raw.ServerObj.ExportToURL() == setting.Selected {
			ref := c
			return &ref, nil
		}
	}
	return nil, fmt.Errorf("%w: 分组 %q 固定的节点不在候选集内", ErrGroupPinnedToNothing, g.Name)
}

// raise records that a group has no usable node. Reported once per outage,
// so a long outage does not repeat the same line every round.
func (w *Watchdog) raise(group string, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.alerting[group] {
		return
	}
	w.alerting[group] = true
	log.Error("plugin/autogroup: 分组 %q 没有可用节点：%v", group, err)
}

// clearAlert lets the next outage be reported again.
func (w *Watchdog) clearAlert(group string) {
	w.mu.Lock()
	w.alerting[group] = false
	w.mu.Unlock()
}

// Alerts returns the groups currently reported as having no usable node.
// The plugins page shows these so a silent outage is visible.
func (w *Watchdog) Alerts() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]string, 0, len(w.alerting))
	for name, on := range w.alerting {
		if on {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
