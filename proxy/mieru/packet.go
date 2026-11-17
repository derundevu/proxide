package mieru

import (
	"errors"
	"fmt"
	"io"
	"net"

	apicommon "github.com/enfein/mieru/v3/apis/common"

	"github.com/xtls/xray-core/common/buf"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/signal"
)

const maxPacketSize = 65535

// The association wrapper replaces this address with the decoded destination.
// Clearing the transport address distinguishes a valid empty UDP payload (the
// upstream wrapper returns io.EOF for it) from EOF in the underlying stream.
type packetStream struct {
	*apicommon.PacketOverStreamTunnel
}

func (s *packetStream) ReadFrom(p []byte) (int, net.Addr, error) {
	n, err := s.PacketOverStreamTunnel.Read(p)
	return n, nil, err
}

func newPacketConn(conn net.Conn) net.PacketConn {
	return apicommon.NewUDPAssociateWrapper(&packetStream{apicommon.NewPacketOverStreamTunnel(conn)})
}

type packetWriter struct {
	conn        net.PacketConn
	destination xnet.Destination
}

func (w *packetWriter) WriteMultiBuffer(mb buf.MultiBuffer) error {
	defer buf.ReleaseMulti(mb)
	for _, b := range mb {
		if b == nil {
			continue
		}
		destination := w.destination
		if b.UDP != nil {
			destination = *b.UDP
		}
		if destination.Network != xnet.Network_UDP || destination.Address == nil || destination.Port == 0 {
			return fmt.Errorf("mieru: invalid UDP destination")
		}
		headerSize := 10
		if destination.Address.Family().IsDomain() {
			length := len(destination.Address.Domain())
			if length == 0 || length > 255 {
				return fmt.Errorf("mieru: UDP destination domain length must be 1..255")
			}
			headerSize = 7 + length
		} else if destination.Address.Family().IsIPv6() {
			headerSize = 22
		}
		if int(b.Len())+headerSize > maxPacketSize {
			return fmt.Errorf("mieru: UDP datagram and destination exceed the 65535-byte frame limit")
		}
		n, err := w.conn.WriteTo(b.Bytes(), destinationAddr(destination))
		if err != nil {
			return err
		}
		if n != int(b.Len()) {
			return io.ErrShortWrite
		}
	}
	return nil
}

type packetReader struct {
	conn net.PacketConn
}

func (r *packetReader) ReadMultiBuffer() (buf.MultiBuffer, error) {
	// Allocate for the entire Mieru frame rather than buf.Size; splitting an
	// oversized datagram into buffers would change its packet boundaries.
	b := buf.NewWithSize(maxPacketSize)
	n, addr, err := r.conn.ReadFrom(b.Extend(maxPacketSize))
	if err == io.EOF && n == 0 && addr != nil {
		err = nil
	}
	if err != nil {
		b.Release()
		return nil, err
	}
	udp, ok := addr.(*net.UDPAddr)
	if !ok || udp.IP == nil || udp.Port < 1 || udp.Port > 65535 {
		b.Release()
		return nil, fmt.Errorf("mieru: invalid UDP response address %v", addr)
	}
	b.Resize(0, int32(n))
	destination := xnet.UDPDestination(xnet.IPAddress(udp.IP), xnet.Port(udp.Port))
	b.UDP = &destination
	return buf.MultiBuffer{b}, nil
}

// buf.Copy discards zero-length datagrams because it treats them as an empty
// stream buffer. A datagram is present whenever the batch contains a buffer.
func copyDatagrams(reader buf.Reader, writer buf.Writer, timer signal.ActivityUpdater) error {
	for {
		mb, err := reader.ReadMultiBuffer()
		if len(mb) > 0 {
			timer.Update()
			if writeErr := writer.WriteMultiBuffer(mb); writeErr != nil {
				return writeErr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}
