package conf_test

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/enfein/mieru/v3/pkg/appctl"
	pb "github.com/enfein/mieru/v3/pkg/appctl/appctlpb"
	"github.com/xtls/xray-core/infra/conf"
	"github.com/xtls/xray-core/proxy/mieru"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func mieruTestProfile() *pb.ClientProfile {
	return &pb.ClientProfile{
		ProfileName: proto.String("test profile"),
		User:        &pb.User{Name: proto.String("user@example.org"), Password: proto.String("p:/?#%@ ss")},
		Servers: []*pb.ServerEndpoint{{
			IpAddress: proto.String("2001:db8::1"),
			PortBindings: []*pb.PortBinding{
				{PortRange: proto.String("2090-2099"), Protocol: pb.TransportProtocol_TCP.Enum()},
				{Port: proto.Int32(2999), Protocol: pb.TransportProtocol_UDP.Enum()},
			},
		}},
		Mtu:           proto.Int32(1400),
		Multiplexing:  &pb.MultiplexingConfig{Level: pb.MultiplexingLevel_MULTIPLEXING_HIGH.Enum()},
		HandshakeMode: pb.HandshakeMode_HANDSHAKE_NO_WAIT.Enum(),
		TrafficPattern: &pb.TrafficPattern{
			Seed: proto.Int32(42), TcpFragment: &pb.TCPFragment{Enable: proto.Bool(true), MaxSleepMs: proto.Int32(10)},
		},
	}
}

func mieruNativeJSON(t *testing.T, value proto.Message) json.RawMessage {
	t.Helper()
	data, err := protojson.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func mieruSettingsJSON(t *testing.T, field string, value interface{}) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]interface{}{field: value})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func mieruBuildProfile(t *testing.T, settings []byte) *pb.ClientProfile {
	t.Helper()
	var config conf.MieruConfig
	if err := json.Unmarshal(settings, &config); err != nil {
		t.Fatal(err)
	}
	built, err := config.Build()
	if err != nil {
		t.Fatal(err)
	}
	profile := new(pb.ClientProfile)
	if err := proto.Unmarshal(built.(*mieru.ClientConfig).Profile, profile); err != nil {
		t.Fatal(err)
	}
	return profile
}

func TestMieruNativeProfileRoundTrip(t *testing.T) {
	profile := mieruTestProfile()
	profile.User.Password = nil
	profile.User.HashedPassword = proto.String(strings.Repeat("ab", 32))
	profile.Servers = append(profile.Servers, &pb.ServerEndpoint{
		DomainName:   proto.String("mieru.example.org"),
		PortBindings: []*pb.PortBinding{{Port: proto.Int32(443), Protocol: pb.TransportProtocol_TCP.Enum()}},
	})
	got := mieruBuildProfile(t, mieruSettingsJSON(t, "profile", mieruNativeJSON(t, profile)))
	if !proto.Equal(got, profile) {
		t.Fatal("native profile fields were lost or changed")
	}
}

func TestMieruOfficialShareURLs(t *testing.T) {
	profile := mieruTestProfile()
	links, err := appctl.ClientProfileToMultiURLs(profile)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := appctl.ClientConfigToURL(&pb.ClientConfig{Profiles: []*pb.ClientProfile{profile}})
	if err != nil {
		t.Fatal(err)
	}
	for _, link := range []string{links[0], encoded} {
		got := mieruBuildProfile(t, mieruSettingsJSON(t, "url", link))
		if !proto.Equal(got, profile) {
			t.Fatal("official URL did not preserve paired port protocols, ranges, escaped credentials, IPv6 or native options")
		}
	}
}

func TestMieruSimpleURLPairOrder(t *testing.T) {
	link := "mierus://user:p%40ss%3A%2F%3F%23%25@[2001:db8::2]?profile=hello%20world&port=443&protocol=UDP&port=8000-8002&protocol=TCP&port=8443&protocol=UDP"
	got := mieruBuildProfile(t, mieruSettingsJSON(t, "url", link))
	if got.GetUser().GetPassword() != "p@ss:/?#%" || got.GetProfileName() != "hello world" || got.Servers[0].GetIpAddress() != "2001:db8::2" {
		t.Fatal("escaped credentials, profile or IPv6 changed")
	}
	want := []*pb.PortBinding{
		{Port: proto.Int32(443), Protocol: pb.TransportProtocol_UDP.Enum()},
		{PortRange: proto.String("8000-8002"), Protocol: pb.TransportProtocol_TCP.Enum()},
		{Port: proto.Int32(8443), Protocol: pb.TransportProtocol_UDP.Enum()},
	}
	if len(got.Servers[0].PortBindings) != len(want) {
		t.Fatal("repeated bindings were lost")
	}
	for i, binding := range want {
		if !proto.Equal(got.Servers[0].PortBindings[i], binding) {
			t.Fatalf("port/protocol pairing changed at index %d", i)
		}
	}
}

func TestMieruFullClientProfileSelection(t *testing.T) {
	first := mieruTestProfile()
	second := proto.Clone(first).(*pb.ClientProfile)
	second.ProfileName = proto.String("second")
	second.User.Password = nil
	second.User.HashedPassword = proto.String(strings.Repeat("cd", 32))
	for _, profiles := range [][]*pb.ClientProfile{{second}, {first, second}} {
		config := &pb.ClientConfig{
			Profiles: profiles, ActiveProfile: proto.String("second"),
			Socks5Port: proto.Int32(1080), RpcPort: proto.Int32(1081), HttpProxyPort: proto.Int32(8080),
		}
		link, err := appctl.ClientConfigToURL(config)
		if err != nil {
			t.Fatal(err)
		}
		for _, settings := range [][]byte{
			mieruSettingsJSON(t, "config", mieruNativeJSON(t, config)),
			mieruSettingsJSON(t, "url", link),
		} {
			got := mieruBuildProfile(t, settings)
			if !proto.Equal(got, second) {
				t.Fatal("activeProfile did not select the exact profile")
			}
		}
	}
	config := &pb.ClientConfig{Profiles: []*pb.ClientProfile{second}}
	if !proto.Equal(mieruBuildProfile(t, mieruSettingsJSON(t, "config", mieruNativeJSON(t, config))), second) {
		t.Fatal("single profile without activeProfile was not selected")
	}
}

func TestMieruPanelNodes(t *testing.T) {
	pattern := mieruTestProfile().TrafficPattern
	patternBytes, err := proto.Marshal(pattern)
	if err != nil {
		t.Fatal(err)
	}
	patternBase64 := base64.StdEncoding.EncodeToString(patternBytes)
	tests := []struct {
		name     string
		node     map[string]interface{}
		bindings []*pb.PortBinding
	}{
		{
			name: "2s-ui Clash node",
			node: map[string]interface{}{
				"name": "panel node", "type": "mieru", "server": "example.org", "port": 2999,
				"transport": "TCP", "username": "panel-user", "password": "panel-pass", "udp": true,
				"multiplexing": "MULTIPLEXING_LOW", "handshake-mode": "HANDSHAKE_NO_WAIT", "mtu": 1400,
				"traffic-pattern": patternBase64,
			},
			bindings: []*pb.PortBinding{{Port: proto.Int32(2999), Protocol: pb.TransportProtocol_TCP.Enum()}},
		},
		{
			name: "Clash UDP range",
			node: map[string]interface{}{
				"type": "mieru", "server": "[2001:db8::1]", "port-range": "2090-2099", "transport": "UDP",
				"username": "panel-user", "password": "panel-pass", "multiplexing": "MULTIPLEXING_OFF",
			},
			bindings: []*pb.PortBinding{{PortRange: proto.String("2090-2099"), Protocol: pb.TransportProtocol_UDP.Enum()}},
		},
		{
			name: "Hiddify documented node",
			node: map[string]interface{}{
				"type": "mieru", "tag": "mieru-out", "server": "127.0.0.1", "server_port": 1080,
				"server_ports": []string{"9000-9010", "9020-9030"}, "transport": "TCP",
				"username": "asdf", "password": "hjkl", "multiplexing": "MULTIPLEXING_LOW",
			},
			bindings: []*pb.PortBinding{
				{Port: proto.Int32(1080), Protocol: pb.TransportProtocol_TCP.Enum()},
				{PortRange: proto.String("9000-9010"), Protocol: pb.TransportProtocol_TCP.Enum()},
				{PortRange: proto.String("9020-9030"), Protocol: pb.TransportProtocol_TCP.Enum()},
			},
		},
		{
			name: "Hiddify current portBindings node",
			node: map[string]interface{}{
				"type": "mieru", "tag": "mieru-out", "server": "127.0.0.1", "server_port": 1080,
				"portBindings": []interface{}{
					map[string]interface{}{"port": 1080, "protocol": "TCP"},
					map[string]interface{}{"portRange": "9000-9010", "protocol": "UDP"},
				},
				"username": "asdf", "password": "hjkl", "handshake_mode": "HANDSHAKE_STANDARD",
				"traffic_pattern": patternBase64,
			},
			bindings: []*pb.PortBinding{
				{Port: proto.Int32(1080), Protocol: pb.TransportProtocol_TCP.Enum()},
				{PortRange: proto.String("9000-9010"), Protocol: pb.TransportProtocol_UDP.Enum()},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := mieruBuildProfile(t, mieruSettingsJSON(t, "config", test.node))
			want := &pb.ServerEndpoint{PortBindings: test.bindings}
			if strings.Contains(test.node["server"].(string), ":") {
				want.IpAddress = proto.String("2001:db8::1")
			} else if test.node["server"] == "127.0.0.1" {
				want.IpAddress = proto.String("127.0.0.1")
			} else {
				want.DomainName = proto.String("example.org")
			}
			if !proto.Equal(got.Servers[0], want) {
				t.Fatal("node server or port/protocol bindings changed")
			}
			if got.GetUser().GetName() != test.node["username"] || got.GetUser().GetPassword() != test.node["password"] {
				t.Fatal("node credentials changed")
			}
			if test.node["traffic-pattern"] != nil || test.node["traffic_pattern"] != nil {
				if !proto.Equal(got.GetTrafficPattern(), pattern) {
					t.Fatal("node traffic pattern changed")
				}
			}
			if expected, ok := test.node["multiplexing"].(string); ok && got.GetMultiplexing().GetLevel().String() != expected {
				t.Fatal("node multiplexing changed")
			}
			if test.node["handshake-mode"] == "HANDSHAKE_NO_WAIT" && got.GetHandshakeMode() != pb.HandshakeMode_HANDSHAKE_NO_WAIT {
				t.Fatal("node handshake mode changed")
			}
			if test.node["mtu"] == 1400 && got.GetMtu() != 1400 {
				t.Fatal("node MTU changed")
			}
		})
	}
}

func mieruRequireError(t *testing.T, settings []byte) {
	t.Helper()
	var config conf.MieruConfig
	err := json.Unmarshal(settings, &config)
	if err == nil {
		_, err = config.Build()
	}
	if err == nil {
		t.Fatal("invalid Mieru settings accepted")
	}
	for _, secret := range []string{"secret-password", "mieru://secret", "sensitive-profile"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatal("Mieru error disclosed supplied credentials or URL")
		}
	}
}

func TestMieruRejectMalformedURLs(t *testing.T) {
	base := "mierus://user:secret-password@example.org?profile=sensitive-profile&port=443&protocol=TCP"
	for name, link := range map[string]string{
		"HTTP fetch":             "https://example.org/secret-password",
		"invalid base64":         "mieru://secret-password",
		"invalid protobuf":       "mieru://AQ==",
		"missing user":           "mierus://example.org?profile=x&port=443&protocol=TCP",
		"missing password":       "mierus://user@example.org?profile=x&port=443&protocol=TCP",
		"missing profile":        "mierus://user:secret-password@example.org?port=443&protocol=TCP",
		"missing ports":          "mierus://user:secret-password@example.org?profile=x",
		"unpaired ports":         base + "&port=444",
		"unknown protocol":       strings.Replace(base, "protocol=TCP", "protocol=TLS", 1),
		"inverted range":         strings.Replace(base, "port=443", "port=444-443", 1),
		"zero port":              strings.Replace(base, "port=443", "port=0", 1),
		"overflow port":          strings.Replace(base, "port=443", "port=65536", 1),
		"unknown multiplexing":   base + "&multiplexing=wrong",
		"unknown handshake":      base + "&handshake-mode=wrong",
		"bad traffic pattern":    base + "&traffic-pattern=AQ%3D%3D",
		"MTU overflow":           base + "&mtu=4294968696",
		"bad MTU":                base + "&mtu=1279",
		"duplicate scalar":       base + "&profile=another",
		"unknown option":         base + "&transport=UDP",
		"ignored authority port": strings.Replace(base, "example.org?", "example.org:443?", 1),
		"ignored path":           strings.Replace(base, "example.org?", "example.org/path?", 1),
		"bad escaping":           strings.Replace(base, "secret-password", "%zz", 1),
	} {
		t.Run(name, func(t *testing.T) {
			mieruRequireError(t, mieruSettingsJSON(t, "url", link))
		})
	}
}

func TestMieruRejectInvalidProfiles(t *testing.T) {
	mutations := map[string]func(*pb.ClientProfile){
		"missing name":            func(p *pb.ClientProfile) { p.ProfileName = nil },
		"missing credentials":     func(p *pb.ClientProfile) { p.User = nil },
		"password length":         func(p *pb.ClientProfile) { p.User.Password = proto.String(strings.Repeat("x", 65)) },
		"malformed hash":          func(p *pb.ClientProfile) { p.User.HashedPassword = proto.String("secret-password") },
		"short hash":              func(p *pb.ClientProfile) { p.User.HashedPassword = proto.String("aa") },
		"user quota":              func(p *pb.ClientProfile) { p.User.Quotas = []*pb.Quota{{}} },
		"missing servers":         func(p *pb.ClientProfile) { p.Servers = nil },
		"bad server IP":           func(p *pb.ClientProfile) { p.Servers[0].IpAddress = proto.String("not-an-ip") },
		"domain contains port":    func(p *pb.ClientProfile) { p.Servers[0].DomainName = proto.String("example.org:443") },
		"missing binding":         func(p *pb.ClientProfile) { p.Servers[0].PortBindings = nil },
		"both native port fields": func(p *pb.ClientProfile) { p.Servers[0].PortBindings[0].Port = proto.Int32(443) },
		"invalid enum":            func(p *pb.ClientProfile) { p.HandshakeMode = pb.HandshakeMode(999).Enum() },
		"invalid multiplexing":    func(p *pb.ClientProfile) { p.Multiplexing.Level = pb.MultiplexingLevel(999).Enum() },
		"invalid protocol":        func(p *pb.ClientProfile) { p.Servers[0].PortBindings[0].Protocol = pb.TransportProtocol(999).Enum() },
		"invalid MTU":             func(p *pb.ClientProfile) { p.Mtu = proto.Int32(1501) },
		"invalid traffic pattern": func(p *pb.ClientProfile) { p.TrafficPattern.TcpFragment.MaxSleepMs = proto.Int32(101) },
		"native dialer": func(p *pb.ClientProfile) {
			p.Dialer = &pb.ClientDialer{Protocol: pb.ProxyProtocol_SOCKS5_PROXY_PROTOCOL.Enum(), Host: proto.String("127.0.0.1"), Port: proto.Int32(1080)}
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			profile := mieruTestProfile()
			mutate(profile)
			mieruRequireError(t, mieruSettingsJSON(t, "profile", mieruNativeJSON(t, profile)))
		})
	}
}

func TestMieruRejectAmbiguousClientProfiles(t *testing.T) {
	profile := mieruTestProfile()
	for name, config := range map[string]*pb.ClientConfig{
		"empty":            {},
		"missing active":   {Profiles: []*pb.ClientProfile{profile, profile}},
		"unknown active":   {Profiles: []*pb.ClientProfile{profile}, ActiveProfile: proto.String("missing")},
		"duplicate active": {Profiles: []*pb.ClientProfile{profile, profile}, ActiveProfile: profile.ProfileName},
	} {
		t.Run(name, func(t *testing.T) {
			mieruRequireError(t, mieruSettingsJSON(t, "config", mieruNativeJSON(t, config)))
			link, err := appctl.ClientConfigToURL(config)
			if err != nil {
				t.Fatal(err)
			}
			mieruRequireError(t, mieruSettingsJSON(t, "url", link))
		})
	}
}

func TestMieruRejectInvalidNodes(t *testing.T) {
	base := map[string]interface{}{
		"type": "mieru", "server": "example.org", "port": 443, "transport": "TCP",
		"username": "user", "password": "secret-password",
	}
	for name, change := range map[string]func(map[string]interface{}){
		"wrong type":             func(n map[string]interface{}) { n["type"] = "shadowsocks" },
		"missing transport":      func(n map[string]interface{}) { delete(n, "transport") },
		"bad transport":          func(n map[string]interface{}) { n["transport"] = "tcp" },
		"both clash port fields": func(n map[string]interface{}) { n["port-range"] = "443-444" },
		"mixed dialect ports":    func(n map[string]interface{}) { n["server_port"] = 443 },
		"bad range":              func(n map[string]interface{}) { delete(n, "port"); n["port-range"] = "444-443" },
		"unknown field":          func(n map[string]interface{}) { n["detour"] = "direct" },
		"UDP restriction":        func(n map[string]interface{}) { n["udp"] = false },
		"wrong multiplexing":     func(n map[string]interface{}) { n["multiplexing"] = "low" },
		"wrong handshake":        func(n map[string]interface{}) { n["handshake_mode"] = "standard" },
		"duplicate handshake aliases": func(n map[string]interface{}) {
			n["handshake_mode"] = "HANDSHAKE_STANDARD"
			n["handshake-mode"] = "HANDSHAKE_STANDARD"
		},
		"duplicate traffic aliases": func(n map[string]interface{}) { n["traffic_pattern"] = ""; n["traffic-pattern"] = "" },
		"wrong traffic pattern":     func(n map[string]interface{}) { n["traffic-pattern"] = "secret-password" },
		"bad IPv6":                  func(n map[string]interface{}) { n["server"] = "[not-an-ip]" },
		"bad port type":             func(n map[string]interface{}) { n["port"] = "443" },
		"fractional port":           func(n map[string]interface{}) { n["port"] = 443.5 },
	} {
		t.Run(name, func(t *testing.T) {
			node := make(map[string]interface{}, len(base))
			for key, value := range base {
				node[key] = value
			}
			change(node)
			mieruRequireError(t, mieruSettingsJSON(t, "config", node))
		})
	}
}

func TestMieruSettingsBoundaries(t *testing.T) {
	profile := string(mieruNativeJSON(t, mieruTestProfile()))
	for name, settings := range map[string]string{
		"missing source":         `{}`,
		"empty URL":              `{"url":""}`,
		"two sources":            `{"url":"mierus://user:secret-password@host","profile":` + profile + `}`,
		"empty URL plus profile": `{"url":"","profile":` + profile + `}`,
		"two JSON sources":       `{"profile":` + profile + `,"config":{}}`,
		"null profile":           `{"profile":null}`,
		"null config":            `{"config":null}`,
		"config array":           `{"config":[]}`,
		"profile unknown field":  `{"profile":{"secret-password":"sensitive-profile"}}`,
		"unknown settings field": `{"profile":` + profile + `,"password":"secret-password"}`,
		"subscription wrapper":   `{"config":{"proxies":[]}}`,
	} {
		t.Run(name, func(t *testing.T) { mieruRequireError(t, []byte(settings)) })
	}
}

func TestMieruOutboundRegistryAndStreamRestrictions(t *testing.T) {
	profileJSON := mieruNativeJSON(t, mieruTestProfile())
	settings := json.RawMessage(mieruSettingsJSON(t, "profile", profileJSON))
	for _, stream := range []string{`null`, `{"network":"raw","sockopt":{"domainStrategy":"UseIP"}}`} {
		var outbound conf.OutboundDetourConfig
		data, err := json.Marshal(map[string]interface{}{"protocol": "mieru", "settings": settings, "streamSettings": json.RawMessage(stream)})
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &outbound); err != nil {
			t.Fatal(err)
		}
		built, err := outbound.Build()
		if err != nil {
			t.Fatal(err)
		}
		if built.ProxySettings.GetType() != "xray.proxy.mieru.ClientConfig" {
			t.Fatal("outbound registry did not build Mieru settings")
		}
	}
	for name, extra := range map[string]map[string]interface{}{
		"Xray mux":       {"mux": map[string]interface{}{"enabled": true}},
		"TLS":            {"streamSettings": map[string]interface{}{"security": "tls"}},
		"WebSocket":      {"streamSettings": map[string]interface{}{"network": "ws"}},
		"TCP header":     {"streamSettings": map[string]interface{}{"rawSettings": map[string]interface{}{"header": map[string]interface{}{"type": "http"}}}},
		"proxy protocol": {"streamSettings": map[string]interface{}{"rawSettings": map[string]interface{}{"acceptProxyProtocol": true}}},
	} {
		t.Run(name, func(t *testing.T) {
			extra["protocol"] = "mieru"
			extra["settings"] = settings
			data, err := json.Marshal(extra)
			if err != nil {
				t.Fatal(err)
			}
			var outbound conf.OutboundDetourConfig
			if err := json.Unmarshal(data, &outbound); err != nil {
				t.Fatal(err)
			}
			if _, err := outbound.Build(); err == nil {
				t.Fatal("incompatible Xray stream or mux settings accepted")
			}
		})
	}
}

func TestMieruSimpleURLNumericProtocolRejected(t *testing.T) {
	u := url.URL{Scheme: "mierus", Host: "example.org", User: url.UserPassword("user", "secret-password")}
	q := url.Values{"profile": {"test"}, "port": {"443"}, "protocol": {"2"}}
	u.RawQuery = q.Encode()
	mieruRequireError(t, mieruSettingsJSON(t, "url", u.String()))
}

func TestMieruPanelDefaultsAndPortList(t *testing.T) {
	got := mieruBuildProfile(t, []byte(`{"config":{
		"type":"mieru","server":"example.org","server_ports":["443","8000-8002"],
		"transport":"UDP","username":"user","password":"password"
	}}`))
	if got.GetProfileName() != "mieru" || got.Multiplexing != nil || got.HandshakeMode != nil || got.Mtu != nil {
		t.Fatal("omitted panel options must retain native defaults")
	}
	want := &pb.ServerEndpoint{
		DomainName: proto.String("example.org"),
		PortBindings: []*pb.PortBinding{
			{Port: proto.Int32(443), Protocol: pb.TransportProtocol_UDP.Enum()},
			{PortRange: proto.String("8000-8002"), Protocol: pb.TransportProtocol_UDP.Enum()},
		},
	}
	if !proto.Equal(got.Servers[0], want) {
		t.Fatal("single and ranged server_ports were not preserved")
	}
}

func TestMieruPanelExplicitBindingDoesNotLoseTransport(t *testing.T) {
	got := mieruBuildProfile(t, []byte(`{"config":{
		"type":"mieru","server":"example.org","server_port":443,"transport":"TCP",
		"portBindings":[{"port":443,"protocol":"UDP"}],"username":"user","password":"password"
	}}`))
	want := &pb.ServerEndpoint{
		DomainName: proto.String("example.org"),
		PortBindings: []*pb.PortBinding{
			{Port: proto.Int32(443), Protocol: pb.TransportProtocol_UDP.Enum()},
			{Port: proto.Int32(443), Protocol: pb.TransportProtocol_TCP.Enum()},
		},
	}
	if !proto.Equal(got.Servers[0], want) {
		t.Fatal("server_port transport was lost when explicit binding used the same port")
	}
	mieruRequireError(t, []byte(`{"config":{
		"type":"mieru","server":"example.org","server_port":0,
		"portBindings":[{"portRange":"8000-8002","protocol":"UDP"}],"username":"user","password":"password"
	}}`))
}
