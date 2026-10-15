package serverObj

import (
	"encoding/json"
	"testing"
)

func TestAnyTLSExplicitInsecureSetting(t *testing.T) {
	cases := []struct {
		name, query string
		want        bool
	}{
		{"provider insecure", "insecure=1", true},
		{"legacy allow insecure", "allow_insecure=true", true},
		{"explicit false", "insecure=0", false},
		{"default", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			link := "anytls://test-password@example.test:443"
			if tc.query != "" {
				link += "?" + tc.query
			}
			server, err := ParseAnyTLSURL(link)
			if err != nil {
				t.Fatal(err)
			}
			configuration, err := server.Configuration(PriorInfo{Tag: "test"})
			if err != nil {
				t.Fatal(err)
			}
			var settings struct {
				AllowInsecure bool `json:"allow_insecure"`
			}
			if err := json.Unmarshal(configuration.CoreOutbound.Settings.Inlined, &settings); err != nil {
				t.Fatal(err)
			}
			if settings.AllowInsecure != tc.want {
				t.Errorf("allow_insecure = %t, want %t", settings.AllowInsecure, tc.want)
			}
		})
	}
}
