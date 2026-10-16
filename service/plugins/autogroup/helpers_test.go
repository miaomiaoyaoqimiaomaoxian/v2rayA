package autogroup

// 测试辅助：拆 host:port、取客户端的代理端口。

import (
	"net/http"
	"net/url"
	"strconv"
	"testing"
)

func splitHostPort(t *testing.T, rawurl string) (string, int) {
	t.Helper()
	u, err := url.Parse(rawurl)
	if err != nil {
		t.Fatal(err)
	}
	host := u.Hostname()
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return host, port
}

// transportProxy returns the transport and the port its proxy points at.
func transportProxy(t *testing.T, client *http.Client) (*http.Transport, int) {
	t.Helper()
	tr, ok := client.Transport.(*http.Transport)
	if !ok || tr.Proxy == nil {
		return nil, 0
	}
	req, _ := http.NewRequest(http.MethodGet, "http://example.test", nil)
	got, err := tr.Proxy(req)
	if err != nil || got == nil {
		return nil, 0
	}
	port, err := strconv.Atoi(got.Port())
	if err != nil {
		t.Fatal(err)
	}
	return tr, port
}
