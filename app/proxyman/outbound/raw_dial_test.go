package outbound

import (
	"context"
	cryptotls "crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"testing"
	"time"

	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol/tls/cert"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/tcp"
	"github.com/xtls/xray-core/transport/internet/tls"
)

func TestDialRawDoesNotDisableTLSForSubsequentDial(t *testing.T) {
	certificate, err := cert.Generate(nil, cert.Authority(true), cert.DNSNames("localhost"), cert.KeyUsage(x509.KeyUsageCertSign|x509.KeyUsageDigitalSignature))
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM := certificate.ToPEM()
	identity, err := cryptotls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	deadline := time.Now().Add(10 * time.Second)
	listener.SetDeadline(deadline)
	serverResult := make(chan error, 1)
	go func() {
		for _, secured := range []bool{false, true} {
			conn, err := listener.Accept()
			if err != nil {
				serverResult <- err
				return
			}
			conn.SetDeadline(deadline)
			if secured {
				conn = cryptotls.Server(conn, &cryptotls.Config{Certificates: []cryptotls.Certificate{identity}})
			}
			var request [1]byte
			_, err = io.ReadFull(conn, request[:])
			if err == nil {
				_, err = conn.Write(request[:])
			}
			conn.Close()
			if err != nil {
				serverResult <- err
				return
			}
		}
		serverResult <- nil
	}()
	handler := &Handler{streamSettings: &internet.MemoryStreamConfig{
		ProtocolName:     "tcp",
		ProtocolSettings: &tcp.Config{},
		SecurityType:     "xray.transport.internet.tls.Config",
		SecuritySettings: &tls.Config{ServerName: "localhost", Certificate: []*tls.Certificate{{Certificate: certPEM, Usage: tls.Certificate_AUTHORITY_VERIFY}}},
	}}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	destination := xnet.TCPDestination(xnet.LocalHostIP, xnet.Port(listener.Addr().(*net.TCPAddr).Port))
	for _, dial := range []func(context.Context, xnet.Destination) (net.Conn, error){
		func(ctx context.Context, dest xnet.Destination) (net.Conn, error) { return handler.DialRaw(ctx, dest) },
		func(ctx context.Context, dest xnet.Destination) (net.Conn, error) { return handler.Dial(ctx, dest) },
	} {
		conn, err := dial(ctx, destination)
		if err != nil {
			t.Fatal(err)
		}
		conn.SetDeadline(deadline)
		_, err = conn.Write([]byte("x"))
		if err == nil {
			var response [1]byte
			_, err = io.ReadFull(conn, response[:])
			if err == nil && response[0] != 'x' {
				t.Errorf("unexpected response: %q", response)
			}
		}
		conn.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := <-serverResult; err != nil {
		t.Fatal(err)
	}
}
