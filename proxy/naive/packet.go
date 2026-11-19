//go:build with_naive_outbound && ((linux && !android && (386 || amd64 || arm || arm64 || loong64 || mipsle || riscv64 || (mips64le && !with_musl))) || (android && (386 || amd64 || arm || arm64)) || (darwin && (amd64 || arm64)) || (windows && (amd64 || arm64)))

package naive

import (
	"fmt"

	sbuf "github.com/sagernet/sing/common/buf"
	"github.com/sagernet/sing/common/uot"

	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/net"
)

// A MultiBuffer is a batch of datagrams, never one coalesced byte stream.
// UoT association mode preserves per-packet destination/source metadata too.
type packetWriter struct {
	connection  *uot.Conn
	destination net.Destination
}

func (w *packetWriter) WriteMultiBuffer(buffers buf.MultiBuffer) error {
	defer buf.ReleaseMulti(buffers)
	for _, buffer := range buffers {
		if buffer.Len() > 65535 {
			return fmt.Errorf("naive: UDP payload exceeds UoT's 65535-byte limit")
		}
		destination := w.destination
		if buffer.UDP != nil {
			destination = *buffer.UDP
		}
		if err := w.connection.WritePacket(sbuf.As(buffer.Bytes()), socksaddr(destination)); err != nil {
			return err
		}
	}
	return nil
}

type packetReader struct{ connection *uot.Conn }

func (r *packetReader) ReadMultiBuffer() (buf.MultiBuffer, error) {
	// UoT lengths are uint16. Reading into a standard 8K Xray buffer would
	// truncate large datagrams and leave the framing stream out of sync.
	buffer := buf.NewWithSize(65535)
	view := sbuf.With(buffer.Extend(65535))
	source, err := r.connection.ReadPacket(view)
	if err != nil {
		buffer.Release()
		return nil, err
	}
	buffer.Resize(0, int32(view.Len()))
	destination := net.UDPDestination(net.ParseAddress(source.AddrString()), net.Port(source.Port))
	buffer.UDP = &destination
	return buf.MultiBuffer{buffer}, nil
}
