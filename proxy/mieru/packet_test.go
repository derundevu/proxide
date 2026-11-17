package mieru

import (
	"bytes"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	apicommon "github.com/enfein/mieru/v3/apis/common"
	"github.com/enfein/mieru/v3/apis/model"

	"github.com/xtls/xray-core/common/buf"
	xnet "github.com/xtls/xray-core/common/net"
)

func TestPacketBoundariesAndMetadata(t *testing.T) {
	for _, host := range []string{"192.0.2.1", "2001:db8::1"} {
		t.Run(host, func(t *testing.T) {
			left, right := net.Pipe()
			defer left.Close()
			defer right.Close()
			left.SetDeadline(time.Now().Add(5 * time.Second))
			right.SetDeadline(time.Now().Add(5 * time.Second))
			destination := xnet.UDPDestination(xnet.ParseAddress(host), 443)
			maximum := 65525
			if destination.Address.Family().IsIPv6() {
				maximum = 65513
			}
			sizes := []int{0, 1, 1300, 7000, 20000, maximum}
			mb := make(buf.MultiBuffer, 0, len(sizes))
			for i, size := range sizes {
				b := buf.NewWithSize(int32(size + 1))
				b.Write(bytes.Repeat([]byte{byte(i + 1)}, size))
				addr := destination
				addr.Port += xnet.Port(i)
				b.UDP = &addr
				mb = append(mb, b)
			}
			buffers := append(buf.MultiBuffer(nil), mb...)
			writer := packetWriter{conn: newPacketConn(left), destination: destination}
			done := make(chan error, 1)
			go func() { done <- writer.WriteMultiBuffer(mb) }()
			reader := packetReader{conn: newPacketConn(right)}
			for i, size := range sizes {
				packet, err := reader.ReadMultiBuffer()
				if err != nil {
					t.Fatal(err)
				}
				if len(packet) != 1 || !bytes.Equal(packet[0].Bytes(), bytes.Repeat([]byte{byte(i + 1)}, size)) {
					buf.ReleaseMulti(packet)
					t.Fatalf("datagram %d lost its boundary or content", i)
				}
				if packet[0].UDP == nil || packet[0].UDP.NetAddr() != net.JoinHostPort(host, strconv.Itoa(443+i)) {
					buf.ReleaseMulti(packet)
					t.Fatalf("datagram %d lost its destination", i)
				}
				buf.ReleaseMulti(packet)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			for _, b := range buffers {
				if b.Len() != 0 || b.UDP != nil {
					t.Fatal("writer did not release input buffer")
				}
			}
		})
	}
}

func TestPacketDomainDestination(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	left.SetDeadline(time.Now().Add(5 * time.Second))
	right.SetDeadline(time.Now().Add(5 * time.Second))
	destination := xnet.UDPDestination(xnet.DomainAddress("server.example"), 53)
	b := buf.New()
	b.WriteString("question")
	writer := packetWriter{conn: newPacketConn(left), destination: destination}
	done := make(chan error, 1)
	go func() { done <- writer.WriteMultiBuffer(buf.MultiBuffer{b}) }()
	frame := make([]byte, maxPacketSize)
	n, err := apicommon.NewPacketOverStreamTunnel(right).Read(frame)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(frame[:3], []byte{0, 0, 0}) {
		t.Fatal("invalid SOCKS UDP header")
	}
	r := bytes.NewReader(frame[3:n])
	var address model.AddrSpec
	if err := address.ReadFromSocks5(r); err != nil {
		t.Fatal(err)
	}
	payload, _ := io.ReadAll(r)
	if address.FQDN != "server.example" || address.Port != 53 || string(payload) != "question" {
		t.Fatalf("wrong domain datagram: %v %q", address, payload)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestPacketOversizeReleasesEntireBatch(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	left.SetDeadline(time.Now().Add(time.Second))
	oversize := buf.NewWithSize(65526)
	oversize.Extend(65526)
	trailing := buf.New()
	trailing.WriteString("not sent")
	writer := packetWriter{
		conn:        newPacketConn(left),
		destination: xnet.UDPDestination(xnet.ParseAddress("192.0.2.1"), 53),
	}
	err := writer.WriteMultiBuffer(buf.MultiBuffer{oversize, trailing})
	if err == nil || !strings.Contains(err.Error(), "frame limit") {
		t.Fatalf("expected explicit frame limit error, got %v", err)
	}
	if oversize.Len() != 0 || trailing.Len() != 0 {
		t.Fatal("error path retained input buffers")
	}
}

func TestPacketReaderStreamEOF(t *testing.T) {
	left, right := net.Pipe()
	left.Close()
	defer right.Close()
	reader := packetReader{conn: newPacketConn(right)}
	mb, err := reader.ReadMultiBuffer()
	if err != io.EOF || mb != nil {
		t.Fatalf("stream EOF became an empty datagram: %v %v", mb, err)
	}
}

type countingActivity struct {
	count int
}

func (a *countingActivity) Update() { a.count++ }

func TestCopyDatagramsPreservesEmptyPayload(t *testing.T) {
	source, input := net.Pipe()
	output, sink := net.Pipe()
	for _, conn := range []net.Conn{source, input, output, sink} {
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
	}
	destination := xnet.UDPDestination(xnet.ParseAddress("192.0.2.1"), 443)
	activity := new(countingActivity)
	copied := make(chan error, 1)
	go func() {
		copied <- copyDatagrams(
			&packetReader{conn: newPacketConn(input)},
			&packetWriter{conn: newPacketConn(output), destination: destination},
			activity,
		)
	}()
	sent := make(chan error, 1)
	go func() {
		defer source.Close()
		writer := packetWriter{conn: newPacketConn(source), destination: destination}
		empty := buf.New()
		payload := buf.New()
		payload.WriteString("hello")
		sent <- writer.WriteMultiBuffer(buf.MultiBuffer{empty, payload})
	}()
	reader := packetReader{conn: newPacketConn(sink)}
	for _, expected := range []string{"", "hello"} {
		mb, err := reader.ReadMultiBuffer()
		if err != nil {
			t.Fatal(err)
		}
		if len(mb) != 1 || string(mb[0].Bytes()) != expected {
			buf.ReleaseMulti(mb)
			t.Fatalf("copy changed datagram %q", expected)
		}
		buf.ReleaseMulti(mb)
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	if err := <-copied; err != nil {
		t.Fatal(err)
	}
	if activity.count != 2 {
		t.Fatalf("empty datagram did not update activity: %d", activity.count)
	}
}
