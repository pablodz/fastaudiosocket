package fastaudiosocket

import (
	"encoding/binary"
	"fmt"
	"net"
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

// FastAudioSocket represents the audio socket connection.
type FastAudioSocket struct {
	conn net.Conn
}

// NewAudioSocket creates a new AudioSocket with the given TCP connection.
func NewAudioSocket(conn net.Conn) *FastAudioSocket {
	return &FastAudioSocket{conn: conn}
}

// Writer handles sending audio packets.
type Writer struct {
	conn net.Conn
}

// NewWriter creates a new Writer instance.
func NewWriter(conn net.Conn) *Writer {
	return &Writer{conn: conn}
}

// WritePCM sends a PCM audio packet.
func (w *Writer) WritePCM(data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("no data to send")
	}

	packet := make([]byte, 3+len(data))
	packet[0] = PacketTypePCM
	binary.BigEndian.PutUint16(packet[1:3], uint16(len(data)))
	copy(packet[3:], data)

	_, err := w.conn.Write(packet)
	return err
}

// StreamAudio sends a large slice of PCM audio data spaced by a specified delay in milliseconds.
func (w *Writer) StreamAudio(audioData []byte, delayMs int) error {
	if len(audioData) == 0 {
		return fmt.Errorf("no audio data to stream")
	}

	for i := 0; i < len(audioData); i += AudioChunkSize {
		end := i + AudioChunkSize
		if end > len(audioData) {
			end = len(audioData)
		}
		chunk := audioData[i:end]

		err := w.WritePCM(chunk)
		if err != nil {
			return fmt.Errorf("failed to write PCM data: %w", err)
		}

		time.Sleep(time.Duration(delayMs) * time.Millisecond) // Wait for the specified delay
	}
	return nil
}

// Terminate sends a termination packet.
func (w *Writer) Terminate() error {
	packet := []byte{PacketTypeTerminate, 0x00, 0x00} // 3-byte terminate packet
	_, err := w.conn.Write(packet)
	return err
}

// Reader handles receiving audio packets.
type Reader struct {
	conn    net.Conn
	uuid    [16]byte // Store the first UUID
	uuidSet bool     // Flag to indicate if UUID has been set
}

// NewReader creates a new Reader instance.
func NewReader(conn net.Conn) *Reader {
	return &Reader{conn: conn}
}

// ReadPacket reads a single packet from the connection.
func (r *Reader) ReadPacket() (byte, []byte, error) {
	header := make([]byte, 3)
	_, err := r.conn.Read(header)
	if err != nil {
		return 0, nil, err
	}

	packetType := header[0]
	payloadLength := binary.BigEndian.Uint16(header[1:3])
	payload := make([]byte, payloadLength)
	_, err = r.conn.Read(payload)
	if err != nil {
		return packetType, nil, err
	}

	// Check for UUID packet and store it
	if packetType == PacketTypeUUID && !r.uuidSet {
		copy(r.uuid[:], payload)
		r.uuidSet = true
	}

	return packetType, payload, nil
}

// GetUUID returns the stored UUID if set.
func (r *Reader) GetUUID() ([16]byte, bool) {
	return r.uuid, r.uuidSet
}

// Close closes the connection.
func (a *FastAudioSocket) Close() error {
	return a.conn.Close()
}
