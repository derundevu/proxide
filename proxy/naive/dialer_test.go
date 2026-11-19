//go:build with_naive_outbound && ((linux && !android && (386 || amd64 || arm || arm64 || loong64 || mipsle || riscv64 || (mips64le && !with_musl))) || (android && (386 || amd64 || arm || arm64)) || (darwin && (amd64 || arm64)) || (windows && (amd64 || arm64)))

package naive

import (
	"testing"

	M "github.com/sagernet/sing/common/metadata"
	"github.com/xtls/xray-core/common/net"
)

func TestSocketDestinationPinsConfiguredIPv4(t *testing.T) {
	server := net.TCPDestination(net.LocalHostIP, 443)
	for _, network := range []string{"tcp", "udp"} {
		for _, host := range []string{"::1", "2001:db8::42"} {
			destination, err := socketDestination(network, M.ParseSocksaddrHostPort(host, 443), server, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !destination.Address.IP().Equal(server.Address.IP()) || destination.Port != server.Port || destination.Network.SystemString() != network {
				t.Fatalf("%s requested %s: escaped configured endpoint: %v", network, host, destination)
			}
		}
	}
	if _, err := socketDestination("udp", M.ParseSocksaddrHostPort("2001:db8::42", 8443), server, nil); err == nil {
		t.Fatal("accepted a socket outside the configured proxy port")
	}
}

func TestSocketDestinationPinsChromiumIPv6Probe(t *testing.T) {
	// Chromium probes IPv6 by connecting UDP to Google's address on port 443 and
	// inspecting the local address. It must be pinned to the proxy, not rejected.
	probe := M.ParseSocksaddrHostPort("2001:4860:4860::8888", 443)
	server := net.TCPDestination(net.ParseAddress("192.0.2.1"), 8443)
	destination, err := socketDestination("udp", probe, server, nil)
	if err != nil {
		t.Fatal(err)
	}
	if destination.Network != net.Network_UDP || destination.Port != server.Port || !destination.Address.IP().Equal(server.Address.IP()) {
		t.Fatalf("probe escaped the configured endpoint: %v", destination)
	}

	// A domain proxy endpoint is resolved by Xray DNS before pinning.
	domainServer := net.TCPDestination(net.DomainAddress("proxy.invalid"), 8443)
	ips := []net.IP{net.ParseIP("192.0.2.10")}
	destination, err = socketDestination("udp", probe, domainServer, ips)
	if err != nil {
		t.Fatal(err)
	}
	if !destination.Address.IP().Equal(ips[0]) || destination.Port != domainServer.Port || destination.Network != net.Network_UDP {
		t.Fatalf("probe did not pin to resolved endpoint: %v", destination)
	}
	if _, err := socketDestination("udp", probe, domainServer, nil); err == nil {
		t.Fatal("allowed an unresolved domain endpoint to escape to the system dialer")
	}

	// Only the exact datagram probe is exempt; other outside sockets stay rejected.
	if _, err := socketDestination("udp", M.ParseSocksaddrHostPort("2001:4860:4860::8888", 444), server, nil); err == nil {
		t.Fatal("accepted a socket outside the configured proxy port")
	}
	if _, err := socketDestination("tcp", probe, server, nil); err == nil {
		t.Fatal("accepted the datagram probe over TCP")
	}
}

func TestSocketDestinationDomainUsesOnlyXrayAnswers(t *testing.T) {
	server := net.TCPDestination(net.DomainAddress("proxy.invalid"), 443)
	ips := []net.IP{net.ParseIP("192.0.2.10"), net.ParseIP("2001:db8::10")}
	for _, test := range []struct{ requested, expected string }{
		{"2001:db8::10", "2001:db8::10"},
		{"::1", "192.0.2.10"},
	} {
		destination, err := socketDestination("udp", M.ParseSocksaddrHostPort(test.requested, 443), server, ips)
		if err != nil {
			t.Fatal(err)
		}
		if !destination.Address.IP().Equal(net.ParseIP(test.expected)) {
			t.Fatalf("requested %s: expected %s, got %v", test.requested, test.expected, destination)
		}
	}
	// A native IPv6 choice must not escape a prior IPv4-only DNS policy.
	destination, err := socketDestination("udp", M.ParseSocksaddrHostPort("2001:db8::10", 443), server, ips[:1])
	if err != nil {
		t.Fatal(err)
	}
	if !destination.Address.IP().Equal(ips[0]) {
		t.Fatal("native address overrode the DNS policy's IPv4-only candidates")
	}
	if _, err := socketDestination("udp", M.ParseSocksaddrHostPort("::1", 443), server, nil); err == nil {
		t.Fatal("allowed an unresolved domain to escape to the system dialer")
	}
}
