package fastaudiosocket

import (
	"bytes"
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

var emptyAudioPacketData = make([]byte, AudioChunkSize)

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
		return make([]byte, 3+AudioChunkSize) // Allocate for header + data
	},
}

// StreamPCM8khz sends PCM audio data in chunks with a specified delay in milliseconds.
func (s *FastAudioSocket) StreamPCM8khz(audioData []byte, debug bool) error {
	if len(audioData) == 0 {
		return fmt.Errorf("no audio data to stream")
	}

	if len(audioData) < AudioChunkSize {
		fmt.Printf("audio data length not enough: %v\n", len(audioData))
		return nil
	}

	packetChan := make(chan []byte)
	var wg sync.WaitGroup

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
					// print headers, 3 first bytes and length
					fmt.Printf("packet header: %v, length: %v\n", packet[:3], len(packet))
				}

				if _, err := s.conn.Write(packet); err != nil {
					if debug {
						fmt.Printf("> last packet length: %v\n", len(lastPacket))
						fmt.Printf("> last packet: %v\n", lastPacket)
						fmt.Printf("- packet length: %v\n", len(packet))
						fmt.Printf("- packet: %v\n", packet)
					}
					fmt.Printf("failed to write PCM data: %v\n", err)
					return
				}

				if debug {
					lastPacket = packet
				}

				packetPool.Put(packet) // Return the packet to the pool
			}
		}
	}()

	for i := 0; i < len(audioData); i += AudioChunkSize {
		end := i + AudioChunkSize
		if end > len(audioData) {
			end = len(audioData)
		}

		chunk := audioData[i:end]

		// Get a packet from the pool
		packet := packetPool.Get().([]byte)
		if cap(packet) < 3+len(chunk) {
			return fmt.Errorf("packet from pool is too small")
		}

		// Construct header and copy data
		packet[0] = PacketTypePCM
		binary.BigEndian.PutUint16(packet[1:3], uint16(len(chunk)))
		copy(packet[3:], chunk)

		// sometimes the last packet is empty, skip it, also skip empty packets
		if len(packet) == 0 || bytes.Equal(packet[3:], emptyAudioPacketData) {
			continue
		}

		if packet[0] != PacketTypePCM {
			fmt.Printf("unexpected packet type: %v\n", packet[0])
			continue
		}

		// bytes 1-2 are the length of the payload and should be equal to AudioChunkSize
		if packet[1] != 0x01 || packet[2] != 0x40 {
			fmt.Printf("unexpected packet length: %v\n", packet[1:3])
			continue
		}

		packetChan <- packet
	}

	close(packetChan) // Close the channel after sending all data
	wg.Wait()         // Wait for the goroutine to finish

	return nil
}

// ReadPacket reads a single packet from the connection.
func (s *FastAudioSocket) ReadPacket() (byte, []byte, error) {
	header := make([]byte, 3)
	if _, err := s.conn.Read(header); err != nil {
		return 0, nil, err
	}

	packetType := header[0]
	payloadLength := binary.BigEndian.Uint16(header[1:3])
	payload := make([]byte, payloadLength)

	if _, err := s.conn.Read(payload); err != nil {
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
