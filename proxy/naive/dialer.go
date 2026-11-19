//go:build with_naive_outbound && ((linux && !android && (386 || amd64 || arm || arm64 || loong64 || mipsle || riscv64 || (mips64le && !with_musl))) || (android && (386 || amd64 || arm || arm64)) || (darwin && (amd64 || arm64)) || (windows && (amd64 || arm64)))

package naive

import (
	"context"
	"fmt"
	stdnet "net"
	"net/netip"

	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/stat"
)

type rawDialer interface {
	DialRaw(context.Context, net.Destination) (stat.Connection, error)
}

type socketDialer struct {
	dialer      rawDialer
	server      net.Destination
	strategy    internet.DomainStrategy
	dialerProxy bool
	gateway     net.Address
	tag         string
}

var _ N.Dialer = (*socketDialer)(nil)

func (d *socketDialer) DialContext(ctx context.Context, network string, address M.Socksaddr) (stdnet.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var ips []net.IP
	if d.server.Address.Family().IsDomain() && (address.Port == uint16(d.server.Port) || isChromiumIPv6Probe(network, address)) {
		var err error
		source := d.gateway
		if d.dialerProxy {
			source = nil
		}
		// Resolve before endpoint pinning, using the same preferred/fallback
		// families and sendThrough constraint as Xray's normal socket path.
		// Even soft UseIP failures must not fall back to the system resolver.
		ips, err = internet.LookupForIP(d.server.Address.Domain(), d.strategy, source)
		if err != nil {
			return nil, fmt.Errorf("naive: resolve proxy endpoint through Xray DNS: %w", err)
		}
	}
	destination, err := socketDestination(network, address, d.server, ips)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Handler.DialRaw preserves sockopt, dialerProxy, sendThrough and counters,
	// while removing only Xray's TLS wrapper. Chromium owns TLS and QUIC.
	// Each physical dial gets fresh session state: concurrent pooled sockets
	// must never mutate one another's outbound target or gateway.
	outbound := &session.Outbound{
		Target: destination, OriginalTarget: d.server,
		Gateway: d.gateway, Tag: d.tag, Name: "naive",
	}
	ctx = session.ContextWithOutbounds(ctx, []*session.Outbound{outbound})
	return d.dialer.DialRaw(ctx, destination)
}

// Chromium decides whether to request and accept AAAA records by connecting a
// datagram socket to this fixed probe and inspecting the local address
// (net/dns/host_resolver_manager.cc: kIPv6ProbeAddress and
// kGloballyReachableCheckPort). The probe only connects: it never sends a
// datagram. Pin it to the configured proxy endpoint like every other socket so
// the answer reflects the proxy path and no unrelated external socket is made.
var chromiumIPv6Probe = netip.MustParseAddr("2001:4860:4860::8888")

func isChromiumIPv6Probe(network string, address M.Socksaddr) bool {
	return N.NetworkName(network) == N.NetworkUDP && address.Port == 443 && address.Addr.Unmap() == chromiumIPv6Probe
}

// Chromium can resolve local names internally, bypassing the DNS callback, and
// can request reachability-probe sockets. The configured proxy remains the only
// physical endpoint: callback addresses must not redirect this outbound. Pinning
// the IPv6 probe still influences Chromium's reachability heuristic, but avoids
// making unrelated external connections, including when the proxy uses port 443.
func socketDestination(network string, requested M.Socksaddr, server net.Destination, ips []net.IP) (net.Destination, error) {
	destination := server
	switch N.NetworkName(network) {
	case N.NetworkTCP:
		destination.Network = net.Network_TCP
	case N.NetworkUDP:
		destination.Network = net.Network_UDP
	default:
		return net.Destination{}, fmt.Errorf("naive: unsupported raw socket network %q", network)
	}
	// The probe targets Chromium's public IPv6 address, not the proxy, so its
	// port cannot match; pin it to the configured endpoint instead of rejecting
	// it. Rejecting it makes Chromium drop every AAAA result for the proxy name.
	if !isChromiumIPv6Probe(network, requested) && (!requested.IsValid() || requested.Port != uint16(server.Port)) {
		return net.Destination{}, fmt.Errorf("naive: Chromium requested a socket outside the configured proxy port")
	}
	if server.Address.Family().IsIP() {
		return destination, nil
	}
	var selected net.IP
	for _, ip := range ips {
		if ip.To16() == nil {
			continue
		}
		if selected == nil {
			selected = ip
		}
		// Preserve Chromium's address selection only when Xray DNS independently
		// confirms it belongs to the configured proxy, never the CONNECT target.
		resolved, _ := netip.AddrFromSlice(ip)
		if requested.Addr.IsValid() && resolved.Unmap() == requested.Addr.Unmap() {
			selected = ip
			break
		}
	}
	if selected == nil {
		return net.Destination{}, fmt.Errorf("naive: Xray DNS returned no real proxy addresses")
	}
	destination.Address = net.IPAddress(selected)
	return destination, nil
}

func (*socketDialer) ListenPacket(context.Context, M.Socksaddr) (stdnet.PacketConn, error) {
	return nil, fmt.Errorf("naive: Chromium requires connected UDP sockets")
}

func socksaddr(destination net.Destination) M.Socksaddr {
	address := destination.Address
	if address.Family().IsIP() {
		ip, _ := netip.AddrFromSlice(address.IP())
		return M.Socksaddr{Addr: ip.Unmap(), Port: uint16(destination.Port)}
	}
	return M.ParseSocksaddrHostPort(address.Domain(), uint16(destination.Port))
}
