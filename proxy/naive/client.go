//go:build with_naive_outbound && ((linux && !android && (386 || amd64 || arm || arm64 || loong64 || mipsle || riscv64 || (mips64le && !with_musl))) || (android && (386 || amd64 || arm || arm64)) || (darwin && (amd64 || arm64)) || (windows && (amd64 || arm64)))

package naive

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	stdnet "net"
	"os"
	"strings"
	"sync"
	"time"

	cronet "github.com/sagernet/cronet-go"
	_ "github.com/sagernet/cronet-go/all"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/common/uot"
	"github.com/xtls/xray-core/app/proxyman"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/common/signal"
	"github.com/xtls/xray-core/common/task"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/dns"
	"github.com/xtls/xray-core/features/policy"
	"github.com/xtls/xray-core/transport"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/tls"
	"google.golang.org/protobuf/proto"
)

// Client owns one Chromium engine. Its socket context belongs to the outbound,
// not to whichever stream happened to establish the shared HTTP connection.
type Client struct {
	ctx            context.Context
	cancel         context.CancelFunc
	config         *ClientConfig
	tls            *tls.Config
	roots          string
	policyManager  policy.Manager
	dns            dns.Client
	domainStrategy internet.DomainStrategy
	dialerProxy    bool
	mu             sync.Mutex
	native         *cronet.NaiveClient
	closed         bool
	stopClose      func() bool
}

func NewClient(ctx context.Context, config *ClientConfig) (*Client, error) {
	if err := ValidateClientConfig(config); err != nil {
		return nil, err
	}
	handler := session.FullHandlerFromContext(ctx)
	if handler == nil || handler.SenderSettings() == nil {
		return nil, fmt.Errorf("naive: outbound sender settings are required")
	}
	senderObject, err := handler.SenderSettings().GetInstance()
	if err != nil {
		return nil, err
	}
	sender, ok := senderObject.(*proxyman.SenderConfig)
	if !ok {
		return nil, fmt.Errorf("naive: invalid outbound sender settings")
	}
	if err := ValidateSender(sender); err != nil {
		return nil, err
	}
	if err := ValidateTransport(sender.StreamSettings, sender.MultiplexSettings); err != nil {
		return nil, err
	}
	security, err := sender.StreamSettings.GetEffectiveSecuritySettings()
	if err != nil {
		return nil, err
	}
	tlsConfig := proto.Clone(security.(*tls.Config)).(*tls.Config)
	roots, err := loadRoots(tlsConfig)
	if err != nil {
		return nil, err
	}
	v := core.MustFromContext(ctx)
	dnsClient, ok := v.GetFeature(dns.ClientType()).(dns.Client)
	if !ok {
		return nil, fmt.Errorf("naive: Xray DNS feature is required")
	}
	lifetime, cancel := context.WithCancel(ctx)
	client := &Client{
		ctx: lifetime, cancel: cancel,
		config: proto.Clone(config).(*ClientConfig),
		tls:    tlsConfig, roots: roots,
		policyManager: v.GetFeature(policy.ManagerType()).(policy.Manager),
		dns:           dnsClient,
	}
	if socket := sender.StreamSettings.SocketSettings; socket != nil {
		client.domainStrategy = socket.DomainStrategy
		client.dialerProxy = socket.DialerProxy != ""
	}
	client.mu.Lock()
	client.stopClose = context.AfterFunc(lifetime, func() { client.Close() })
	client.mu.Unlock()
	return client, nil
}

func loadRoots(config *tls.Config) (string, error) {
	var roots strings.Builder
	for _, certificate := range config.Certificate {
		data := certificate.Certificate
		if len(data) == 0 {
			var err error
			data, err = os.ReadFile(certificate.CertificatePath)
			if err != nil {
				return "", fmt.Errorf("naive: read authorityVerify certificate: %w", err)
			}
		}
		remaining := data
		count := 0
		for len(remaining) > 0 {
			block, rest := pem.Decode(remaining)
			if block == nil {
				if len(strings.TrimSpace(string(remaining))) != 0 {
					return "", fmt.Errorf("naive: authorityVerify certificate must contain PEM certificates")
				}
				break
			}
			if block.Type != "CERTIFICATE" {
				return "", fmt.Errorf("naive: authorityVerify PEM must contain only certificates")
			}
			if _, err := x509.ParseCertificate(block.Bytes); err != nil {
				return "", fmt.Errorf("naive: invalid authorityVerify certificate: %w", err)
			}
			count++
			remaining = rest
		}
		if count == 0 {
			return "", fmt.Errorf("naive: authorityVerify certificate is empty")
		}
		roots.Write(data)
		roots.WriteByte('\n')
	}
	return roots.String(), nil
}

// open serializes native stream creation against engine shutdown. Only stream
// creation is locked; handshakes and all stream I/O remain concurrent.
func (c *Client) open(ctx context.Context, destination M.Socksaddr, dialer internet.Dialer, source context.Context) (cronet.NaiveConn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.ctx.Err() != nil {
		return nil, stdnet.ErrClosed
	}
	if c.native == nil {
		raw, ok := dialer.(rawDialer)
		if !ok {
			return nil, fmt.Errorf("naive: Xray dialer does not support raw Chromium sockets")
		}
		server := net.TCPDestination(c.config.Address.AsAddress(), net.Port(c.config.Port))
		outbound := &session.Outbound{Target: server, OriginalTarget: server, Name: "naive"}
		if outbounds := session.OutboundsFromContext(source); len(outbounds) != 0 {
			outbound.Tag = outbounds[len(outbounds)-1].Tag
			outbound.Gateway = outbounds[len(outbounds)-1].Gateway
		}
		dialer.SetOutboundGateway(source, outbound)
		adapter := &socketDialer{dialer: raw, server: server, strategy: c.domainStrategy, dialerProxy: c.dialerProxy, gateway: outbound.Gateway, tag: outbound.Tag}
		engineContext := session.ContextWithConnectionObservationDisabled(c.ctx)
		if sockopt := session.SockoptFromContext(source); sockopt != nil {
			engineContext = session.ContextWithSockopt(engineContext, &session.Sockopt{Mark: sockopt.Mark})
		}
		client, err := cronet.NewNaiveClient(cronet.NaiveClientOptions{
			Context:       engineContext,
			ServerAddress: socksaddr(server),
			ServerName:    c.tls.ServerName,
			Username:      c.config.Username, Password: c.config.Password,
			ExtraHeaders:            c.config.Headers,
			TrustedRootCertificates: c.roots,
			DNSResolver:             c.resolveDNS,
			Dialer:                  adapter,
			QUIC:                    c.config.Quic,
		})
		if err != nil {
			return nil, fmt.Errorf("naive: initialize Chromium: %w", err)
		}
		if err := client.Start(); err != nil {
			client.Close()
			return nil, fmt.Errorf("naive: start Chromium: %w", err)
		}
		c.native = client
	}
	return c.native.DialEarly(ctx, destination)
}

func (c *Client) Process(ctx context.Context, link *transport.Link, dialer internet.Dialer) error {
	outbounds := session.OutboundsFromContext(ctx)
	if len(outbounds) == 0 || !outbounds[len(outbounds)-1].Target.IsValid() {
		return fmt.Errorf("naive: target not specified")
	}
	outbound := outbounds[len(outbounds)-1]
	outbound.Name = "naive"
	outbound.CanSpliceCopy.Store(3)
	target := outbound.Target
	if target.Network != net.Network_TCP && target.Network != net.Network_UDP {
		return fmt.Errorf("naive: unsupported target network")
	}
	if target.Network == net.Network_UDP && !c.config.UdpOverTcp {
		return fmt.Errorf("naive: UDP requires udpOverTcp and a server supporting sing UoT v2")
	}
	p := c.policyManager.ForLevel(c.config.Level)
	streamContext := ctx
	if session.TimeoutOnlyFromContext(ctx) {
		streamContext = session.ConnectionObservationDetachedContext(ctx)
	}
	streamContext, cancel := context.WithCancel(streamContext)
	defer cancel()
	stopLifetime := context.AfterFunc(c.ctx, cancel)
	defer stopLifetime()
	destination := socksaddr(target)
	if target.Network == net.Network_UDP {
		destination = uot.RequestDestination(uot.Version)
	}
	conn, err := c.open(streamContext, destination, dialer, ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	stopStream := context.AfterFunc(streamContext, func() { conn.Close() })
	defer stopStream()
	handshakeContext := streamContext
	var stopHandshake context.CancelFunc
	if p.Timeouts.Handshake != 0 {
		handshakeContext, stopHandshake = context.WithTimeout(streamContext, p.Timeouts.Handshake)
	}
	err = conn.HandshakeContext(handshakeContext)
	if stopHandshake != nil {
		stopHandshake()
	}
	if err != nil {
		return fmt.Errorf("naive: CONNECT handshake failed: %w", err)
	}
	var connection net.Conn = conn
	var packetConnection *uot.Conn
	if target.Network == net.Network_UDP {
		if p.Timeouts.Handshake != 0 {
			if err := conn.SetWriteDeadline(time.Now().Add(p.Timeouts.Handshake)); err != nil {
				return err
			}
		}
		client := uot.Client{Version: uot.Version}
		packetConnection, err = client.DialConn(conn, false, socksaddr(target))
		if err != nil {
			return fmt.Errorf("naive: UoT handshake failed: %w", err)
		}
		if p.Timeouts.Handshake != 0 {
			if err := conn.SetWriteDeadline(time.Time{}); err != nil {
				return err
			}
		}
	}
	timer := signal.CancelAfterInactivity(streamContext, cancel, p.Timeouts.ConnectionIdle)
	defer timer.SetTimeout(0)
	request := func() error {
		defer timer.SetTimeout(p.Timeouts.DownlinkOnly)
		if target.Network == net.Network_UDP {
			return buf.Copy(link.Reader, &packetWriter{connection: packetConnection, destination: target}, buf.UpdateActivity(timer))
		}
		// Cronet does not expose CloseWrite. Preserve the response side after
		// uplink EOF, using Xray's normal downlink-only timeout.
		return buf.Copy(link.Reader, buf.NewWriter(connection), buf.UpdateActivity(timer))
	}
	response := func() error {
		defer timer.SetTimeout(p.Timeouts.UplinkOnly)
		if target.Network == net.Network_UDP {
			return buf.Copy(&packetReader{connection: packetConnection}, link.Writer, buf.UpdateActivity(timer))
		}
		return buf.Copy(buf.NewReader(connection), link.Writer, buf.UpdateActivity(timer))
	}
	if err := task.Run(streamContext, request, task.OnSuccess(response, task.Close(link.Writer))); err != nil {
		common.Interrupt(link.Reader)
		common.Interrupt(link.Writer)
		return errors.New("naive: connection ends").Base(err)
	}
	return nil
}

func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	client := c.native
	c.mu.Unlock()
	if c.stopClose != nil {
		c.stopClose()
	}
	c.cancel()
	if client != nil {
		return client.Close()
	}
	return nil
}
