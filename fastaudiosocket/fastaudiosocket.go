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

// StreamWritePCM sends PCM audio data in chunks with a specified delay of 20 milliseconds between packets.
func (s *FastAudioSocket) StreamWritePCM(audioData []byte) error {
	const packetDelay = 20 * time.Millisecond // Define the desired delay between packets
	if len(audioData) == 0 {
		return fmt.Errorf("no audio data to stream")
	}

	// Preallocate a packet buffer to avoid reallocations
	packetBuffer := make([]byte, 3+AudioChunkSize) // Max size needed for a packet
	packetBuffer[0] = PacketTypePCM                // Set packet type

	// Create a buffered channel to send packets
	packetChannel := make(chan []byte, 10) // Buffer for 10 packets

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Start a ticker for the specified packet delay
		ticker := time.NewTicker(packetDelay)
		defer ticker.Stop() // Ensure ticker stops when the goroutine exits

		for {
			select {
			case packet, ok := <-packetChannel:
				if !ok {
					return // Exit if the channel is closed
				}

				// Wait for the next tick before sending the packet
				<-ticker.C // Wait for the ticker to tick

				if _, err := s.conn.Write(packet); err != nil {
					fmt.Printf("failed to write PCM data: %v\n", err) // Log error but don't stop streaming
				}

			default:
				// If there are no packets to send, wait for the next tick
				<-ticker.C
			}
		}
	}()

	// Preallocate a slice to avoid reallocations
	chunk := make([]byte, AudioChunkSize)

	for i := 0; i < len(audioData); i += AudioChunkSize {
		end := i + AudioChunkSize
		if end > len(audioData) {
			end = len(audioData)
		}
		copy(chunk[:end-i], audioData[i:end]) // Copy data into the chunk

		// Prepare the packet for sending
		binary.BigEndian.PutUint16(packetBuffer[1:3], uint16(end-i)) // Set the length of the chunk
		copy(packetBuffer[3:], chunk[:end-i])                        // Copy the chunk into the packet

		// Send the packet to the channel
		packetChannel <- packetBuffer[:3+end-i] // Send the slice of the packet buffer
	}

	close(packetChannel) // Close the channel after all packets are sent
	wg.Wait()            // Wait for the goroutine to finish
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
