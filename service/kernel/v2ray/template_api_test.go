package v2ray

import "testing"

func TestSetAPIOnRandomPortIgnoresConfiguredPort(t *testing.T) {
	tmpl := &Template{}
	port, err := tmpl.setAPI(nil, 0, nil)
	if err != nil {
		t.Fatalf("setAPI() error = %v", err)
	}
	if port <= 0 {
		t.Fatalf("setAPI() port = %d, want a dynamically allocated port", port)
	}
	if tmpl.ApiPort != port {
		t.Fatalf("template API port = %d, want %d", tmpl.ApiPort, port)
	}
}
