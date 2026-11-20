package conf_test

import (
	"encoding/json"
	"encoding/pem"
	"testing"

	"github.com/xtls/xray-core/common/protocol/tls/cert"
	. "github.com/xtls/xray-core/infra/conf"
)

func TestNaiveClientConfig(t *testing.T) {
	creator := func() Buildable { return new(NaiveClientConfig) }
	for _, tc := range []struct {
		name  string
		input string
	}{
		{"missing address", `{"port":443}`},
		{"null address", `{"address":null,"port":443}`},
		{"empty address", `{"address":"","port":443}`},
		{"missing port", `{"address":"example.com"}`},
		{"zero port", `{"address":"example.com","port":0}`},
		{"overflow port", `{"address":"example.com","port":65536}`},
		{"negative port", `{"address":"example.com","port":-1}`},
		{"colon in username", `{"address":"example.com","port":443,"user":"alice:admin"}`},
		{"unknown QUIC congestion control", `{"address":"example.com","port":443,"quic":true,"quicCongestionControl":"unknown"}`},
		{"QUIC congestion control without QUIC", `{"address":"example.com","port":443,"quicCongestionControl":"bbr"}`},
		{"invalid header token", `{"address":"example.com","port":443,"headers":{"Bad Name":"value"}}`},
		{"header name injection", `{"address":"example.com","port":443,"headers":{"X-Test\r\nInjected":"value"}}`},
		{"header value injection", `{"address":"example.com","port":443,"headers":{"X-Test":"value\r\nInjected: yes"}}`},
		{"header NUL", `{"address":"example.com","port":443,"headers":{"X-Test":"value\u0000"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := loadJSON(creator)(tc.input); err == nil {
				t.Fatal("expected invalid Naive settings to fail")
			}
		})
	}
}

func TestNaiveOutboundSendThrough(t *testing.T) {
	for _, tc := range []struct {
		via   string
		valid bool
	}{
		{"192.0.2.1", true},
		{"192.0.2.0/24", true},
		{"origin", false},
		{"srcip", false},
	} {
		t.Run(tc.via, func(t *testing.T) {
			input := `{"protocol":"naive","sendThrough":"` + tc.via + `","settings":{"address":"example.com","port":443},"streamSettings":{"security":"tls"}}`
			config := new(OutboundDetourConfig)
			if err := json.Unmarshal([]byte(input), config); err != nil {
				t.Fatal(err)
			}
			_, err := config.Build()
			if tc.valid && err != nil {
				t.Fatal(err)
			}
			if !tc.valid && err == nil {
				t.Fatal("expected dynamic sendThrough to be rejected")
			}
		})
	}
}

func TestNaiveOutboundConfig(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stream string
		mux    string
		valid  bool
	}{
		{"default network", `{"security":"tls"}`, "", true},
		{"raw", `{"network":"raw","security":"tls","tlsSettings":{"serverName":"proxy.example"}}`, "", true},
		{"tcp with QUIC", `{"network":"tcp","security":"tls"}`, "", true},
		{"disabled mux", `{"security":"tls"}`, `,"mux":{"enabled":false,"concurrency":-1}`, true},
		{"socket defaults with domain strategy", `{"security":"tls","sockopt":{"domainStrategy":"ForceIPv4","dialerProxy":"detour"}}`, "", true},
		{"missing stream", `null`, "", false},
		{"missing TLS", `{"network":"raw"}`, "", false},
		{"non raw transport", `{"network":"ws","security":"tls"}`, "", false},
		{"uTLS fingerprint", `{"security":"tls","tlsSettings":{"fingerprint":"chrome"}}`, "", false},
		{"custom ALPN", `{"security":"tls","tlsSettings":{"alpn":["h2"]}}`, "", false},
		{"TLS version override", `{"security":"tls","tlsSettings":{"minVersion":"1.2"}}`, "", false},
		{"root override", `{"security":"tls","tlsSettings":{"disableSystemRoot":true}}`, "", false},
		{"ECH override", `{"security":"tls","tlsSettings":{"echConfigList":"example.com"}}`, "", false},
		{"normalized pin override", `{"security":"tls","tlsSettings":{"pinnedPeerCertSha256":" , "}}`, "", false},
		{"normalized name override", `{"security":"tls","tlsSettings":{"verifyPeerCertByName":" , "}}`, "", false},
		{"custom Happy Eyeballs", `{"security":"tls","sockopt":{"happyEyeballs":{"tryDelayMs":250}}}`, "", false},
		{"address port strategy", `{"security":"tls","sockopt":{"addressPortStrategy":"srvportandaddress"}}`, "", false},
		{"Mux Cool", `{"security":"tls"}`, `,"mux":{"enabled":true}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := `{"protocol":"naive","settings":{"address":"example.com","port":443,"quic":true},"streamSettings":` + tc.stream + tc.mux + `}`
			config := new(OutboundDetourConfig)
			if err := json.Unmarshal([]byte(input), config); err != nil {
				t.Fatal(err)
			}
			_, err := config.Build()
			if tc.valid && err != nil {
				t.Fatal(err)
			}
			if !tc.valid && err == nil {
				t.Fatal("expected invalid Naive stream settings to fail")
			}
		})
	}

	for _, field := range []string{"pinnedPeerCertSha256", "verifyPeerCertByName"} {
		t.Run("mixed case protocol "+field, func(t *testing.T) {
			input := `{"protocol":"NaIvE","settings":{"address":"example.com","port":443},"streamSettings":{"security":"tls","tlsSettings":{"` + field + `":" , "}}}`
			config := new(OutboundDetourConfig)
			if err := json.Unmarshal([]byte(input), config); err != nil {
				t.Fatal(err)
			}
			if _, err := config.Build(); err == nil {
				t.Fatal("expected unsupported TLS override to fail")
			}
		})
	}
}

func TestNaiveOutboundAuthorityCertificate(t *testing.T) {
	certificate, err := cert.Generate(nil)
	if err != nil {
		t.Fatal(err)
	}
	certificatePEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate}))
	for _, usage := range []string{"verify", "encipherment", "issue"} {
		t.Run(usage, func(t *testing.T) {
			settings := json.RawMessage(`{"address":"example.com","port":443}`)
			config := &OutboundDetourConfig{
				Protocol: "naive",
				Settings: &settings,
				StreamSetting: &StreamConfig{
					Security: "tls",
					TLSSettings: &TLSConfig{
						ServerName: "proxy.example",
						Certs: []*TLSCertConfig{{
							CertStr: []string{certificatePEM},
							Usage:   usage,
						}},
					},
				},
			}
			_, err := config.Build()
			if usage == "verify" && err != nil {
				t.Fatal(err)
			}
			if usage != "verify" && err == nil {
				t.Fatal("expected a non-authority-verification certificate to fail")
			}
		})
	}
}
