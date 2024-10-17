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
	AudioChunkSize      = 320 // PCM audio chunk size in bytes (20ms of audio)
	MaxHeaderSize       = 3   // Header size: Type (1 byte) + Length (2 bytes)
	MaxAudioSize        = 320 // Maximum audio size in bytes
)

var emptyAudioPacketData = make([]byte, AudioChunkSize) // Reusable empty packet.

// Packet represents a network packet with precomputed header.
type Packet struct {
	Header [MaxHeaderSize]byte // Precomputed header (Type + Length).
	Data   []byte              // Audio data.
}

// newPCM8khzPacket creates a pre-populated PCM packet for 8khz audio chunks.
func newPCM8khzPacket(chunk []byte) Packet {
	header := [MaxHeaderSize]byte{PacketTypePCM, 0x01, 0x40}
	return Packet{
		Header: header,
		Data:   chunk,
	}
}

// toBytes serializes the packet by copying its header and data into a single buffer.
func (p *Packet) toBytes() []byte {
	buf := make([]byte, MaxHeaderSize+MaxAudioSize)
	copy(buf[:MaxHeaderSize], p.Header[:])
	copy(buf[MaxHeaderSize:], p.Data)
	return buf
}

// FastAudioSocket represents the audio socket connection.
type FastAudioSocket struct {
	conn net.Conn
	uuid [16]byte // Store the UUID from the first packet.
}

// NewFastAudioSocket creates a new FastAudioSocket and captures the UUID from the first packet.
func NewFastAudioSocket(conn net.Conn) (*FastAudioSocket, error) {
	s := &FastAudioSocket{conn: conn}

	packet, err := s.ReadPacket()
	if err != nil {
		return nil, fmt.Errorf("failed to read first packet: %w", err)
	}

	if len(packet.Data) != 16 {
		return nil, fmt.Errorf("invalid or missing UUID packet")
	}
	copy(s.uuid[:], packet.Data)

	return s, nil
}

// StreamPCM8khz streams PCM audio data with optimized packet structure.
func (s *FastAudioSocket) StreamPCM8khz(audioData []byte, debug bool) error {
	if len(audioData) == 0 {
		return fmt.Errorf("no audio data to stream")
	}

	packetChan := make(chan Packet, 100) // Buffered channel to avoid blocking.
	var wg sync.WaitGroup

	// Goroutine for timed packet sending.
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case packet, ok := <-packetChan:
				if !ok {
					return // Channel closed, exit the goroutine.
				}
				s.sendPacket(packet, debug)
			case <-ticker.C:
				// Heartbeat to maintain tick rate even if no packets are ready.
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
		if len(chunk) < AudioChunkSize {
			chunk = padChunk(chunk)
		}

		packet := newPCM8khzPacket(chunk)

		// Skip empty packets.
		if bytes.Equal(packet.Data, emptyAudioPacketData) {
			continue
		}

		packetChan <- packet
	}

	close(packetChan) // Close the channel after sending all data.
	wg.Wait()         // Wait for the goroutine to finish.

	return nil
}

// sendPacket sends a packet through the connection.
func (s *FastAudioSocket) sendPacket(packet Packet, debug bool) {
	if debug {
		fmt.Printf("Sending packet: Type=%v, Length=%v\n", packet.Header[0], binary.BigEndian.Uint16(packet.Header[1:]))
	}

	if _, err := s.conn.Write(packet.toBytes()); err != nil {
		if strings.HasSuffix(err.Error(), "broken pipe") {
			return
		}
		if debug {
			fmt.Printf("Failed to write packet: %v\n", err)
		}
	}
}

// padChunk pads a chunk to the required AudioChunkSize.
func padChunk(chunk []byte) []byte {
	padding := make([]byte, AudioChunkSize-len(chunk))
	return append(chunk, padding...)
}

// ReadPacket reads a packet from the connection.
func (s *FastAudioSocket) ReadPacket() (Packet, error) {
	header := make([]byte, MaxHeaderSize)
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
		Header: [MaxHeaderSize]byte{packetType, header[1], header[2]},
		Data:   payload,
	}, nil
}

// readFull ensures all expected bytes are read.
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
	_, err := s.conn.Write(packet.toBytes())
	return err
}

// Close closes the connection.
func (s *FastAudioSocket) Close() error {
	return s.conn.Close()
}
