package fastaudiosocket

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/uuid"
)

const (
	PacketTypeTerminate = 0x00
	PacketTypeUUID      = 0x01
	PacketTypePCM       = 0x10
	PacketTypeError     = 0xff
	AudioChunkSize      = 320 // PCM audio chunk size (20ms of audio)
	HeaderSize          = 3   // Header: Type (1 byte) + Length (2 bytes)
	MaxPacketSize       = 323
)

var (
	emptyAudioPacketData = make([]byte, AudioChunkSize)
	packetPool           = sync.Pool{
		New: func() interface{} {
			return make([]byte, MaxPacketSize)
		},
	}
	writingHeader = [3]byte{PacketTypePCM, 0x01, 0x40}
)

type PacketWriter struct {
	Header  [HeaderSize]byte
	Payload []byte
}
type PacketReader struct {
	Type    byte
	Length  uint16
	Payload []byte
}

func newPCM8khzPacket(chunk []byte) PacketWriter {
	return PacketWriter{Header: writingHeader, Payload: chunk}
}

func (p *PacketWriter) toBytes() []byte {
	packetBuffer := packetPool.Get().([]byte)
	defer packetPool.Put(packetBuffer) // Ensure the buffer is returned to the pool

	copy(packetBuffer[:HeaderSize], p.Header[:])
	copy(packetBuffer[HeaderSize:], p.Payload)

	return packetBuffer[:MaxPacketSize]
}

func (s *FastAudioSocket) sendPacket(packet PacketWriter) {
	serialized := packet.toBytes()
	if s.debug {
		fmt.Printf(">>> Sending packet: Type=%#x, Length=%v, Payload: %v\n", packet.Header[0], len(packet.Payload), serialized)
	}
	if _, err := s.conn.Write(serialized); err != nil {
		if strings.HasSuffix(err.Error(), "broken pipe") {
			return
		}
		if s.debug {
			fmt.Printf("Failed to write packet: %v\n", err)
		}
	}
}

type FastAudioSocket struct {
	conn  net.Conn
	uuid  [16]byte
	debug bool
}

// Modify NewFastAudioSocket to accept a debug parameter
func NewFastAudioSocket(conn net.Conn, debug bool) (*FastAudioSocket, error) {
	s := &FastAudioSocket{conn: conn, debug: debug}
	packet, err := s.ReadPacket()
	if err != nil {
		return nil, fmt.Errorf("failed to read first packet: %w", err)
	}

	if packet.Type != PacketTypeUUID {
		return nil, fmt.Errorf("expected UUID packet, got %#x", packet.Type)
	}

	if len(packet.Payload) != 16 {
		return nil, fmt.Errorf("invalid UUID packet")
	}

	copy(s.uuid[:], packet.Payload)
	return s, nil
}

func (s *FastAudioSocket) StreamPCM8khz(audioData []byte) error {
	if len(audioData) < MaxPacketSize {
		return fmt.Errorf("audio data is too short")
	}

	packetChan := make(chan PacketWriter)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case packet, ok := <-packetChan:
				if !ok {
					return
				}
				s.sendPacket(packet)
			case <-ticker.C:
				// Maintain tick rate even if no packets are ready.
			}
		}
	}()

	for i := 0; i < len(audioData); i += AudioChunkSize {
		end := i + AudioChunkSize
		if end > len(audioData) {
			end = len(audioData)
		}

		chunk := audioData[i:end]
		if bytes.Equal(chunk, emptyAudioPacketData) {
			continue // Skip empty chunks.
		}

		packet := newPCM8khzPacket(chunk)
		packetChan <- packet
	}

	close(packetChan) // Close the channel after sending all data.
	wg.Wait()         // Wait for the goroutine to finish.

	return nil
}

func (s *FastAudioSocket) ReadPacket() (PacketReader, error) {
	header := make([]byte, HeaderSize)
	if _, err := s.conn.Read(header); err != nil {
		return PacketReader{}, err
	}

	packetType := header[0]
	payloadLength := binary.BigEndian.Uint16(header[1:3])
	payload := make([]byte, payloadLength)
	if _, err := s.conn.Read(payload); err != nil {
		return PacketReader{}, err
	}

	if s.debug {
		fmt.Printf("<<< Received packet: Type=%#x, Length=%v, Payload: %v\n", packetType, payloadLength, payload)
	}

	return PacketReader{
		Type:    packetType,
		Length:  payloadLength,
		Payload: payload,
	}, nil
}

func (s *FastAudioSocket) GetUUID() (uuid.UUID, error) {
	return uuid.FromBytes(s.uuid[:])
}

func (s *FastAudioSocket) Terminate() error {
	command := []byte{PacketTypeTerminate, 0x00, 0x00}
	if _, err := s.conn.Write(command); err != nil {
		return fmt.Errorf("failed to send termination packet: %w", err)
	}
	return nil
}

func (s *FastAudioSocket) Close() error {
	return s.conn.Close()
}
