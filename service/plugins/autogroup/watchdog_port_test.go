package autogroup

// 验证巡检探测端口的自动选择。
//
// 背景：巡检原先固定用 Ports.Socks5，但 system_proxy 模式下真正承载流量的是
// transparent 入站；普通 socks 入站可能接受连接后直接丢弃，于是每个分组都
// 被判成不可用 —— 那是误报，正是巡检最不该发出的警报。

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBuildProbeClientPicksAReachablePort(t *testing.T) {
	resetWatchdogPort()
	t.Cleanup(resetWatchdogPort)

	// 一个真的会应答的本地服务，代表"可用的本机代理端口"。
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ok.Close()
	host, port := splitHostPort(t, ok.URL)

	setWatchdogPort("http", port)
	client := buildProbeClient()
	if client == nil {
		t.Fatal("已记住可用端口时应给出客户端")
	}
	tr, p := transportProxy(t, client)
	if tr == nil {
		t.Fatal("客户端必须经代理")
	}
	_ = host
	if p != port {
		t.Errorf("应使用已记住的端口 %d，实际 %d", port, p)
	}
}

// TestBuildProbeClientFallsBackWhenNothingAnswers 验证没有端口应答时仍然
// 给出客户端（走普通 socks），让失败以"探测失败"的形式上报，而不是静默跳过
// 一整轮 —— 静默会让用户以为一切正常。
func TestBuildProbeClientFallsBackWhenNothingAnswers(t *testing.T) {
	resetWatchdogPort()
	t.Cleanup(resetWatchdogPort)

	// 没有任何端口应答（连一个 refused 的服务都没有）时，回退到普通 socks。
	client := buildProbeClient()
	if client == nil {
		t.Skip("本机未配置 socks 端口")
	}
	tr, _ := transportProxy(t, client)
	if tr == nil {
		t.Fatal("回退客户端也必须经代理")
	}
}
