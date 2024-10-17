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

type Packet struct {
	Header [HeaderSize]byte
	Data   []byte
}

func newPCM8khzPacket(chunk []byte) Packet {
	return Packet{Header: writingHeader, Data: chunk}
}

func (p *Packet) toBytes() []byte {
	packetBuffer := packetPool.Get().([]byte)
	defer packetPool.Put(packetBuffer) // Ensure the buffer is returned to the pool

	copy(packetBuffer[:HeaderSize], p.Header[:])
	copy(packetBuffer[HeaderSize:], p.Data)

	return packetBuffer[:HeaderSize+len(p.Data)]
}

func (s *FastAudioSocket) sendPacket(packet Packet) {
	if s.debug {
		fmt.Printf(">>> Sending packet: Type=%#x, Length=%v\n", packet.Header[0], len(packet.Data))
		fmt.Printf(">>> Payload: %v\n", packet.Data)
	}
	serialized := packet.toBytes()
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
	debug bool // Add debug field
}

// Modify NewFastAudioSocket to accept a debug parameter
func NewFastAudioSocket(conn net.Conn, debug bool) (*FastAudioSocket, error) {
	s := &FastAudioSocket{conn: conn, debug: debug} // Initialize debug field
	packet, err := s.ReadPacket()
	if err != nil {
		return nil, fmt.Errorf("failed to read first packet: %w", err)
	}
	if len(packet.Data) != 16 {
		return nil, fmt.Errorf("invalid UUID packet")
	}
	copy(s.uuid[:], packet.Data)
	return s, nil
}

func (s *FastAudioSocket) StreamPCM8khz(audioData []byte) error {
	if len(audioData) == 0 {
		return fmt.Errorf("no audio data to stream")
	}

	packetChan := make(chan Packet)
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

func (s *FastAudioSocket) ReadPacket() (Packet, error) {
	header := make([]byte, HeaderSize)
	if _, err := s.conn.Read(header); err != nil {
		return Packet{}, err
	}

	packetType := header[0]
	payloadLength := binary.BigEndian.Uint16(header[1:3])
	payload := make([]byte, payloadLength)
	if _, err := s.conn.Read(payload); err != nil {
		return Packet{}, err
	}

	if s.debug {
		fmt.Printf("<<< Received packet: Type=%#x, Length=%v\n", packetType, payloadLength)
		fmt.Printf("<<< Payload: %v\n", payload)
	}

	return Packet{
		Header: [HeaderSize]byte{packetType, header[1], header[2]},
		Data:   payload,
	}, nil
}

func (s *FastAudioSocket) GetUUID() [16]byte {
	return s.uuid
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
