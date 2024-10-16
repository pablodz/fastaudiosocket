package fastaudiosocket

import (
	"bufio"
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
	MaxBufferedPackets  = 10  // Number of packets to buffer for batch sending
)

// FastAudioSocket represents the audio socket connection and handles both reading and writing.
type FastAudioSocket struct {
	conn       net.Conn
	reader     *bufio.Reader
	writer     *bufio.Writer
	uuid       [16]byte   // Store the first UUID
	uuidSet    bool       // Flag to indicate if UUID has been set
	packetPool *sync.Pool // Pool for packet buffers
}

// NewFastAudioSocket creates a new FastAudioSocket with the given TCP connection
// and captures the UUID from the first packet.
func NewFastAudioSocket(conn net.Conn) (*FastAudioSocket, error) {
	pool := &sync.Pool{
		New: func() interface{} {
			return make([]byte, AudioChunkSize+3) // Preallocate space for packets
		},
	}

	// Initialize buffered reader and writer
	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)

	s := &FastAudioSocket{
		conn:       conn,
		reader:     reader,
		writer:     writer,
		packetPool: pool,
	}

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

	packet := s.packetPool.Get().([]byte)
	defer s.packetPool.Put(packet)

	packet[0] = PacketTypePCM
	binary.BigEndian.PutUint16(packet[1:3], uint16(len(data)))
	copy(packet[3:], data)

	// Write to the buffered writer
	if _, err := s.writer.Write(packet); err != nil {
		return fmt.Errorf("failed to write packet: %w", err)
	}

	// Check if we reached the maximum buffered packets
	if s.writer.Buffered() >= (AudioChunkSize+3)*MaxBufferedPackets {
		if err := s.flush(); err != nil {
			return err
		}
	}

	return nil
}

// flush sends all buffered packets in one go
func (s *FastAudioSocket) flush() error {
	if err := s.writer.Flush(); err != nil {
		return fmt.Errorf("failed to flush buffered packets: %w", err)
	}
	return nil
}

// StreamAudio sends a large slice of PCM audio data spaced by a specified delay in milliseconds.
func (s *FastAudioSocket) StreamAudio(audioData []byte, delayMs int) error {
	if len(audioData) == 0 {
		return fmt.Errorf("no audio data to stream")
	}

	startTime := time.Now()
	for i := 0; i < len(audioData); i += AudioChunkSize {
		end := i + AudioChunkSize
		if end > len(audioData) {
			end = len(audioData)
		}

		if err := s.WritePCM(audioData[i:end]); err != nil {
			return fmt.Errorf("failed to write PCM data: %w", err)
		}

		// Calculate time spent on sending data
		elapsed := time.Since(startTime)
		time.Sleep(time.Duration(delayMs)*time.Millisecond - elapsed)
		startTime = time.Now() // Reset start time for the next iteration
	}

	// Flush any remaining packets after streaming
	return s.flush()
}

// ReadPacket reads a single packet from the connection.
func (s *FastAudioSocket) ReadPacket() (byte, []byte, error) {
	header := make([]byte, 3)
	if _, err := s.reader.Read(header); err != nil {
		return 0, nil, err
	}

	packetType := header[0]
	payloadLength := binary.BigEndian.Uint16(header[1:3])
	payload := make([]byte, payloadLength)
	if _, err := s.reader.Read(payload); err != nil {
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
	if _, err := s.writer.Write(packet); err != nil {
		return err
	}
	return s.flush() // Ensure termination packet is sent
}

// Close closes the connection.
func (s *FastAudioSocket) Close() error {
	// Flush any remaining packets before closing
	if err := s.flush(); err != nil {
		return err
	}
	return s.conn.Close()
}
