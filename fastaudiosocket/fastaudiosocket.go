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

// Constants for packet types
const (
	PacketTypeTerminate = 0x00
	PacketTypeUUID      = 0x01
	PacketTypePCM       = 0x10
	PacketTypeError     = 0xff
	AudioChunkSize      = 320 // PCM audio chunk size in bytes (20ms of audio)
)

var emptyAudioPacketData = make([]byte, AudioChunkSize)

// FastAudioSocket represents the audio socket connection.
type FastAudioSocket struct {
	conn net.Conn
	uuid [16]byte // Store the UUID from the first packet
}

// NewFastAudioSocket creates a new FastAudioSocket and captures the UUID from the first packet.
func NewFastAudioSocket(conn net.Conn) (*FastAudioSocket, error) {
	s := &FastAudioSocket{conn: conn}

	packetType, payload, err := s.ReadPacket()
	if err != nil {
		return nil, fmt.Errorf("failed to read first packet: %w", err)
	}

	if packetType == PacketTypeUUID && len(payload) == 16 {
		copy(s.uuid[:], payload)
	}

	return s, nil
}

// Packet pool for memory efficiency.
var packetPool = sync.Pool{
	New: func() interface{} {
		return make([]byte, 3+AudioChunkSize) // Header + PCM data
	},
}

// StreamPCM8khz streams PCM audio data with precise packet structure.
func (s *FastAudioSocket) StreamPCM8khz(audioData []byte, debug bool) error {
	if len(audioData) == 0 {
		return fmt.Errorf("no audio data to stream")
	}

	packetChan := make(chan []byte)
	var wg sync.WaitGroup

	// Goroutine for timed packet sending
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()

		lastPacket := []byte{}
		for range ticker.C {
			select {
			case packet, ok := <-packetChan:
				if !ok {
					return // Channel closed, exit goroutine
				}

				if debug {
					fmt.Printf("packet header: %v, length: %v\n", packet[:3], len(packet))
				}

				if _, err := s.conn.Write(packet); err != nil {
					if debug {
						fmt.Printf("> last packet length: %v\n", len(lastPacket))
						fmt.Printf("> last packet: %v\n", lastPacket)
						fmt.Printf("- packet length: %v\n", len(packet))
						fmt.Printf("- packet: %v\n", packet)
					}
					if strings.HasSuffix(err.Error(), "broken pipe") {
						return
					}
					fmt.Printf("failed to write PCM data: %v\n", err)
					return
				}

				if debug {
					lastPacket = packet
				}

				packetPool.Put(packet) // Return to pool
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

		// Get packet from the pool
		packet := packetPool.Get().([]byte)
		packet[0] = PacketTypePCM
		binary.BigEndian.PutUint16(packet[1:3], uint16(AudioChunkSize))
		copy(packet[3:], chunk)

		// Skip empty or incorrect packets
		if bytes.Equal(packet[3:], emptyAudioPacketData) {
			packetPool.Put(packet) // Return to pool
			continue
		}

		packetChan <- packet
	}

	close(packetChan) // Close channel after sending all data
	wg.Wait()         // Wait for goroutine to finish

	return nil
}

// ReadPacket reads a packet from the connection.
func (s *FastAudioSocket) ReadPacket() (byte, []byte, error) {
	header := make([]byte, 3)
	if _, err := s.conn.Read(header); err != nil {
		return 0, nil, err
	}

	packetType := header[0]
	payloadLength := binary.BigEndian.Uint16(header[1:3])

	if payloadLength > AudioChunkSize {
		return packetType, nil, fmt.Errorf("invalid payload length: %d", payloadLength)
	}

	payload := make([]byte, payloadLength)
	if _, err := s.conn.Read(payload); err != nil {
		return packetType, nil, err
	}

	return packetType, payload, nil
}

// GetUUID returns the stored UUID if set.
func (s *FastAudioSocket) GetUUID() [16]byte {
	return s.uuid
}

// Terminate sends a termination packet.
func (s *FastAudioSocket) Terminate() error {
	packet := []byte{PacketTypeTerminate, 0x00, 0x00}
	_, err := s.conn.Write(packet)
	return err
}

// Close closes the connection.
func (s *FastAudioSocket) Close() error {
	return s.conn.Close()
}
