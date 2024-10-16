package fastaudiosocket

import (
	"bufio"
	"encoding/binary"
	"errors"
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
	AudioChunkSize      = 320 // Size of PCM data for 20ms of audio
)

// FastAudioSocket manages an audio connection with efficient reading and writing.
type FastAudioSocket struct {
	conn    net.Conn
	writer  *bufio.Writer // Buffered writer to reduce write syscalls
	reader  *bufio.Reader // Buffered reader for efficient reads
	uuid    [16]byte      // Store the first UUID
	uuidSet bool          // Whether the UUID has been captured
	bufPool *sync.Pool    // Pool of reusable byte buffers
}

// NewFastAudioSocket initializes a new audio socket with a TCP connection.
func NewFastAudioSocket(conn net.Conn) *FastAudioSocket {
	pool := &sync.Pool{New: func() interface{} { return make([]byte, AudioChunkSize) }}

	return &FastAudioSocket{
		conn:    conn,
		writer:  bufio.NewWriter(conn),
		reader:  bufio.NewReader(conn),
		bufPool: pool,
	}
}

// HandleConnection manages the connection lifecycle in a goroutine.
func (s *FastAudioSocket) HandleConnection() {
	defer s.Close()

	// Read the initial packet to capture UUID if available.
	packetType, payload, err := s.ReadPacket()
	if err != nil {
		fmt.Printf("Error reading packet: %v\n", err)
		return
	}

	if packetType == PacketTypeUUID {
		copy(s.uuid[:], payload)
		s.uuidSet = true
	}

	// Example loop: continuously read incoming data.
	for {
		_, _, err := s.ReadPacket()
		if err != nil {
			fmt.Printf("Connection closed or error: %v\n", err)
			return
		}
	}
}

// WritePCM sends a PCM audio packet using a reusable buffer.
func (s *FastAudioSocket) WritePCM(data []byte) error {
	if len(data) == 0 {
		return errors.New("no data to send")
	}

	packet := s.bufPool.Get().([]byte)[:3+len(data)]
	packet[0] = PacketTypePCM
	binary.BigEndian.PutUint16(packet[1:3], uint16(len(data)))
	copy(packet[3:], data)

	if _, err := s.writer.Write(packet); err != nil {
		return err
	}

	s.bufPool.Put(packet[:AudioChunkSize]) // Return buffer to pool
	return s.writer.Flush()
}

// StreamAudio sends PCM data with precise 20ms spacing using pooled buffers.
func (s *FastAudioSocket) StreamAudio(audioData []byte, delayMs int) error {
	if len(audioData) == 0 {
		return errors.New("no audio data to stream")
	}

	delay := time.Duration(delayMs) * time.Millisecond
	startTime := time.Now()

	for i := 0; i < len(audioData); i += AudioChunkSize {
		end := i + AudioChunkSize
		if end > len(audioData) {
			end = len(audioData)
		}

		buf := s.bufPool.Get().([]byte)[:end-i]
		copy(buf, audioData[i:end])

		if err := s.WritePCM(buf); err != nil {
			return fmt.Errorf("failed to write PCM data: %w", err)
		}
		s.bufPool.Put(buf) // Return buffer to pool

		elapsed := time.Since(startTime)
		sleepDuration := delay - elapsed
		if sleepDuration > 0 {
			time.Sleep(sleepDuration)
		}

		startTime = time.Now() // Reset start time
	}

	return nil
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
	packet := []byte{PacketTypeTerminate, 0x00, 0x00}
	_, err := s.writer.Write(packet)
	s.writer.Flush()
	return err
}

// Close closes the connection.
func (s *FastAudioSocket) Close() error {
	return s.conn.Close()
}
