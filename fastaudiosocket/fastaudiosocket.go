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
)

var emptyAudioPacketData = make([]byte, AudioChunkSize)

// Packet represents a network packet.
type Packet struct {
	Type   byte
	Length uint16
	Data   []byte
}

// newPacket creates a new Packet instance.
func newPacket(packetType byte, data []byte) Packet {
	return Packet{
		Type:   packetType,
		Length: uint16(len(data)),
		Data:   data,
	}
}

// toBytes serializes the packet into a byte slice.
func (p *Packet) toBytes() []byte {
	buf := make([]byte, 3+len(p.Data))
	buf[0] = p.Type
	binary.BigEndian.PutUint16(buf[1:3], p.Length)
	copy(buf[3:], p.Data)
	return buf
}

// FastAudioSocket represents the audio socket connection.
type FastAudioSocket struct {
	conn net.Conn
	uuid [16]byte // Store the UUID from the first packet
}

// NewFastAudioSocket creates a new FastAudioSocket and captures the UUID from the first packet.
func NewFastAudioSocket(conn net.Conn) (*FastAudioSocket, error) {
	s := &FastAudioSocket{conn: conn}

	packet, err := s.ReadPacket()
	if err != nil {
		return nil, fmt.Errorf("failed to read first packet: %w", err)
	}

	if packet.Type == PacketTypeUUID && len(packet.Data) == 16 {
		copy(s.uuid[:], packet.Data)
	} else {
		return nil, fmt.Errorf("invalid UUID packet")
	}

	return s, nil
}

// StreamPCM8khz streams PCM audio data with precise packet structure.
func (s *FastAudioSocket) StreamPCM8khz(audioData []byte, debug bool) error {
	if len(audioData) == 0 {
		return fmt.Errorf("no audio data to stream")
	}

	packetChan := make(chan Packet)
	var wg sync.WaitGroup

	// Goroutine for timed packet sending
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()

		for range ticker.C {
			select {
			case packet, ok := <-packetChan:
				if !ok {
					return // Channel closed, exit goroutine
				}

				if debug {
					fmt.Printf("Sending packet: Type=%v, Length=%v\n", packet.Type, packet.Length)
				}

				if _, err := s.conn.Write(packet.toBytes()); err != nil {
					if strings.HasSuffix(err.Error(), "broken pipe") {
						return
					}
					if debug {
						fmt.Printf("Failed to write packet: %v\n", err)
					}
					fmt.Printf("Error writing PCM data: %v\n", err)
					return
				}
			}
		}
	}()

	// Send audio data in chunks
	for i := 0; i < len(audioData); i += AudioChunkSize {
		end := i + AudioChunkSize
		if end > len(audioData) {
			end = len(audioData)
		}

		chunk := audioData[i:end]

		// Pad the chunk if it's smaller than AudioChunkSize
		if len(chunk) < AudioChunkSize {
			chunk = append(chunk, make([]byte, AudioChunkSize-len(chunk))...)
		}

		packet := newPacket(PacketTypePCM, chunk)

		// Skip empty or incorrect packets
		if bytes.Equal(packet.Data, emptyAudioPacketData) {
			continue
		}

		packetChan <- packet
	}

	close(packetChan) // Close channel after sending all data
	wg.Wait()         // Wait for goroutine to finish

	return nil
}

// ReadPacket reads a packet from the connection.
func (s *FastAudioSocket) ReadPacket() (Packet, error) {
	header := make([]byte, 3)
	if _, err := s.conn.Read(header); err != nil {
		return Packet{}, err
	}

	packetType := header[0]
	payloadLength := binary.BigEndian.Uint16(header[1:3])

	if payloadLength > AudioChunkSize {
		return Packet{}, fmt.Errorf("invalid payload length: %d", payloadLength)
	}

	payload := make([]byte, payloadLength)
	if _, err := s.conn.Read(payload); err != nil {
		return Packet{}, err
	}

	return Packet{
		Type:   packetType,
		Length: payloadLength,
		Data:   payload,
	}, nil
}

// GetUUID returns the stored UUID if set.
func (s *FastAudioSocket) GetUUID() [16]byte {
	return s.uuid
}

// Terminate sends a termination packet.
func (s *FastAudioSocket) Terminate() error {
	packet := newPacket(PacketTypeTerminate, nil)
	_, err := s.conn.Write(packet.toBytes())
	return err
}

// Close closes the connection.
func (s *FastAudioSocket) Close() error {
	return s.conn.Close()
}
