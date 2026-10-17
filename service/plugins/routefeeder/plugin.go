// Package routefeeder 是「路由学习器」插件。
//
// 功能：首次访问某站点时，用直连和代理两条线路各测一次，比对返回内容，
// 择优（可用且快）写入 RoutingA 规则，让下一次访问直接走对的线路。
//
// 为什么做成插件：它要观察流量域名、要发双路探测请求、要改 RoutingA 并
// 触发重载——三件事都跨在"插件能碰"与"核心才有"的边界上。但它和
// autogroup 一样，本质是核心之上的策略层，核心不该知道它存在。
//
// 判定"可用"（用户明确的诉求）：
//
//	不能只看 HTTP 状态码——401/403 说明线路通但服务拒绝，与"线路不通"
//	是两回事。线路可用 = 到达服务器并完成 TLS 且得到非 5xx 响应。
//
// 判定"择优"：
//
//	两条都可用先核对返回内容一致性（哈希），一致才比速度；只有一条可用
//	选那条；两条都不可用不写规则。代理更快时不写规则（现有默认已最优）。
//
// 安全边界：
//
//	只为新域名学习，绝不动用户手写的规则行；学到的规则集中在带标记的
//	块里，整块可删；每天有学习上限防探测风暴；每次改动写审计日志。
package routefeeder

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/v2rayA/v2rayA/db/configure"
	"github.com/v2rayA/v2rayA/kernel/v2ray"
	"github.com/v2rayA/v2rayA/pkg/util/log"
	"github.com/v2rayA/v2rayA/plugin"
)

// PluginName 是插件注册名。
const PluginName = "routefeeder"

const (
	pluginVersion     = "0.1.0"
	pluginDescription = "路由学习器：首次访问自动双路探测（直连/代理），择优写入 RoutingA"
)

// Settings 是用户可调参数。
type Settings struct {
	// Enabled 总开关。false 时插件运行但不学习（事件照收，探测不做）。
	Enabled bool `json:"enabled"`
	// ProbeTimeoutMS 单线路探测超时。
	ProbeTimeoutMS int `json:"probeTimeoutMs"`
	// MinBodyBytes 返回体小于此字节数不做哈希比对（204、空壳页无区分度）。
	MinBodyBytes int `json:"minBodyBytes"`
	// MaxLearnPerDay 每天最多学习多少个新域名（防探测风暴）。
	MaxLearnPerDay int `json:"maxLearnPerDay"`
	// ReloadCooldownSec 两次写规则并重载核心的最小间隔（秒）。
	ReloadCooldownSec int `json:"reloadCooldownSec"`
}

func defaults() Settings {
	return Settings{
		Enabled:           true,
		ProbeTimeoutMS:    5000,
		MinBodyBytes:      64,
		MaxLearnPerDay:    20,
	}
}

// Load 读设置，缺省兜底。
func Load() Settings {
	s := defaults()
	if plugin.GetSetting(PluginName, "settings", &s) {
		if s.ProbeTimeoutMS <= 0 {
			s.ProbeTimeoutMS = defaults().ProbeTimeoutMS
		}
		if s.MinBodyBytes < 0 {
			s.MinBodyBytes = defaults().MinBodyBytes
		}
		if s.MaxLearnPerDay <= 0 {
			s.MaxLearnPerDay = defaults().MaxLearnPerDay
		}
		if s.ReloadCooldownSec <= 0 {
			s.ReloadCooldownSec = defaults().ReloadCooldownSec
		}
	}
	return s
}

// Save 写设置。
func Save(s Settings) error {
	if s.ProbeTimeoutMS <= 0 {
		s.ProbeTimeoutMS = defaults().ProbeTimeoutMS
	}
	if s.MaxLearnPerDay <= 0 {
		s.MaxLearnPerDay = defaults().MaxLearnPerDay
	}
	return plugin.SetSetting(PluginName, "settings", s)
}

// Plugin 实现 plugin.Plugin。
type Plugin struct {
	mu       sync.Mutex
	learned  map[string]bool // 已学习域名（跨启动持久化在 settings）
	observed map[string]bool // 本轮观察过（避免重复入队）
	unsubs   []func()
	logger   *fwLogger
	detector *detector
	settings Settings

	// 探测配额（每天最多探多少个新域名）
	dayKey   string
	dayCount int
}

var _ plugin.Plugin = (*Plugin)(nil)

// New 构造插件。
func New() *Plugin { return &Plugin{} }

func (*Plugin) Name() string        { return PluginName }
func (*Plugin) Description() string { return pluginDescription }
func (*Plugin) Version() string     { return pluginVersion }
func (*Plugin) Builtin() bool       { return true }

// OnStart 加载已学清单、构造探测器、订阅事件。
func (p *Plugin) OnStart(ctx *plugin.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.logger != nil {
		return nil // 已启动
	}
	p.settings = Load()
	p.logger = newFWLogger(PluginName)
	p.learned = loadLearnedSet()
	p.observed = make(map[string]bool)
	p.detector = newDetector(p.settings)
	p.dayKey = time.Now().Format("2006-01-02")
	p.dayCount = 0

	p.unsubs = []func(){
		ctx.Events.Subscribe(plugin.EventServersChanged, func(interface{}) {
			p.onTrafficHint()
		}),
	}
	p.logger.info("已启用：双路学习开启，已学 %d 个域名，每日上限 %d",
		len(p.learned), p.settings.MaxLearnPerDay)
	return nil
}

// OnStop 停止观察。已学规则保留（那是数据，不是运行态）。
func (p *Plugin) OnStop(ctx *plugin.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, u := range p.unsubs {
		u()
	}
	p.unsubs = nil
	p.observed = nil
	p.detector = nil
	if p.logger != nil {
		p.logger.info("已停用（已学 %d 条规则保留）", len(p.learned))
		p.logger.close()
		p.logger = nil
	}
	return nil
}

// onTrafficHint 事件回调：从核心日志观察新域名，入学习队列。
// 真正的探测在独立 goroutine（不阻塞总线——总线是同步的）。
func (p *Plugin) onTrafficHint() {
	p.mu.Lock()
	if p.detector == nil || !p.settings.Enabled {
		p.mu.Unlock()
		return
	}
	domains := p.harvestLocked()
	p.mu.Unlock()
	if len(domains) == 0 {
		return
	}
	go p.learnDomains(domains)
}

// harvestLocked 挑出未学过、值得学的新域名。调用方持锁。
func (p *Plugin) harvestLocked() []string {
	recent := recentProxyDomains(observationWindow)
	out := make([]string, 0, len(recent))
	for _, d := range recent {
		d = strings.ToLower(parsedHost(d))
		if d == "" || p.learned[d] || p.observed[d] {
			continue
		}
		if !worthLearning(d) {
			continue
		}
		p.observed[d] = true
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// learnDomains 对一批域名做双路探测，结论只收集、不写规则、不重载。
//
// 为什么不直接写规则：写入要靠重载核心才生效，而重载就是断网几秒。
// 观察是持续后台行为，重载不能是——所以收集与应用严格分开：
//   收集：每轮自动做，结论落库（observations），日志可回放；
//   应用：用户显式触发 ApplyLearned，一次性写入+重载。
// 这样断网时机完全由用户掌控。
func (p *Plugin) learnDomains(domains []string) {
	p.mu.Lock()
	s := p.settings
	day := time.Now().Format("2006-01-02")
	if p.dayKey != day {
		p.dayKey, p.dayCount = day, 0
	}
	if p.dayCount >= s.MaxLearnPerDay {
		p.mu.Unlock()
		p.logger.debug("今日探测额度已用完（%d/%d），跳过 %d 个域名",
			p.dayCount, s.MaxLearnPerDay, len(domains))
		return
	}
	if remain := s.MaxLearnPerDay - p.dayCount; len(domains) > remain {
		domains = domains[:remain]
	}
	p.mu.Unlock()

	var fresh []observation
	for _, d := range domains {
		// 生产环境：目标留空，探测器自己按域名构造 https URL。
		v := p.detector.probeBoth(d, "")
		obs := observation{
			Domain:     d,
			At:         time.Now().Format(time.RFC3339),
			DirectOK:   lineUsable(v.direct),
			ProxyOK:    lineUsable(v.viaProxy),
			DirectMS:   v.direct.ms,
			ProxyMS:    v.viaProxy.ms,
			DirectCode: v.direct.status,
			ProxyCode:  v.viaProxy.status,
			Verdict:    v.reason,
		}
		if v.learn {
			obs.Suggested = v.chosen
		}
		p.mu.Lock()
		p.dayCount++
		p.mu.Unlock()
		fresh = append(fresh, obs)
		if v.learn {
			p.logger.info("域名 %s 建议走 %s（%s）", d, v.chosen, v.reason)
		} else {
			p.logger.debug("域名 %s 不建议调整：%s", d, v.reason)
		}
	}
	if len(fresh) > 0 {
		if err := appendObservations(fresh); err != nil {
			p.logger.error("落库 %d 条观察失败: %v", len(fresh), err)
		}
	}
}

// observation 是一次双路探测的完整记录。
type observation struct {
	Domain     string `json:"domain"`
	At         string `json:"at"`
	DirectOK   bool   `json:"directOk"`
	ProxyOK    bool   `json:"proxyOk"`
	DirectMS   int64  `json:"directMs"`
	ProxyMS    int64  `json:"proxyMs"`
	DirectCode int    `json:"directCode,omitempty"`
	ProxyCode  int    `json:"proxyCode,omitempty"`
	// Suggested 是插件的建议线路："direct" 或空（不调整）。
	Suggested string `json:"suggested,omitempty"`
	// Verdict 是人读的判定理由。
	Verdict string `json:"verdict"`
}

const observationsKey = "observations"

// appendObservations 把观察追加到插件私有存储（保留最近 maxObs 条）。
func appendObservations(fresh []observation) error {
	var all []observation
	_ = plugin.GetSetting(PluginName, observationsKey, &all)
	all = append(all, fresh...)
	if len(all) > maxObservations {
		all = all[len(all)-maxObservations:]
	}
	return plugin.SetSetting(PluginName, observationsKey, all)
}

// maxObservations 是观察记录的保留上限（防无限增长）。
const maxObservations = 2000

// GetObservations 返回收集到的观察（API 层用，最新的在前）。
func GetObservations() []observation {
	var all []observation
	if plugin.GetSetting(PluginName, observationsKey, &all) {
		// 逆序：最新在前。
		for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
			all[i], all[j] = all[j], all[i]
		}
		return all
	}
	return nil
}

// ApplyLearned 把收集到的建议写入 RoutingA 并重载核心。
//
// 这是唯一的"写规则+重载"入口，只能由用户显式触发（API 或界面按钮）。
// 断网发生在这一刻、且只发生一次——时机由用户掌控。
// 返回写入的规则数。
func (p *Plugin) ApplyLearned() (int, error) {
	obs := GetObservations()
	p.mu.Lock()
	defer p.mu.Unlock()
	written := 0
	seen := make(map[string]bool)
	for _, o := range obs {
		if o.Suggested == "" || seen[o.Domain] {
			continue
		}
		seen[o.Domain] = true
		if err := p.appendRuleLocked(o.Domain, o.Suggested); err != nil {
			p.logger.error("写规则 %s -> %s 失败: %v", o.Domain, o.Suggested, err)
			continue
		}
		written++
	}
	if written == 0 {
		return 0, nil
	}
	p.learned = loadLearnedSet()
	for d := range seen {
		p.learned[d] = true
	}
	persistLearned(p.learned)

	if v2ray.ProcessManager.Running() {
		if err := v2ray.UpdateV2RayConfig(); err != nil {
			return written, fmt.Errorf("重载核心失败（规则已写入，重试或重启后生效）: %w", err)
		}
	}
	p.logger.info("用户应用了 %d 条学习规则，核心已重载", written)
	return written, nil
}

// appendRuleLocked 把 domain(...) -> outbound 追加到学习块。调用方持锁。
// 幂等：块内已有该域名则替换位置不动。
func (p *Plugin) appendRuleLocked(domain, outbound string) error {
	rules := configure.GetRoutingA()
	newLine := fmt.Sprintf("domain(domain:%s)->%s", domain, outbound)

	var out []string
	inBlock := false
	inserted := false
	for _, ln := range strings.Split(rules, "\n") {
		t := strings.TrimSpace(ln)
		switch {
		case t == learningBlockBegin:
			inBlock = true
			out = append(out, ln, newLine)
			inserted = true
		case t == learningBlockEnd:
			inBlock = false
			out = append(out, ln)
		case inBlock && strings.Contains(t, "domain:"+domain+")"):
			// 旧行跳过（新行已在块首），保持幂等。
		default:
			out = append(out, ln)
		}
	}
	if !inserted {
		out = append(out, learningBlockBegin, newLine, learningBlockEnd)
	}
	updated := strings.Join(out, "\n")
	return configure.SetRoutingA(&updated)
}

// 学习块的标记行：界面上肉眼可辨，整块可删。
const (
	learningBlockBegin = "# [routefeeder] 本块由路由学习器插件维护，可整块删除"
	learningBlockEnd   = "# [/routefeeder]"
)

// settingsKeyLearned 存已学域名集合的键。
const settingsKeyLearned = "learned"

// loadLearnedSet 从插件私有存储恢复已学域名集合。
func loadLearnedSet() map[string]bool {
	var list []string
	if plugin.GetSetting(PluginName, settingsKeyLearned, &list) {
		m := make(map[string]bool, len(list))
		for _, d := range list {
			m[d] = true
		}
		return m
	}
	return make(map[string]bool)
}

// persistLearned 把已学域名集合写回插件私有存储。
func persistLearned(m map[string]bool) {
	list := make([]string, 0, len(m))
	for d := range m {
		list = append(list, d)
	}
	sort.Strings(list)
	_ = plugin.SetSetting(PluginName, settingsKeyLearned, list)
}

// observationWindow 是每次事件回调从核心日志回看的行数。
// 覆盖一次典型浏览会话产生的日志量，又不至于每轮扫描太重。
const observationWindow = 300

// worthLearning 过滤不值得学的域名：CDN/静态资源域名海量化，学不过来且收益低。
func worthLearning(domain string) bool {
	if domain == "" || !strings.Contains(domain, ".") {
		return false
	}
	lower := strings.ToLower(domain)
	for _, suffix := range []string{
		".gstatic.com", ".googleapis.com", ".cloudfront.net",
		".akamaized.net", ".alicdn.com", ".githubusercontent.com",
		".githubassets.com", ".jsdelivr.net", ".unpkg.com",
		".sentry.io", ".doubleclick.net",
	} {
		if strings.HasSuffix(lower, suffix) {
			return false
		}
	}
	return true
}

// recentProxyDomains 从核心日志提取最近走代理的域名。
// 包级变量便于测试注入假日志。
var recentProxyDomains = recentProxyDomainsFromLog

var _ = log.Info
