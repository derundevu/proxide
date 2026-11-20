package naive

import (
	"context"
	"fmt"
	"strings"

	"github.com/xtls/xray-core/app/proxyman"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/tcp"
	"github.com/xtls/xray-core/transport/internet/tls"
	"golang.org/x/net/http/httpguts"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// ValidateClientConfig validates the native CONNECT request, including headers
// which would otherwise override Chromium's internal Naive control headers.
func ValidateClientConfig(config *ClientConfig) error {
	if config == nil || config.Address == nil {
		return fmt.Errorf("naive: address is required")
	}
	switch address := config.Address.Address.(type) {
	case *net.IPOrDomain_Ip:
		if len(address.Ip) != 4 && len(address.Ip) != 16 {
			return fmt.Errorf("naive: invalid IP address")
		}
	case *net.IPOrDomain_Domain:
		if address.Domain == "" || strings.ContainsAny(address.Domain, "\x00\r\n /:#?@[]\\") {
			return fmt.Errorf("naive: invalid server domain")
		}
	default:
		return fmt.Errorf("naive: address is required")
	}
	if config.Port == 0 || config.Port > 65535 {
		return fmt.Errorf("naive: port must be between 1 and 65535")
	}
	switch config.QuicCongestionControl {
	case "":
	case "bbr", "bbr2", "cubic", "reno":
		if !config.Quic {
			return fmt.Errorf("naive: quicCongestionControl requires quic")
		}
	default:
		return fmt.Errorf("naive: unsupported quicCongestionControl %q", config.QuicCongestionControl)
	}
	if strings.Contains(config.Username, ":") {
		return fmt.Errorf("naive: username must not contain a colon")
	}
	if config.Username == "" && config.Password != "" {
		return fmt.Errorf("naive: password requires a username")
	}
	seen := make(map[string]bool, len(config.Headers))
	for name, value := range config.Headers {
		key := strings.ToLower(name)
		if !httpguts.ValidHeaderFieldName(name) || strings.HasPrefix(key, "-") || !httpguts.ValidHeaderFieldValue(value) {
			return fmt.Errorf("naive: invalid header %q", name)
		}
		switch key {
		case "host", "connection", "proxy-connection", "keep-alive", "transfer-encoding", "upgrade", "te", "trailer", "content-length", "proxy-authorization", "padding":
			return fmt.Errorf("naive: reserved header %q", name)
		}
		if seen[key] {
			return fmt.Errorf("naive: duplicate case-insensitive header %q", name)
		}
		seen[key] = true
	}
	return nil
}

// ValidateSender rejects dynamic sendThrough modes which Chromium's shared
// connection pool cannot reproduce: one physical HTTP/2 or HTTP/3 socket may
// carry streams opened from different inbound connections, and cronet-go's
// dialer callback receives no per-stream context. A fixed IP or CIDR remains a
// plain outbound-level socket bind and is supported.
func ValidateSender(sender *proxyman.SenderConfig) error {
	if sender == nil || sender.Via == nil {
		return nil
	}
	switch domain := sender.Via.GetDomain(); domain {
	case "origin", "srcip":
		return fmt.Errorf(`naive: sendThrough %q is unsupported; use a fixed IP or CIDR`, domain)
	}
	return nil
}

// ValidateTransport rejects options that Chromium cannot honor. Supported socket
// options remain Xray's responsibility and use the raw handler dial path.
func ValidateTransport(stream *internet.StreamConfig, mux *proxyman.MultiplexingConfig) error {
	if mux != nil && mux.Enabled {
		return fmt.Errorf("naive: Mux.Cool/XUDP is unsupported; use native HTTP multiplexing and udpOverTcp")
	}
	if stream == nil || stream.GetEffectiveProtocol() != "tcp" {
		return fmt.Errorf("naive: streamSettings network must be raw/tcp")
	}
	if stream.Address != nil || stream.Port != 0 || len(stream.Tcpmasks) != 0 || len(stream.Udpmasks) != 0 || stream.QuicParams != nil {
		return fmt.Errorf("naive: stream address/port overrides, masks and quicParams are unsupported")
	}
	if err := rejectFields(stream, "protocol_name", "transport_settings", "security_type", "security_settings", "socket_settings"); err != nil {
		return err
	}
	if socket := stream.SocketSettings; socket != nil {
		if _, known := internet.DomainStrategy_name[int32(socket.DomainStrategy)]; !known {
			return fmt.Errorf("naive: unknown sockopt.domainStrategy")
		}
		if socket.AddressPortStrategy != internet.AddressPortStrategy_None {
			return fmt.Errorf("naive: sockopt.addressPortStrategy is unsupported")
		}
		if racing := socket.HappyEyeballs; racing != nil {
			if len(racing.ProtoReflect().GetUnknown()) != 0 {
				return fmt.Errorf("naive: unknown sockopt.happyEyeballs options are unsupported")
			}
			// Match DialSystem's activation condition. JSON injects nonzero
			// interleave/concurrency defaults even when racing is disabled.
			if racing.TryDelayMs > 0 && racing.MaxConcurrentTry > 0 {
				return fmt.Errorf("naive: enabled sockopt.happyEyeballs is unsupported; Chromium manages connection racing")
			}
		}
	}
	for _, transport := range stream.TransportSettings {
		if transport == nil || transport.ProtocolName != "tcp" || transport.Settings == nil {
			return fmt.Errorf("naive: non-raw transport settings are unsupported")
		}
		settings, err := transport.GetTypedSettings()
		if err != nil {
			return err
		}
		raw, ok := settings.(*tcp.Config)
		if !ok || raw == nil || raw.HeaderSettings != nil || raw.AcceptProxyProtocol || len(raw.ProtoReflect().GetUnknown()) != 0 {
			return fmt.Errorf("naive: raw header and PROXY protocol settings are unsupported")
		}
	}
	if stream.SecurityType != "xray.transport.internet.tls.Config" || len(stream.SecuritySettings) != 1 || stream.SecuritySettings[0] == nil || stream.SecuritySettings[0].Type != stream.SecurityType {
		return fmt.Errorf("naive: streamSettings security must be tls")
	}
	security, err := stream.GetEffectiveSecuritySettings()
	if err != nil {
		return err
	}
	settings, ok := security.(*tls.Config)
	if !ok {
		return fmt.Errorf("naive: streamSettings security must be tls")
	}
	return validateTLS(settings)
}

func rejectFields(message proto.Message, allowed ...protoreflect.Name) error {
	reflection := message.ProtoReflect()
	if len(reflection.GetUnknown()) != 0 {
		return fmt.Errorf("naive: unknown %s options are unsupported", reflection.Descriptor().FullName())
	}
	var unsupported protoreflect.Name
	reflection.Range(func(field protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		for _, name := range allowed {
			if field.Name() == name {
				return true
			}
		}
		unsupported = field.Name()
		return false
	})
	if unsupported != "" {
		return fmt.Errorf("naive: unsupported %s option %s", reflection.Descriptor().FullName(), unsupported)
	}
	return nil
}

func validateTLS(settings *tls.Config) error {
	if settings == nil {
		return fmt.Errorf("naive: TLS settings are required")
	}
	if err := rejectFields(settings, "server_name", "certificate"); err != nil {
		return err
	}
	if strings.ContainsAny(settings.ServerName, "\x00\r\n /:#?@[]\\") {
		// IPv6 literals are valid certificate identities, but brackets are not SNI.
		if net.ParseIP(settings.ServerName) == nil {
			return fmt.Errorf("naive: invalid TLS serverName")
		}
	}
	for _, certificate := range settings.Certificate {
		if certificate == nil || certificate.Usage != tls.Certificate_AUTHORITY_VERIFY {
			return fmt.Errorf("naive: only authorityVerify certificates are supported")
		}
		if err := rejectFields(certificate, "certificate", "certificate_path", "usage", "One_time_loading"); err != nil {
			return err
		}
		if len(certificate.Certificate) == 0 && certificate.CertificatePath == "" {
			return fmt.Errorf("naive: authorityVerify certificate is empty")
		}
	}
	return nil
}

func init() {
	common.Must(common.RegisterConfig((*ClientConfig)(nil), func(ctx context.Context, config interface{}) (interface{}, error) {
		return NewClient(ctx, config.(*ClientConfig))
	}))
}
