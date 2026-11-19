//go:build with_naive_outbound && ((linux && !android && (386 || amd64 || arm || arm64 || loong64 || mipsle || riscv64 || (mips64le && !with_musl))) || (android && (386 || amd64 || arm || arm64)) || (darwin && (amd64 || arm64)) || (windows && (amd64 || arm64)))

package naive

import (
	"context"
	"strings"

	mdns "github.com/miekg/dns"
	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/features/dns"
)

// resolveDNS is Chromium's only resolver. Only proxy-server names reach this
// bridge; CONNECT authorities are passed unchanged to the remote Naive server.
// Xray's DNS feature exposes A/AAAA, not arbitrary wire-message exchange. Other
// query types receive valid NOERROR/NODATA instead of escaping to system DNS.
// In particular ECH is rejected at configuration time, rather than pretending
// this bridge can obtain HTTPS/SVCB records.
func (c *Client) resolveDNS(ctx context.Context, request *mdns.Msg) *mdns.Msg {
	response := new(mdns.Msg)
	response.SetReply(request)
	response.RecursionAvailable = true
	if ctx.Err() != nil {
		response.Rcode = mdns.RcodeServerFailure
		return response
	}
	if request.Opcode != mdns.OpcodeQuery {
		response.Rcode = mdns.RcodeNotImplemented
		return response
	}
	if len(request.Question) != 1 {
		response.Rcode = mdns.RcodeFormatError
		return response
	}
	question := request.Question[0]
	if question.Qclass != mdns.ClassINET {
		response.Rcode = mdns.RcodeRefused
		return response
	}
	if question.Qtype != mdns.TypeA && question.Qtype != mdns.TypeAAAA {
		return response
	}
	ips, ttl, err := c.dns.LookupIP(strings.TrimSuffix(question.Name, "."), dns.IPOption{
		IPv4Enable: question.Qtype == mdns.TypeA,
		IPv6Enable: question.Qtype == mdns.TypeAAAA,
		FakeEnable: false,
	})
	if err != nil {
		if code := dns.RCodeFromError(err); code != 0 {
			response.Rcode = int(code)
		} else if errors.Cause(err) != dns.ErrEmptyResponse {
			response.Rcode = mdns.RcodeServerFailure
		}
		return response
	}
	for _, ip := range ips {
		header := mdns.RR_Header{Name: question.Name, Rrtype: question.Qtype, Class: mdns.ClassINET, Ttl: ttl}
		if question.Qtype == mdns.TypeA {
			if ipv4 := ip.To4(); ipv4 != nil {
				response.Answer = append(response.Answer, &mdns.A{Hdr: header, A: ipv4})
			}
		} else if ip.To4() == nil && ip.To16() != nil {
			response.Answer = append(response.Answer, &mdns.AAAA{Hdr: header, AAAA: ip.To16()})
		}
	}
	return response
}
