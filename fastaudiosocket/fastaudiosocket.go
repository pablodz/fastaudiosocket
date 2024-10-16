package fastaudiosocket

import (
	"encoding/binary"
	"fmt"
	"net"
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

// WritePCM sends a PCM audio packet.
func (s *FastAudioSocket) WritePCM(data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("no data to send")
	}

	packet := make([]byte, 3+len(data))
	packet[0] = PacketTypePCM
	binary.BigEndian.PutUint16(packet[1:3], uint16(len(data)))
	copy(packet[3:], data)

	// Use a single write to send the packet
	_, err := s.conn.Write(packet)
	return err
}

// StreamAudio sends a large slice of PCM audio data spaced by a specified delay in milliseconds.
func (s *FastAudioSocket) StreamAudio(audioData []byte, delayMs int) error {
	if len(audioData) == 0 {
		return fmt.Errorf("no audio data to stream")
	}

	// Preallocate a slice to avoid reallocations
	chunk := make([]byte, AudioChunkSize)

	for i := 0; i < len(audioData); i += AudioChunkSize {
		end := i + AudioChunkSize
		if end > len(audioData) {
			end = len(audioData)
		}
		copy(chunk[:end-i], audioData[i:end]) // Copy data into the chunk

		if err := s.WritePCM(chunk[:end-i]); err != nil {
			return fmt.Errorf("failed to write PCM data: %w", err)
		}
	}

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
