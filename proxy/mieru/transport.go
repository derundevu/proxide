package mieru

import (
	"context"
	"fmt"
	"net"
	"strings"

	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/transport/internet"
)

type dialerKey struct{}

type transportAdapter struct {
	owner *Client
}

// The mux supplies the context of the session that opens each underlay. Never
// store an individual request's context or dialer on this shared adapter.
func (a *transportAdapter) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" && network != "udp" && network != "udp4" && network != "udp6" {
		return nil, fmt.Errorf("mieru: unsupported transport network %q", network)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dialer, ok := ctx.Value(dialerKey{}).(internet.Dialer)
	if !ok || dialer == nil {
		return nil, fmt.Errorf("mieru: missing Xray transport dialer")
	}
	transport := "tcp"
	if strings.HasPrefix(network, "udp") {
		transport = "udp"
	}
	destination, err := xnet.ParseDestination(transport + ":" + address)
	if err != nil {
		return nil, fmt.Errorf("mieru: invalid server address: %w", err)
	}
	// Chained dialers can retain ctx for the entire socket lifetime. Preserve
	// this caller's routing values, but detach successful shared underlays
	// from its cancellation; they belong to the outbound client instead.
	transportCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stopClient := context.AfterFunc(a.owner.ctx, cancel)
	stopRequest := context.AfterFunc(ctx, cancel)
	conn, err := dialer.Dial(transportCtx, destination)
	stopRequest()
	release := func() {
		stopClient()
		cancel()
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		if conn != nil {
			conn.Close()
		}
		release()
		return nil, ctxErr
	}
	if err != nil {
		if conn != nil {
			conn.Close()
		}
		release()
		return nil, err
	}
	return a.owner.track(conn, release, true)
}

func (a *transportAdapter) ListenPacket(ctx context.Context, network, local, remote string) (net.PacketConn, error) {
	if network != "udp" && network != "udp4" && network != "udp6" {
		return nil, fmt.Errorf("mieru: unsupported packet network %q", network)
	}
	if local != "" {
		return nil, fmt.Errorf("mieru: explicit packet bind address is unsupported")
	}
	// The official packet underlay resolves the endpoint before this call.
	addr, err := xnet.ParseDestination("udp:" + remote)
	if err != nil || addr.Address == nil || !addr.Address.Family().IsIP() {
		return nil, fmt.Errorf("mieru: packet endpoint must be an IP address: %q", remote)
	}
	conn, err := a.DialContext(ctx, network, remote)
	if err != nil {
		return nil, err
	}
	return &connectedPacketConn{
		Conn:   conn,
		remote: &net.UDPAddr{IP: addr.Address.IP(), Port: int(addr.Port)},
	}, nil
}

// Adapting a connected Xray socket retains its protection, outbound gateway,
// statistics and proxy chaining. Read and Write each represent one datagram.
type connectedPacketConn struct {
	net.Conn
	remote *net.UDPAddr
}

func (c *connectedPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	n, err := c.Conn.Read(p)
	return n, c.remote, err
}

func (c *connectedPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	udp, ok := addr.(*net.UDPAddr)
	if !ok || !udp.IP.Equal(c.remote.IP) || udp.Port != c.remote.Port || udp.Zone != c.remote.Zone {
		return 0, fmt.Errorf("mieru: unexpected packet transport destination %v", addr)
	}
	return c.Conn.Write(p)
}

type dnsResolver struct {
	strategy internet.DomainStrategy
}

func (r *dnsResolver) LookupIP(ctx context.Context, network, host string) ([]net.IP, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if network != "ip" && network != "ip4" && network != "ip6" {
		return nil, fmt.Errorf("mieru: unsupported DNS network %q", network)
	}
	strategy := r.strategy
	if network == "ip4" {
		strategy = internet.DomainStrategy_FORCE_IP4
	} else if network == "ip6" {
		strategy = internet.DomainStrategy_FORCE_IP6
	}
	var local xnet.Address
	if outbounds := session.OutboundsFromContext(ctx); len(outbounds) > 0 {
		local = outbounds[len(outbounds)-1].Gateway
	}
	ips, err := internet.LookupForIP(host, strategy, local)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return ips, nil
}
