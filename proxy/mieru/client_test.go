package mieru

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	mieruclient "github.com/enfein/mieru/v3/apis/client"
	"github.com/enfein/mieru/v3/pkg/appctl/appctlpb"
	"google.golang.org/protobuf/proto"

	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/transport/internet/stat"
)

func newTestClient(t *testing.T, start bool) *Client {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{
		client:      mieruclient.NewClient(),
		ctx:         ctx,
		cancel:      cancel,
		closeDone:   make(chan struct{}),
		connections: make(map[*managedConn]struct{}),
	}
	if start {
		adapter := &transportAdapter{owner: c}
		err := c.client.Store(&mieruclient.ClientConfig{
			Profile: &appctlpb.ClientProfile{
				ProfileName: proto.String("test"),
				User:        &appctlpb.User{Name: proto.String("user"), Password: proto.String("password")},
				Servers: []*appctlpb.ServerEndpoint{{
					IpAddress:    proto.String("192.0.2.1"),
					PortBindings: []*appctlpb.PortBinding{{Port: proto.Int32(443), Protocol: appctlpb.TransportProtocol_TCP.Enum()}},
				}},
				Multiplexing:  &appctlpb.MultiplexingConfig{Level: appctlpb.MultiplexingLevel_MULTIPLEXING_OFF.Enum()},
				HandshakeMode: appctlpb.HandshakeMode_HANDSHAKE_NO_WAIT.Enum(),
			},
			Dialer:       adapter,
			PacketDialer: adapter,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := c.client.Start(); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { c.Close() })
	return c
}

type callerKey struct{}

type pipeDialer struct {
	calls        chan string
	block        bool
	mu           sync.Mutex
	peers        []net.Conn
	contexts     []context.Context
	writeStarted chan struct{}
}

func (d *pipeDialer) Dial(ctx context.Context, destination xnet.Destination) (stat.Connection, error) {
	marker, _ := ctx.Value(callerKey{}).(string)
	d.calls <- marker
	if d.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	left, right := net.Pipe()
	d.mu.Lock()
	d.peers = append(d.peers, right)
	d.contexts = append(d.contexts, ctx)
	d.mu.Unlock()
	if d.writeStarted != nil {
		return &observedWriteConn{Conn: left, started: d.writeStarted}, nil
	}
	return left, nil
}

func (*pipeDialer) DestIpAddress() xnet.IP                                { return nil }
func (*pipeDialer) SetOutboundGateway(context.Context, *session.Outbound) {}

func (d *pipeDialer) close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, peer := range d.peers {
		peer.Close()
	}
}

func TestCloseCancelsPendingDial(t *testing.T) {
	c := newTestClient(t, true)
	dialer := &pipeDialer{calls: make(chan string, 1), block: true}
	done := make(chan error, 1)
	go func() {
		_, err := c.dial(context.Background(), xnet.TCPDestination(xnet.ParseAddress("192.0.2.2"), 80), dialer)
		done <- err
	}()
	select {
	case <-dialer.calls:
	case <-time.After(5 * time.Second):
		t.Fatal("dial did not reach transport")
	}
	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("pending dial succeeded after Close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pending dial outlived Close")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not wait for and stop the pending dial")
	}
}

func TestCloseOwnsResourcesAndIsConcurrent(t *testing.T) {
	c := newTestClient(t, false)
	left, right := net.Pipe()
	defer right.Close()
	if _, err := c.track(left, nil, true); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.Close(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	right.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := right.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("tracked socket remained open: %v", err)
	}
	late, peer := net.Pipe()
	defer peer.Close()
	conn, err := c.track(late, nil, true)
	if !errors.Is(err, net.ErrClosed) || conn != nil {
		t.Fatalf("late transport accepted after close: %v %v", conn, err)
	}
	peer.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := peer.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("late socket was leaked: %v", err)
	}
	c.mu.Lock()
	remaining := len(c.connections)
	c.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("Close retained %d resources", remaining)
	}
}

func TestUnderlayContextOutlivesCaller(t *testing.T) {
	c := newTestClient(t, false)
	dialer := &pipeDialer{calls: make(chan string, 1)}
	defer dialer.close()
	requestCtx, cancel := context.WithCancel(context.WithValue(context.Background(), callerKey{}, "retained"))
	requestCtx = context.WithValue(requestCtx, dialerKey{}, dialer)
	adapter := &transportAdapter{owner: c}
	conn, err := adapter.DialContext(requestCtx, "tcp", "192.0.2.1:443")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	cancel()
	dialer.mu.Lock()
	retained := dialer.contexts[0]
	dialer.mu.Unlock()
	if retained.Err() != nil || retained.Value(callerKey{}) != "retained" {
		t.Fatal("shared transport lost its lifetime or routing context")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-retained.Done():
	default:
		t.Fatal("Close did not cancel the retained transport context")
	}
}

func TestConnectedPacketRejectsWrongDestination(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	packet := &connectedPacketConn{Conn: left, remote: &net.UDPAddr{IP: net.ParseIP("192.0.2.1"), Port: 443}}
	if _, err := packet.WriteTo([]byte("packet"), &net.UDPAddr{IP: net.ParseIP("192.0.2.1"), Port: 444}); err == nil {
		t.Fatal("connected packet socket accepted a different destination")
	}
}

type observedWriteConn struct {
	net.Conn
	started chan struct{}
	once    sync.Once
}

func (c *observedWriteConn) Write(p []byte) (int, error) {
	c.once.Do(func() { close(c.started) })
	return c.Conn.Write(p)
}

func TestCloseUnblocksPhysicalWriteBeforeSession(t *testing.T) {
	c := newTestClient(t, true)
	dialer := &pipeDialer{calls: make(chan string, 1), writeStarted: make(chan struct{})}
	defer dialer.close()
	conn, err := c.dial(context.Background(), xnet.TCPDestination(xnet.ParseAddress("192.0.2.2"), 80), dialer)
	if err != nil {
		t.Fatal(err)
	}
	written := make(chan error, 1)
	go func() {
		_, err := conn.Write(make([]byte, 32000))
		written <- err
	}()
	select {
	case <-dialer.writeStarted:
	case <-time.After(5 * time.Second):
		dialer.close()
		t.Fatal("session did not start a physical transport write")
	}
	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		dialer.close()
		t.Fatal("Close waited for a session while its underlay write was blocked")
	}
	select {
	case <-written:
	case <-time.After(5 * time.Second):
		t.Fatal("session write outlived client shutdown")
	}
}
