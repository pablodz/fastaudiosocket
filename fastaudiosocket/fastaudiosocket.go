package fastaudiosocket

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// Constants for packet types and audio chunk sizes.
const (
	PacketTypeTerminate = 0x00
	PacketTypeUUID      = 0x01
	PacketTypePCM       = 0x10
	PacketTypeError     = 0xff
	AudioChunkSize      = 320 // PCM audio chunk size (20ms of audio)
	HeaderSize          = 3   // Header: Type (1 byte) + Length (2 bytes)
	MaxPacketSize       = 323
)

var (
	// Reusable empty packet data.
	emptyAudioPacketData = make([]byte, AudioChunkSize)

	// Packet pool to reduce allocations.
	packetPool = sync.Pool{
		New: func() interface{} {
			return make([]byte, MaxPacketSize)
		},
	}

	writingHeader = [3]byte{PacketTypePCM, 0x01, 0x40}
)

// Packet represents a network packet.
type Packet struct {
	Header [HeaderSize]byte // Precomputed header (Type + Length).
	Data   []byte           // Audio data.
}

// newPCM8khzPacket creates a pre-populated PCM packet.
func newPCM8khzPacket(chunk []byte) Packet {
	return Packet{Header: writingHeader, Data: chunk}
}

// toBytes serializes the packet using a reusable buffer.
func (p *Packet) toBytes(buf []byte) []byte {
	copy(buf[:HeaderSize], p.Header[:])
	copy(buf[HeaderSize:], p.Data)
	return buf[:MaxPacketSize]
}

// FastAudioSocket represents an audio socket connection.
type FastAudioSocket struct {
	conn net.Conn
	uuid [16]byte // UUID from the first packet.
}

// NewFastAudioSocket initializes a new FastAudioSocket.
func NewFastAudioSocket(conn net.Conn) (*FastAudioSocket, error) {
	s := &FastAudioSocket{conn: conn}

	packet, err := s.ReadPacket()
	if err != nil {
		return nil, fmt.Errorf("failed to read first packet: %w", err)
	}
	if len(packet.Data) != 16 {
		return nil, fmt.Errorf("invalid UUID packet")
	}
	copy(s.uuid[:], packet.Data)
	return s, nil
}

// StreamPCM8khz streams PCM audio efficiently using a pool and buffered channels.
func (s *FastAudioSocket) StreamPCM8khz(audioData []byte, debug bool) error {
	if len(audioData) == 0 {
		return fmt.Errorf("no audio data to stream")
	}

	packetChan := make(chan Packet, 100) // Buffered channel.
	var wg sync.WaitGroup

	// Packet sender goroutine with a fixed 20ms interval.
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()

		buf := packetPool.Get().([]byte)
		defer packetPool.Put(buf)

		for {
			select {
			case packet, ok := <-packetChan:
				if !ok {
					return
				}
				s.sendPacket(packet, buf, debug)
			case <-ticker.C:
				// Maintain tick rate even if no packets are ready.
			}
		}
	}()

	// Send audio data in chunks.
	for i := 0; i < len(audioData); i += AudioChunkSize {
		end := i + AudioChunkSize
		if end > len(audioData) {
			end = len(audioData)
		}

		chunk := audioData[i:end]
		if bytes.Equal(chunk, emptyAudioPacketData) {
			continue // Skip empty chunks.
		}

		packet := newPCM8khzPacket(chunk)
		packetChan <- packet
	}

	close(packetChan) // Close the channel after sending all data.
	wg.Wait()         // Wait for the goroutine to finish.

	return nil
}

// sendPacket sends a packet through the connection using a pre-allocated buffer.
func (s *FastAudioSocket) sendPacket(packet Packet, buf []byte, debug bool) {
	if debug {
		fmt.Printf("Sending packet: Type=%v, Length=%v\n", packet.Header[0], binary.BigEndian.Uint16(packet.Header[1:]))
	}
	serialized := packet.toBytes(buf)
	if _, err := s.conn.Write(serialized); err != nil {
		if strings.HasSuffix(err.Error(), "broken pipe") {
			return
		}
		if debug {
			fmt.Printf("Failed to write packet: %v\n", err)
		}
	}
}

// ReadPacket reads a packet from the connection.
func (s *FastAudioSocket) ReadPacket() (Packet, error) {
	header := make([]byte, HeaderSize)
	if err := s.readFull(header); err != nil {
		return Packet{}, err
	}

	packetType := header[0]
	payloadLength := binary.BigEndian.Uint16(header[1:3])
	if payloadLength > AudioChunkSize {
		return Packet{}, fmt.Errorf("invalid payload length: %d", payloadLength)
	}

	payload := make([]byte, payloadLength)
	if err := s.readFull(payload); err != nil {
		return Packet{}, err
	}

	return Packet{
		Header: [HeaderSize]byte{packetType, header[1], header[2]},
		Data:   payload,
	}, nil
}

// readFull reads all bytes into the buffer.
func (s *FastAudioSocket) readFull(buf []byte) error {
	_, err := s.conn.Read(buf)
	return err
}

// GetUUID returns the stored UUID.
func (s *FastAudioSocket) GetUUID() [16]byte {
	return s.uuid
}

// Terminate sends a termination packet.
func (s *FastAudioSocket) Terminate() error {
	packet := newPCM8khzPacket(nil) // Create a termination packet.
	packet.Header[0] = PacketTypeTerminate
	buf := packetPool.Get().([]byte)
	defer packetPool.Put(buf)

	_, err := s.conn.Write(packet.toBytes(buf))
	return err
}

// Close closes the connection.
func (s *FastAudioSocket) Close() error {
	return s.conn.Close()
}
