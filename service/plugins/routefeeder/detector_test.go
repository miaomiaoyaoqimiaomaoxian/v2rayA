package routefeeder

// 双路探测与学习决策的回归测试。
//
// 这些用例钉住的是判据本身，而不是实现细节：
//   - 401/403 是"线路可用、服务拒绝"，不是线路故障 —— 这是用户明确的
//     诉求，也是这个插件与其他"测活"实现的分界线。
//   - 内容不一致时不学习（CDN 分流/劫持下贸然择优会选错）。
//   - 代理更快时不写规则（现有默认已最优，写 direct 反而是退化）。

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func testDetector(t *testing.T, directHandler, proxyHandler http.HandlerFunc) (*detector, string) {
	t.Helper()
	dsrv := httptest.NewServer(directHandler)
	t.Cleanup(dsrv.Close)
	psrv := httptest.NewServer(proxyHandler)
	t.Cleanup(psrv.Close)

	// 双路模型：两条"线路"各自到达自己的服务器——
	//   直连线路：direct 客户端连 target（dsrv）；
	//   代理线路：proxy 客户端的传输层把请求改写到 psrv（模拟"经代理后
	//   内容与速度由代理侧决定"）。
	// 生产环境里这个改写由代理服务器完成；单测里用 RoundTripper 复刻。
	proxyRT := rewriteRoundTripper{to: psrv.URL, base: http.DefaultTransport}
	proxy := &http.Client{Timeout: 2 * time.Second, Transport: proxyRT}

	return &detector{
		timeout:  2 * time.Second,
		minBody:  8,
		proxyURL: psrv.URL,
		direct:   dsrv.Client(),
		proxy:    proxy,
	}, dsrv.URL + "/"
}

// rewriteRoundTripper 把请求改写到另一个 base URL，模拟代理线路。
type rewriteRoundTripper struct {
	to   string
	base http.RoundTripper
}

func (rt rewriteRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	u, err := url.Parse(rt.to)
	if err != nil {
		return nil, err
	}
	req.URL.Scheme = u.Scheme
	req.URL.Host = u.Host
	req.Host = u.Host
	return rt.base.RoundTrip(req)
}

// probeURLs 返回探测目标（直连服务器 URL；两个 handler 收到的都是同一 URL，
// 内容差异由 handler 决定，这正是双路探测的模型）。
func probeTarget(dsrv *httptest.Server) string { return dsrv.URL + "/" }

func withStatus(w http.ResponseWriter, code int, body string) {
	w.WriteHeader(code)
	_, _ = w.Write([]byte(body))
}

// TestLineUsableTreats4xxAsReachable 是判据的分界线：
// 401/403 证明线路通（到达服务器并完成 TLS），不能算线路故障。
func TestLineUsableTreats4xxAsReachable(t *testing.T) {
	for _, code := range []int{200, 301, 401, 403, 429} {
		r := probeResult{status: code}
		if !lineUsable(r) {
			t.Errorf("status %d 应判定为线路可用", code)
		}
	}
	for _, code := range []int{500, 502, 503} {
		r := probeResult{status: code}
		if lineUsable(r) {
			t.Errorf("status %d 应判定为线路不可用", code)
		}
	}
	if lineUsable(probeResult{err: fmt.Errorf("timeout")}) {
		t.Error("传输错误应判定为线路不可用")
	}
}

// TestProbeBothChoosesDirectWhenProxyDead：
// 直连 200、代理侧 5xx（线路不可用的服务端形态）→ 学到 direct。
// 这正是 yc6.ai 的场景（国内站被代理兜底兜坏）。
func TestProbeBothChoosesDirectWhenProxyDead(t *testing.T) {
	d, target := testDetector(t,
		func(w http.ResponseWriter, _ *http.Request) { withStatus(w, 200, strings.Repeat("A", 100)) },
		func(w http.ResponseWriter, _ *http.Request) { withStatus(w, 503, "proxy line down") },
	)
	// 再把代理客户端换成必然连接失败的死端口，覆盖"传输错误"这一类。
	d.proxy = &http.Client{
		Timeout:   300 * time.Millisecond,
		Transport: rewriteRoundTripper{to: "http://127.0.0.1:1", base: http.DefaultTransport},
	}

	v := d.probeBoth("example.com", target)
	if !v.learn {
		t.Fatalf("直连可用而代理不可用时必须学到 direct，实际 reason=%s", v.reason)
	}
	if v.chosen != "direct" {
		t.Errorf("chosen = %s, want direct", v.chosen)
	}
}

// TestProbeBothSkipsWhenProxyFaster：
// 两条都可用且代理更快 → 不学习（现有默认走代理已最优，写 direct 是退化）。
func TestProbeBothSkipsWhenProxyFaster(t *testing.T) {
	d, target := testDetector(t,
		func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(200 * time.Millisecond) // 直连慢
			withStatus(w, 200, strings.Repeat("A", 100))
		},
		func(w http.ResponseWriter, _ *http.Request) { withStatus(w, 200, strings.Repeat("A", 100)) },
	)
	v := d.probeBoth("example.com", target)
	if v.learn {
		t.Fatalf("代理更快时不应学习（写 direct 是退化），实际 chosen=%s reason=%s", v.chosen, v.reason)
	}
}

// TestProbeBothRequiresContentMatch：
// 两条线路返回不同内容（劫持/CDN 分流）→ 保守不学习。
func TestProbeBothRequiresContentMatch(t *testing.T) {
	d, target := testDetector(t,
		func(w http.ResponseWriter, _ *http.Request) { withStatus(w, 200, strings.Repeat("A", 100)) },
		func(w http.ResponseWriter, _ *http.Request) { withStatus(w, 200, strings.Repeat("B", 100)) },
	)
	v := d.probeBoth("example.com", target)
	if v.learn {
		t.Fatalf("内容不一致时不应学习，实际 chosen=%s reason=%s", v.chosen, v.reason)
	}
}

// TestProbeBothHandlesAuthEndpoints：
// 两条线路都返回 401（如 API 端点）：线路都可用、内容可比 → 比速度。
// 这条测试钉住"401 不是线路故障"的判据。
func TestProbeBothHandlesAuthEndpoints(t *testing.T) {
	d, target := testDetector(t,
		func(w http.ResponseWriter, _ *http.Request) { withStatus(w, 401, `{"error":"unauthorized"}`) },
		func(w http.ResponseWriter, _ *http.Request) { withStatus(w, 401, `{"error":"unauthorized"}`) },
	)
	v := d.probeBoth("api.example.com", target)
	// 两条都可用（401 可用），谁快学谁；无论学不学，reason 必须承认两条都通。
	if !strings.Contains(v.reason, "两条线路都可用") {
		t.Errorf("401 双侧应判为两条线路都可用，实际 reason=%s", v.reason)
	}
}

// TestProbeBothBothDead：两条都不可用 → 不学习。
func TestProbeBothBothDead(t *testing.T) {
	d, target := testDetector(t,
		func(w http.ResponseWriter, _ *http.Request) { withStatus(w, 503, "x") },
		func(w http.ResponseWriter, _ *http.Request) { withStatus(w, 503, "x") },
	)
	v := d.probeBoth("example.com", target)
	if v.learn {
		t.Fatalf("两条都不可用时不应学习，实际 chosen=%s", v.chosen)
	}
}

// TestWorthLearningFiltersCDN：CDN/静态资源域名不学习。
func TestWorthLearningFiltersCDN(t *testing.T) {
	yes := []string{"yc6.ai", "example.com", "api.deepseek.com"}
	no := []string{"a.jsdelivr.net", "x.githubusercontent.com", "b.alicdn.com", "localhost", ""}
	for _, d := range yes {
		if !worthLearning(d) {
			t.Errorf("%s 应值得学习", d)
		}
	}
	for _, d := range no {
		if worthLearning(d) {
			t.Errorf("%s 不应值得学习", d)
		}
	}
}

// TestParsedHostStripsPort：host 提取去端口、转小写。
func TestParsedHostStripsPort(t *testing.T) {
	if got := parsedHost("Example.COM:8443"); got != "example.com" {
		t.Errorf("parsedHost = %q", got)
	}
}
