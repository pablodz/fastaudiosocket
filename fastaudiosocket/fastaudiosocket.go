package fastaudiosocket

import (
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"time"
)

// Constants for packet types
const (
	PacketTypeTerminate = 0x00
	PacketTypeUUID      = 0x01
	PacketTypePCM       = 0x10
	PacketTypeError     = 0xff
	AudioChunkSize      = 320 // Size of PCM data in bytes for 20ms of audio
)

// FastAudioSocket represents the audio socket connection and handles both reading and writing.
type FastAudioSocket struct {
	conn    net.Conn
	uuid    [16]byte // Store the first UUID
	uuidSet bool     // Flag to indicate if UUID has been set
}

// NewFastAudioSocket creates a new FastAudioSocket with the given TCP connection
// and captures the UUID from the first packet.
func NewFastAudioSocket(conn net.Conn) (*FastAudioSocket, error) {
	s := &FastAudioSocket{conn: conn}

	// Read the first packet to capture the UUID if it's available
	packetType, payload, err := s.ReadPacket()
	if err != nil {
		return nil, fmt.Errorf("failed to read first packet: %w", err)
	}

	// Check if the first packet is a UUID packet
	if packetType == PacketTypeUUID {
		copy(s.uuid[:], payload)
		s.uuidSet = true
	}

	return s, nil
}

// Initialize a sync.Pool for packet slices.
var packetPool = sync.Pool{
	New: func() interface{} {
		// Allocate a new packet slice of maximum expected size
		return make([]byte, 3+AudioChunkSize)
	},
}

// StreamPCM8khz sends PCM audio data in chunks with a specified delay in milliseconds.
func (s *FastAudioSocket) StreamPCM8khz(audioData []byte) error {
	if len(audioData) == 0 {
		return fmt.Errorf("no audio data to stream")
	}

	// Preallocate a slice to avoid reallocations
	chunk := make([]byte, AudioChunkSize)
	packetChan := make(chan []byte)

	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()

		for range ticker.C {
			select {
			case packet, ok := <-packetChan:
				if !ok {
					return
				}
				if _, err := s.conn.Write(packet); err != nil {
					fmt.Printf("failed to write PCM data: %v\n", err)
					return
				}
				// Return the packet to the pool after use
				packetPool.Put(packet)
			}
		}
	}()

	for i := 0; i < len(audioData); i += AudioChunkSize {
		end := i + AudioChunkSize
		if end > len(audioData) {
			end = len(audioData)
		}

		// Check if there's enough data to send a full packet
		if end-i < AudioChunkSize && end < len(audioData) {
			// Not enough data to form a complete packet, omit this chunk
			continue
		}

		copy(chunk[:end-i], audioData[i:end]) // Copy data into the chunk

		// Get a packet from the pool
		packet := packetPool.Get().([]byte)
		packet[0] = PacketTypePCM
		binary.BigEndian.PutUint16(packet[1:3], uint16(end-i))
		copy(packet[3:], chunk[:end-i])

		packetChan <- packet
	}

	close(packetChan)

	return nil
}

// ReadPacket reads a single packet from the connection.
func (s *FastAudioSocket) ReadPacket() (byte, []byte, error) {
	header := make([]byte, 3)
	_, err := s.conn.Read(header)
	if err != nil {
		return 0, nil, err
	}

	packetType := header[0]
	payloadLength := binary.BigEndian.Uint16(header[1:3])
	payload := make([]byte, payloadLength)
	_, err = s.conn.Read(payload)
	if err != nil {
		return packetType, nil, err
	}

	return packetType, payload, nil
}

// GetUUID returns the stored UUID if set.
func (s *FastAudioSocket) GetUUID() ([16]byte, bool) {
	return s.uuid, s.uuidSet
}

// Terminate sends a termination packet.
func (s *FastAudioSocket) Terminate() error {
	packet := []byte{PacketTypeTerminate, 0x00, 0x00} // 3-byte terminate packet
	_, err := s.conn.Write(packet)
	return err
}

// Close closes the connection.
func (s *FastAudioSocket) Close() error {
	return s.conn.Close()
}
