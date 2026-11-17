package mieru

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	mieruclient "github.com/enfein/mieru/v3/apis/client"
	apicommon "github.com/enfein/mieru/v3/apis/common"
	"github.com/enfein/mieru/v3/apis/model"
	"github.com/enfein/mieru/v3/pkg/appctl/appctlpb"
	"google.golang.org/protobuf/proto"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/common/signal"
	"github.com/xtls/xray-core/common/task"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/policy"
	"github.com/xtls/xray-core/transport"
	"github.com/xtls/xray-core/transport/internet"
)

// Client uses Mieru's native multiplexer. Transport connections and sessions are
// owned here as well: stopping the upstream client alone does not close them.
type Client struct {
	client        mieruclient.Client
	policyManager policy.Manager
	ctx           context.Context
	cancel        context.CancelFunc

	mu          sync.Mutex
	closed      bool
	closeDone   chan struct{}
	closeErr    error
	connections map[*managedConn]struct{}
	dials       sync.WaitGroup
}

// NewClient validates and starts an official native Mieru outbound client.
func NewClient(ctx context.Context, config *ClientConfig) (*Client, error) {
	if config == nil || len(config.Profile) == 0 {
		return nil, fmt.Errorf("mieru: missing client profile")
	}
	profile := new(appctlpb.ClientProfile)
	if err := proto.Unmarshal(config.Profile, profile); err != nil {
		return nil, fmt.Errorf("mieru: invalid client profile: %w", err)
	}
	if _, ok := appctlpb.HandshakeMode_name[int32(profile.GetHandshakeMode())]; !ok {
		return nil, fmt.Errorf("mieru: unsupported handshake mode %d", profile.GetHandshakeMode())
	}
	if _, ok := appctlpb.MultiplexingLevel_name[int32(profile.GetMultiplexing().GetLevel())]; !ok {
		return nil, fmt.Errorf("mieru: unsupported multiplexing level %d", profile.GetMultiplexing().GetLevel())
	}
	v := core.MustFromContext(ctx)
	c := &Client{
		client:        mieruclient.NewClient(),
		policyManager: v.GetFeature(policy.ManagerType()).(policy.Manager),
		closeDone:     make(chan struct{}),
		connections:   make(map[*managedConn]struct{}),
	}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	adapter := &transportAdapter{owner: c}
	resolver := &dnsResolver{strategy: internet.DomainStrategy_USE_IP}
	dnsConfig := &apicommon.ClientDNSConfig{}
	if stream, ok := session.StreamSettingsFromContext(ctx).(*internet.MemoryStreamConfig); ok && stream.SocketSettings != nil {
		// AsIs proxy chains may resolve server names remotely.
		dnsConfig.BypassDialerDNS = len(stream.SocketSettings.DialerProxy) > 0
		if strategy := stream.SocketSettings.DomainStrategy; strategy.HasStrategy() {
			resolver.strategy = strategy
			// Let DialSystem retain its TCP family policy and Happy Eyeballs.
			dnsConfig.BypassDialerDNS = true
		}
	}
	if err := c.client.Store(&mieruclient.ClientConfig{
		Profile:      profile,
		Dialer:       adapter,
		PacketDialer: adapter,
		Resolver:     resolver,
		DNSConfig:    dnsConfig,
	}); err != nil {
		c.cancel()
		return nil, fmt.Errorf("mieru: invalid profile: %w", err)
	}
	if err := c.client.Start(); err != nil {
		c.cancel()
		return nil, fmt.Errorf("mieru: start client: %w", err)
	}
	return c, nil
}

func (c *Client) track(conn net.Conn, release func(), physical bool) (net.Conn, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		if release != nil {
			release()
		}
		conn.SetDeadline(time.Now())
		conn.Close()
		return nil, net.ErrClosed
	}
	managed := &managedConn{Conn: conn, owner: c, release: release, physical: physical}
	c.connections[managed] = struct{}{}
	c.mu.Unlock()
	return managed, nil
}

// Close cancels pending opens, closes all physical transports and sessions,
// then stops the mux after no DialContext call can publish another underlay.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		<-c.closeDone
		return c.closeErr
	}
	c.closed = true
	c.cancel()
	connections := make([]*managedConn, 0, len(c.connections))
	for conn := range c.connections {
		connections = append(connections, conn)
	}
	c.mu.Unlock()
	// A session's Close can wait for an underlay write lock. Closing sockets
	// first releases blocked underlay I/O before any logical session is closed.
	for _, conn := range connections {
		if conn.physical {
			conn.Close()
		}
	}
	for _, conn := range connections {
		if !conn.physical {
			conn.Close()
		}
	}
	c.dials.Wait()
	c.closeErr = c.client.Stop()
	close(c.closeDone)
	return c.closeErr
}

type managedConn struct {
	net.Conn
	owner            *Client
	physical         bool
	release          func()
	once             sync.Once
	err              error
	handshakeTimeout time.Duration
	handshakeCancel  context.CancelFunc
}

func (c *managedConn) Close() error {
	c.once.Do(func() {
		if c.release != nil {
			c.release()
		}
		// In no-wait mode Close waits for a concurrent first Write. A deadline
		// must unblock that write before entering the upstream Close method.
		c.Conn.SetDeadline(time.Now())
		c.err = c.Conn.Close()
		c.owner.mu.Lock()
		delete(c.owner.connections, c)
		c.owner.mu.Unlock()
	})
	return c.err
}

func (c *managedConn) NeedHandshake() bool {
	early, ok := c.Conn.(interface{ NeedHandshake() bool })
	return ok && early.NeedHandshake()
}

func (c *managedConn) Write(p []byte) (int, error) {
	if c.handshakeTimeout > 0 && c.NeedHandshake() {
		timer := time.AfterFunc(c.handshakeTimeout, c.handshakeCancel)
		defer timer.Stop()
	}
	return c.Conn.Write(p)
}

type dialResult struct {
	conn net.Conn
	err  error
}

func (c *Client) dial(ctx context.Context, target xnet.Destination, dialer internet.Dialer) (net.Conn, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, net.ErrClosed
	}
	c.dials.Add(1)
	c.mu.Unlock()

	parentCtx := ctx
	ctx, cancel := context.WithCancel(context.WithValue(ctx, dialerKey{}, dialer))
	stop := context.AfterFunc(c.ctx, cancel)
	result := make(chan dialResult)
	go func() {
		defer c.dials.Done()
		defer cancel()
		defer stop()
		// The official blocking handshake has its own ten-second timeout and
		// does not observe ctx. Return cancellation promptly, and reclaim any
		// late result here rather than leaking its session.
		conn, err := c.client.DialContext(ctx, destinationAddr(target))
		if err != nil && conn != nil {
			conn.Close()
			conn = nil
		}
		if err == nil {
			conn, err = c.track(conn, nil, false)
		}
		select {
		case result <- dialResult{conn: conn, err: err}:
		case <-ctx.Done():
			if conn != nil {
				conn.Close()
			}
		}
	}()
	select {
	case result := <-result:
		if err := parentCtx.Err(); err != nil {
			if result.conn != nil {
				result.conn.Close()
			}
			return nil, err
		}
		return result.conn, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func destinationAddr(destination xnet.Destination) model.NetAddrSpec {
	addr := model.NetAddrSpec{Net: destination.Network.SystemString()}
	addr.Port = int(destination.Port)
	if destination.Address.Family().IsDomain() {
		addr.FQDN = destination.Address.Domain()
	} else {
		addr.IP = destination.Address.IP()
	}
	return addr
}

// Process relays a TCP stream or destination-addressed UDP datagrams.
func (c *Client) Process(ctx context.Context, link *transport.Link, dialer internet.Dialer) error {
	outbounds := session.OutboundsFromContext(ctx)
	if len(outbounds) == 0 || !outbounds[len(outbounds)-1].Target.IsValid() {
		return fmt.Errorf("mieru: target not specified")
	}
	ob := outbounds[len(outbounds)-1]
	target := ob.Target
	if target.Network != xnet.Network_TCP && target.Network != xnet.Network_UDP {
		return fmt.Errorf("mieru: unsupported destination network %s", target.Network)
	}
	if target.Address.Family().IsDomain() && target.Address.Domain() == "v1.mux.cool" {
		return fmt.Errorf("mieru: Xray mux is unsupported; use native Mieru multiplexing")
	}
	ob.Name = "mieru"
	ob.CanSpliceCopy.Store(3)
	sessionPolicy := c.policyManager.ForLevel(0)
	dialCtx := ctx
	if sessionPolicy.Timeouts.Handshake > 0 {
		var cancel context.CancelFunc
		dialCtx, cancel = context.WithTimeout(ctx, sessionPolicy.Timeouts.Handshake)
		defer cancel()
	}
	conn, err := c.dial(dialCtx, target, dialer)
	if err != nil {
		return fmt.Errorf("mieru: open destination: %w", err)
	}
	defer conn.Close()

	if session.TimeoutOnlyFromContext(ctx) {
		ctx = session.ConnectionObservationDetachedContext(ctx)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(c.ctx, cancel)
	defer stop()
	timer := signal.CancelAfterInactivity(ctx, cancel, sessionPolicy.Timeouts.ConnectionIdle)
	defer timer.SetTimeout(0)
	defer common.Interrupt(link.Reader)
	managed := conn.(*managedConn)
	managed.handshakeTimeout = sessionPolicy.Timeouts.Handshake
	managed.handshakeCancel = cancel

	var reader buf.Reader = buf.NewReader(conn)
	var writer buf.Writer = buf.NewWriter(conn)
	if target.Network == xnet.Network_UDP {
		packet := newPacketConn(conn)
		reader = &packetReader{conn: packet}
		writer = &packetWriter{conn: packet, destination: target}
	}
	requestDone := func() error {
		defer timer.SetTimeout(sessionPolicy.Timeouts.DownlinkOnly)
		// Mieru sessions do not expose CloseWrite. Keep the response direction
		// alive after EOF, bounded by the policy's downlink-only timeout.
		if target.Network == xnet.Network_UDP {
			return copyDatagrams(link.Reader, writer, timer)
		}
		if managed.NeedHandshake() {
			// Coalesce the first payload when available, but still open a
			// server-first destination when the local peer has nothing to send.
			err := buf.CopyOnceTimeout(link.Reader, writer, 100*time.Millisecond)
			if err != nil && err != buf.ErrNotTimeoutReader && err != buf.ErrReadTimeout && err != io.EOF {
				return err
			}
			if managed.NeedHandshake() {
				if _, writeErr := conn.Write(nil); writeErr != nil {
					return writeErr
				}
			}
			if err == io.EOF {
				return nil
			}
		}
		return buf.Copy(link.Reader, writer, buf.UpdateActivity(timer))
	}
	responseDone := func() error {
		defer timer.SetTimeout(sessionPolicy.Timeouts.UplinkOnly)
		if target.Network == xnet.Network_UDP {
			return copyDatagrams(reader, link.Writer, timer)
		}
		return buf.Copy(reader, link.Writer, buf.UpdateActivity(timer))
	}
	if err := task.Run(ctx, requestDone, task.OnSuccess(responseDone, task.Close(link.Writer))); err != nil {
		common.Interrupt(link.Writer)
		return fmt.Errorf("mieru: connection ends: %w", err)
	}
	return nil
}

func init() {
	common.Must(common.RegisterConfig((*ClientConfig)(nil), func(ctx context.Context, config interface{}) (interface{}, error) {
		return NewClient(ctx, config.(*ClientConfig))
	}))
}
