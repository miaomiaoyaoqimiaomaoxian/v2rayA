package anytls

import (
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func TestExplicitAllowInsecureRoundTrip(t *testing.T) {
	var config ClientConfig
	if err := protojson.Unmarshal([]byte(`{"address":"example.test","port":443,"allow_insecure":true}`), &config); err != nil {
		t.Fatal(err)
	}
	if !config.GetAllowInsecure() {
		t.Fatal("allow_insecure was lost while reading the core config")
	}
	encoded, err := proto.Marshal(&config)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ClientConfig
	if err := proto.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.GetAllowInsecure() {
		t.Fatal("allow_insecure was lost in the core config round trip")
	}
}

func TestClientTLSConfigVerificationPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, pin string
		allow     bool
		wantSkip  bool
		wantPin   bool
	}{
		{name: "strict by default"},
		{name: "explicit insecure", allow: true, wantSkip: true},
		{name: "pinned certificate", pin: "0000000000000000000000000000000000000000000000000000000000000000", wantSkip: true, wantPin: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config, err := clientTLSConfig(&ClientConfig{
				Address:                          "example.test",
				AllowInsecure:                    tc.allow,
				PinnedPeerCertificateChainSha256: tc.pin,
			})
			if err != nil {
				t.Fatal(err)
			}
			if config.InsecureSkipVerify != tc.wantSkip || (config.VerifyPeerCertificate != nil) != tc.wantPin {
				t.Fatalf("skip verification = %t, pin verifier present = %t", config.InsecureSkipVerify, config.VerifyPeerCertificate != nil)
			}
		})
	}
}
