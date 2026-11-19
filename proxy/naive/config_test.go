package naive

import (
	"testing"

	"github.com/xtls/xray-core/app/proxyman"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/tls"
)

func TestValidateSenderRejectsDynamicSendThrough(t *testing.T) {
	for _, domain := range []string{"origin", "srcip"} {
		sender := &proxyman.SenderConfig{Via: &net.IPOrDomain{Address: &net.IPOrDomain_Domain{Domain: domain}}}
		if err := ValidateSender(sender); err == nil {
			t.Fatalf("accepted sendThrough %q", domain)
		}
	}
	for _, sender := range []*proxyman.SenderConfig{
		nil,
		{},
		{Via: &net.IPOrDomain{Address: &net.IPOrDomain_Ip{Ip: []byte{192, 0, 2, 1}}}},
		{Via: &net.IPOrDomain{Address: &net.IPOrDomain_Domain{Domain: "origin.example"}}},
	} {
		if err := ValidateSender(sender); err != nil {
			t.Fatalf("rejected supported sender: %v", err)
		}
	}
}

func TestValidateTransportRejectsUnimplementedSocketOptions(t *testing.T) {
	for _, test := range []struct {
		name   string
		socket *internet.SocketConfig
		valid  bool
	}{
		{"default", &internet.SocketConfig{}, true},
		{"emptyHappyEyeballs", &internet.SocketConfig{HappyEyeballs: &internet.HappyEyeballsConfig{}}, true},
		{"injectedHappyEyeballsDefaults", &internet.SocketConfig{HappyEyeballs: &internet.HappyEyeballsConfig{Interleave: 1, MaxConcurrentTry: 4}}, true},
		{"disabledHappyEyeballsConcurrency", &internet.SocketConfig{HappyEyeballs: &internet.HappyEyeballsConfig{TryDelayMs: 250}}, true},
		{"domainStrategy", &internet.SocketConfig{DomainStrategy: internet.DomainStrategy_FORCE_IP4}, true},
		{"invalidDomainStrategy", &internet.SocketConfig{DomainStrategy: internet.DomainStrategy(99)}, false},
		{"srv", &internet.SocketConfig{AddressPortStrategy: internet.AddressPortStrategy_SrvPortAndAddress}, false},
		{"enabledHappyEyeballs", &internet.SocketConfig{HappyEyeballs: &internet.HappyEyeballsConfig{TryDelayMs: 250, MaxConcurrentTry: 4}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			stream := &internet.StreamConfig{
				ProtocolName:     "tcp",
				SecurityType:     "xray.transport.internet.tls.Config",
				SecuritySettings: []*serial.TypedMessage{serial.ToTypedMessage(&tls.Config{})},
				SocketSettings:   test.socket,
			}
			err := ValidateTransport(stream, nil)
			if test.valid && err != nil {
				t.Fatal(err)
			}
			if !test.valid && err == nil {
				t.Fatal("accepted unsupported socket options")
			}
		})
	}
}
