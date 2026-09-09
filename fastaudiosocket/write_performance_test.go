package fastaudiosocket

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"testing"
)

type captureWriteConn struct {
	net.Conn
	wire bytes.Buffer
}

func (c *captureWriteConn) Write(b []byte) (int, error) { return c.wire.Write(b) }

func TestSendPacketReusesStorageWithoutChangingWireOrInput(t *testing.T) {
	c := &captureWriteConn{}
	s := &FastAudioSocket{conn: c}
	var expected []byte
	for i, size := range []int{320, 1, 0, 640, 320} {
		payload := bytes.Repeat([]byte{byte(i + 1)}, size)
		original := append([]byte(nil), payload...)
		packet := PacketWriter{Payload: payload}
		packet.Header[0] = PacketTypeAudio
		binary.BigEndian.PutUint16(packet.Header[1:], uint16(size))
		if err := s.sendPacket(packet); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(payload, original) {
			t.Fatal("send modified caller data")
		}
		expected = append(expected, frame(PacketTypeAudio, original)...)
	}
	if !bytes.Equal(c.wire.Bytes(), expected) {
		t.Fatal("frame reuse changed framing, length, or payload")
	}
}

type discardWriteConn struct {
	net.Conn
	bytes uint64
}

func (c *discardWriteConn) Write(b []byte) (int, error) {
	c.bytes += uint64(len(b))
	return len(b), nil
}

// Isolate serialization from socket scheduling and audio pacing. Large frames
// exercise the fallback for internal callers outside the ordinary 320-byte path.
func BenchmarkSendPacket(b *testing.B) {
	for _, size := range []int{320, 640} {
		b.Run(fmt.Sprintf("bytes_%d", size), func(b *testing.B) {
			c := &discardWriteConn{}
			s := &FastAudioSocket{conn: c}
			packet := PacketWriter{Payload: make([]byte, size)}
			packet.Header[0] = PacketTypeAudio
			binary.BigEndian.PutUint16(packet.Header[1:], uint16(size))
			b.ReportAllocs()
			b.SetBytes(int64(size))
			for b.Loop() {
				if err := s.sendPacket(packet); err != nil {
					b.Fatal(err)
				}
			}
			if c.bytes != uint64(b.N*(HeaderSize+size)) {
				b.Fatal("incorrect write count")
			}
		})
	}
}
