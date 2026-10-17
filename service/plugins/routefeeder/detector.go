package routefeeder

// 双路探测器：直连与代理各请求一次，比对结论，决定该域名该走哪条线。
//
// 判定原则：
//
//   "能访问"不等于"可用"。401/403/429 说明线路通、服务拒绝——这证明
//   线路本身可用（到达服务器并完成 TLS），不因状态码不好看而算线路故障。
//   线路不可用的判据是：连接失败、TLS 失败、超时、5xx。
//
//   两条线路的返回体哈希一致才进入"比速度"——不一致说明两条线看到不同
//   内容（CDN 分流、劫持、地域差异化），此时不学习，保守维持现有规则。
//
//   一条可用另一条不可用时选可用的——这比"都可用选快的"更有价值：它把
//   "代理根本到不了"的域名从代理兜底里解救出来。

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/v2rayA/v2rayA/db/configure"
)

// detector 持有探测配置。
type detector struct {
	timeout  time.Duration
	minBody  int
	proxyURL string
	direct   *http.Client
	proxy    *http.Client
}

func newDetector(s Settings) *detector {
	timeout := time.Duration(s.ProbeTimeoutMS) * time.Millisecond
	// 探测容忍证书问题：探测对象是线路，不是站点的证书链。
	tlsCfg := &tls.Config{InsecureSkipVerify: true} //nolint:gosec // 线路探测
	noRedirect := func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	d := &detector{
		timeout:  timeout,
		minBody:  s.MinBodyBytes,
		proxyURL: localProxyURL(),
	}
	d.direct = &http.Client{
		Timeout:       timeout,
		Transport:     &http.Transport{TLSClientConfig: tlsCfg},
		CheckRedirect: noRedirect,
	}
	if d.proxyURL != "" {
		if u, err := url.Parse(d.proxyURL); err == nil {
			d.proxy = &http.Client{
				Timeout:       timeout,
				Transport:     &http.Transport{Proxy: http.ProxyURL(u), TLSClientConfig: tlsCfg},
				CheckRedirect: noRedirect,
			}
		}
	}
	return d
}

// localProxyURL 返回本机 HTTP 代理入口（与用户浏览器同路径）。
func localProxyURL() string {
	ports := configure.GetPortsNotNil()
	if ports.HttpWithPac != 0 {
		return "http://127.0.0.1:" + strconv.Itoa(ports.HttpWithPac)
	}
	if ports.Http != 0 {
		return "http://127.0.0.1:" + strconv.Itoa(ports.Http)
	}
	return ""
}

// probeVerdict 是双路探测后的决定。
type probeVerdict struct {
	learn    bool   // 是否学到一条规则
	chosen   string // "direct" 或出站名（learn=true 时有意义）
	reason   string
	direct   probeResult
	viaProxy probeResult
}

// probeResult 是单线路探测的结论。
type probeResult struct {
	status int
	ms     int64
	body   []byte // 截断到 maxBodyBytes，用于一致性核对
	err    error
}

const maxBodyBytes = 64 << 10

// probeBoth 双路探测一个域名。
//
// target 由调用方构造（生产环境是 https://<host>/；测试传 httptest 服务器的
// URL，因为单测无法劫持任意域名的 DNS）。
func (d *detector) probeBoth(domain, target string) probeVerdict {
	if target == "" {
		target = "https://" + parsedHost(domain) + "/"
	}

	direct := d.probe(d.direct, target)
	via := probeResult{err: fmt.Errorf("本机代理端口不可用")}
	if d.proxy != nil {
		via = d.probe(d.proxy, target)
	}

	v := probeVerdict{direct: direct, viaProxy: via}
	dOK, pOK := lineUsable(direct), lineUsable(via)

	switch {
	case dOK && pOK:
		if !d.comparable(direct, via) {
			v.reason = "两条线路返回内容不一致，保守不学习"
			return v
		}
		if direct.ms <= via.ms {
			v.learn, v.chosen = true, "direct"
			v.reason = fmt.Sprintf("两条线路都可用，直连更快（%dms vs %dms）", direct.ms, via.ms)
		} else {
			v.reason = fmt.Sprintf("两条线路都可用且代理更快（%dms vs %dms），维持现有规则", via.ms, direct.ms)
		}
	case dOK && !pOK:
		v.learn, v.chosen = true, "direct"
		v.reason = fmt.Sprintf("直连可用而代理不可用（代理侧 %s）", failReason(via))
	case pOK && !dOK:
		v.reason = "代理可用而直连不可用，现有规则（走代理）已是最优"
	default:
		v.reason = "两条线路都不可用"
	}
	return v
}

// lineUsable：2xx/3xx/4xx 都证明线路可达；5xx 与传输错误不算。
func lineUsable(r probeResult) bool {
	return r.err == nil && r.status >= 200 && r.status < 500
}

// comparable：状态码类别一致，且返回体（足够大时）哈希一致。
func (d *detector) comparable(a, b probeResult) bool {
	if a.status/100 != b.status/100 {
		return false
	}
	if len(a.body) < d.minBody || len(b.body) < d.minBody {
		// 返回体太小（204、空壳页），哈希无区分度，视为可比。
		return true
	}
	return hashBody(a.body) == hashBody(b.body)
}

func hashBody(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

// probe 走指定客户端请求一次。
func (d *detector) probe(client *http.Client, target string) probeResult {
	ctx, cancel := context.WithTimeout(context.Background(), d.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return probeResult{err: err}
	}
	req.Header.Set("User-Agent", "v2raya-routefeeder/0.1 (+probe)")

	started := time.Now()
	resp, err := client.Do(req)
	ms := time.Since(started).Milliseconds()
	if err != nil {
		return probeResult{err: err}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))

	return probeResult{status: resp.StatusCode, ms: ms, body: body}
}

// parsedHost 提取域名本体（去端口、转小写）。
func parsedHost(domain string) string {
	if u, err := url.Parse("https://" + domain); err == nil && u.Hostname() != "" {
		return strings.ToLower(u.Hostname())
	}
	return strings.ToLower(domain)
}

// failReason 是给日志看的失败摘要。
func failReason(r probeResult) string {
	if r.err != nil {
		e := r.err.Error()
		if len(e) > 60 {
			e = e[:60]
		}
		return e
	}
	return strconv.Itoa(r.status)
}
