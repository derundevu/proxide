package conf

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/enfein/mieru/v3/pkg/appctl"
	"github.com/enfein/mieru/v3/pkg/appctl/appctlcommon"
	pb "github.com/enfein/mieru/v3/pkg/appctl/appctlpb"
	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/proxy/mieru"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// MieruConfig accepts a share link, a native profile, or a native client / panel node.
type MieruConfig struct {
	URL     string          `json:"url"`
	Profile json.RawMessage `json:"profile"`
	Config  json.RawMessage `json:"config"`
	urlSet  bool
}

// UnmarshalJSON decodes Mieru settings and rejects unsupported settings fields.
func (c *MieruConfig) UnmarshalJSON(data []byte) error {
	type plain MieruConfig
	var decoded plain
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&decoded) != nil {
		return errors.New("mieru: invalid settings JSON or unsupported settings field")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return errors.New("mieru: settings must be a JSON object")
	}
	_, decoded.urlSet = fields["url"]
	*c = MieruConfig(decoded)
	return nil
}

// Build validates and encodes one native profile, rejecting unsupported dialers and node fields.
func (c *MieruConfig) Build() (proto.Message, error) {
	count := 0
	if c.URL != "" || c.urlSet {
		count++
	}
	if len(c.Profile) != 0 {
		count++
	}
	if len(c.Config) != 0 {
		count++
	}
	if count != 1 {
		return nil, errors.New("mieru: specify exactly one of url, profile or config")
	}

	var profile *pb.ClientProfile
	var err error
	switch {
	case c.URL != "":
		if strings.HasPrefix(c.URL, "mierus://") {
			if err = validateMieruSimpleURL(c.URL); err != nil {
				return nil, err
			}
			profile, err = appctl.URLToClientProfile(c.URL)
		} else if strings.HasPrefix(c.URL, "mieru://") {
			var config *pb.ClientConfig
			config, err = appctl.URLToClientConfig(c.URL)
			if err == nil {
				profile, err = selectMieruProfile(config)
			}
		} else {
			return nil, errors.New("mieru: URL must use mieru:// or mierus://")
		}
		if err != nil {
			// Upstream errors may contain the URL, user info, or other supplied values.
			return nil, errors.New("mieru: invalid share URL or ambiguous profile selection")
		}
	case len(c.Profile) != 0:
		profile = new(pb.ClientProfile)
		if err := protojson.Unmarshal(c.Profile, profile); err != nil {
			return nil, errors.New("mieru: invalid native profile JSON")
		}
	default:
		var object map[string]json.RawMessage
		if json.Unmarshal(c.Config, &object) != nil || object == nil {
			return nil, errors.New("mieru: config must be a JSON object")
		}
		if _, node := object["type"]; node {
			profile, err = parseMieruNode(c.Config)
		} else {
			config := new(pb.ClientConfig)
			if protojson.Unmarshal(c.Config, config) != nil {
				return nil, errors.New("mieru: invalid native client JSON")
			}
			profile, err = selectMieruProfile(config)
		}
		if err != nil {
			return nil, err
		}
	}
	if err := validateMieruProfile(profile); err != nil {
		return nil, err
	}
	encoded, err := proto.Marshal(profile)
	if err != nil {
		return nil, errors.New("mieru: failed to encode profile")
	}
	return &mieru.ClientConfig{Profile: encoded}, nil
}

func selectMieruProfile(config *pb.ClientConfig) (*pb.ClientProfile, error) {
	name := config.GetActiveProfile()
	if name == "" && len(config.GetProfiles()) == 1 {
		return config.Profiles[0], nil
	}
	var selected *pb.ClientProfile
	for _, profile := range config.GetProfiles() {
		if name != "" && profile.GetProfileName() == name {
			if selected != nil {
				return nil, errors.New("mieru: activeProfile must identify exactly one profile")
			}
			selected = profile
		}
	}
	if selected == nil {
		return nil, errors.New("mieru: activeProfile must identify exactly one profile")
	}
	return selected, nil
}

func validateMieruProfile(profile *pb.ClientProfile) error {
	if profile == nil {
		return errors.New("mieru: profile is missing")
	}
	if profile.Dialer != nil {
		return errors.New("mieru: native profile dialer is unsupported; use Xray routing and sockopt")
	}
	if appctlcommon.ValidateClientConfigSingleProfile(profile) != nil {
		return errors.New("mieru: invalid native profile credentials, servers, port bindings, MTU or traffic pattern")
	}
	for _, server := range profile.GetServers() {
		if host := server.GetDomainName(); strings.ContainsAny(host, ":/[] \t\r\n") {
			return errors.New("mieru: server domainName must not contain a URL, port or whitespace")
		}
		for _, binding := range server.GetPortBindings() {
			if binding.GetPort() != 0 && binding.GetPortRange() != "" {
				return errors.New("mieru: native port and portRange are mutually exclusive")
			}
		}
	}
	if _, ok := pb.MultiplexingLevel_name[int32(profile.GetMultiplexing().GetLevel())]; !ok {
		return errors.New("mieru: invalid multiplexing level")
	}
	if _, ok := pb.HandshakeMode_name[int32(profile.GetHandshakeMode())]; !ok {
		return errors.New("mieru: invalid handshake mode")
	}
	if hash := profile.GetUser().GetHashedPassword(); hash != "" {
		decoded, err := hex.DecodeString(hash)
		if err != nil || len(decoded) != sha256.Size {
			return errors.New("mieru: hashedPassword must be a hex-encoded SHA-256 hash")
		}
	}
	return nil
}

// The upstream simple-link parser defaults unrecognized enum names to zero and
// narrows MTU to int32. Check these boundaries before calling that parser.
func validateMieruSimpleURL(link string) error {
	u, err := url.Parse(link)
	if err != nil || u.Opaque != "" || u.Path != "" || u.Port() != "" {
		return errors.New("mieru: invalid simple share URL")
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return errors.New("mieru: invalid simple share URL query")
	}
	for key, values := range query {
		switch key {
		case "port", "protocol":
		case "profile", "mtu", "multiplexing", "handshake-mode", "traffic-pattern":
			if len(values) != 1 || values[0] == "" {
				return errors.New("mieru: invalid or repeated simple share URL option")
			}
		default:
			return errors.New("mieru: unsupported simple share URL option")
		}
	}
	if value := query.Get("mtu"); value != "" {
		if _, err := strconv.ParseInt(value, 10, 32); err != nil {
			return errors.New("mieru: invalid simple share URL MTU")
		}
	}
	if value := query.Get("multiplexing"); value != "" {
		if _, ok := pb.MultiplexingLevel_value[value]; !ok {
			return errors.New("mieru: invalid multiplexing level")
		}
	}
	if value := query.Get("handshake-mode"); value != "" {
		if _, ok := pb.HandshakeMode_value[value]; !ok {
			return errors.New("mieru: invalid handshake mode")
		}
	}
	return nil
}

type mieruNode struct {
	Type         string            `json:"type"`
	Name         string            `json:"name"`
	Tag          string            `json:"tag"`
	Server       string            `json:"server"`
	Port         *int32            `json:"port"`
	PortRange    *string           `json:"port-range"`
	ServerPort   *int32            `json:"server_port"`
	ServerPorts  []string          `json:"server_ports"`
	PortBindings []*pb.PortBinding `json:"-"`
	Bindings     json.RawMessage   `json:"portBindings"`
	Transport    string            `json:"transport"`
	Username     string            `json:"username"`
	Password     string            `json:"password"`
	Multiplexing string            `json:"multiplexing"`
	MTU          *int32            `json:"mtu"`
	Handshake    *string           `json:"handshake-mode"`
	HandshakeAlt *string           `json:"handshake_mode"`
	Traffic      *string           `json:"traffic-pattern"`
	TrafficAlt   *string           `json:"traffic_pattern"`
	UDP          *bool             `json:"udp"`
}

func parseMieruNode(data []byte) (*pb.ClientProfile, error) {
	var node mieruNode
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&node) != nil || node.Type != "mieru" {
		return nil, errors.New("mieru: invalid node JSON or unsupported node field")
	}
	if node.UDP != nil && !*node.UDP {
		return nil, errors.New("mieru: udp:false restrictions are unsupported; use Xray routing")
	}
	if node.ServerPort != nil && (*node.ServerPort < 1 || *node.ServerPort > 65535) {
		return nil, errors.New("mieru: invalid node server_port")
	}
	if node.Port != nil && node.PortRange != nil {
		return nil, errors.New("mieru: node port and port-range are mutually exclusive")
	}
	if (node.Port != nil || node.PortRange != nil) && (node.ServerPort != nil || len(node.ServerPorts) != 0 || len(node.Bindings) != 0) {
		return nil, errors.New("mieru: do not mix Clash and sing-box port fields")
	}
	protocol := pb.TransportProtocol_UNKNOWN_TRANSPORT_PROTOCOL
	if node.Transport != "" {
		value, ok := pb.TransportProtocol_value[node.Transport]
		if !ok || value == 0 {
			return nil, errors.New("mieru: node transport must be TCP or UDP")
		}
		protocol = pb.TransportProtocol(value)
	}
	if len(node.Bindings) != 0 {
		// Native proto JSON keeps protocol enum names and port ranges intact.
		wrapper := append([]byte(`{"portBindings":`), node.Bindings...)
		wrapper = append(wrapper, '}')
		server := new(pb.ServerEndpoint)
		if protojson.Unmarshal(wrapper, server) != nil {
			return nil, errors.New("mieru: invalid node portBindings")
		}
		node.PortBindings = server.PortBindings
	}
	if protocol == pb.TransportProtocol_UNKNOWN_TRANSPORT_PROTOCOL && (node.Port != nil || node.PortRange != nil || len(node.ServerPorts) != 0 || len(node.PortBindings) == 0) {
		return nil, errors.New("mieru: node transport must be TCP or UDP")
	}
	bindings := node.PortBindings
	if node.Port != nil {
		bindings = append(bindings, &pb.PortBinding{Port: node.Port, Protocol: protocol.Enum()})
	}
	if node.PortRange != nil {
		bindings = append(bindings, &pb.PortBinding{PortRange: node.PortRange, Protocol: protocol.Enum()})
	}
	if node.ServerPort != nil {
		// Hiddify's newer schema repeats server_port in portBindings. Avoid
		// requiring an unused transport when an explicit binding already covers it.
		found := false
		for _, binding := range bindings {
			if binding.GetPort() == *node.ServerPort && (protocol == pb.TransportProtocol_UNKNOWN_TRANSPORT_PROTOCOL || binding.GetProtocol() == protocol) {
				found = true
			}
		}
		if !found {
			if protocol == pb.TransportProtocol_UNKNOWN_TRANSPORT_PROTOCOL {
				return nil, errors.New("mieru: node server_port requires a transport or matching portBinding")
			}
			bindings = append(bindings, &pb.PortBinding{Port: node.ServerPort, Protocol: protocol.Enum()})
		}
	}
	for _, ports := range node.ServerPorts {
		if port, err := strconv.ParseInt(ports, 10, 32); err == nil {
			bindings = append(bindings, &pb.PortBinding{Port: proto.Int32(int32(port)), Protocol: protocol.Enum()})
		} else {
			bindings = append(bindings, &pb.PortBinding{PortRange: proto.String(ports), Protocol: protocol.Enum()})
		}
	}
	name := node.Name
	if name == "" {
		name = node.Tag
	}
	if name == "" {
		name = "mieru"
	}
	server := &pb.ServerEndpoint{PortBindings: bindings}
	host := node.Server
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
		if net.ParseIP(host) == nil {
			return nil, errors.New("mieru: invalid bracketed server IP")
		}
	}
	if net.ParseIP(host) != nil {
		server.IpAddress = proto.String(host)
	} else {
		server.DomainName = proto.String(host)
	}
	profile := &pb.ClientProfile{
		ProfileName: proto.String(name),
		User:        &pb.User{Name: proto.String(node.Username), Password: proto.String(node.Password)},
		Servers:     []*pb.ServerEndpoint{server},
		Mtu:         node.MTU,
	}
	if node.Multiplexing != "" {
		value, ok := pb.MultiplexingLevel_value[node.Multiplexing]
		if !ok {
			return nil, errors.New("mieru: invalid multiplexing level")
		}
		profile.Multiplexing = &pb.MultiplexingConfig{Level: pb.MultiplexingLevel(value).Enum()}
	}
	if node.Handshake != nil && node.HandshakeAlt != nil || node.Traffic != nil && node.TrafficAlt != nil {
		return nil, errors.New("mieru: duplicate node option aliases")
	}
	if node.Handshake == nil {
		node.Handshake = node.HandshakeAlt
	}
	if node.Handshake != nil {
		value, ok := pb.HandshakeMode_value[*node.Handshake]
		if !ok {
			return nil, errors.New("mieru: invalid handshake mode")
		}
		profile.HandshakeMode = pb.HandshakeMode(value).Enum()
	}
	if node.Traffic == nil {
		node.Traffic = node.TrafficAlt
	}
	if node.Traffic != nil && *node.Traffic != "" {
		encoded, err := base64.StdEncoding.DecodeString(*node.Traffic)
		profile.TrafficPattern = new(pb.TrafficPattern)
		if err != nil || proto.Unmarshal(encoded, profile.TrafficPattern) != nil {
			return nil, errors.New("mieru: invalid node traffic pattern")
		}
	}
	return profile, nil
}
