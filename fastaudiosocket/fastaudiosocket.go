package fastaudiosocket

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	PacketTypeHangup = 0x00
	PacketTypeUUID   = 0x01
	PacketTypePCM    = 0x10
	PacketTypeError  = 0xff
	WriteChunkSize   = 320 // PCM audio chunk size (20ms of audio)
	HeaderSize       = 3   // Header: Type (1 byte) + Length (2 bytes)
	MaxPacketSize    = 323
)

var (
	onceSilentPacket sync.Once
	silentPacket     []byte
	writingHeader    = [3]byte{PacketTypePCM, 0x01, 0x40}
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

func getSilentPacket() []byte {
	onceSilentPacket.Do(func() {
		silentPacket = make([]byte, MaxPacketSize)
		silentPacket[0] = PacketTypePCM
		silentPacket[1] = 0x01
		silentPacket[2] = 0x40
		for i := 3; i < MaxPacketSize; i++ {
			silentPacket[i] = 0xff // set 255 as silent packet
		}
	})

	return silentPacket
}

type FastAudioSocket struct {
	conn       net.Conn
	uuid       string
	debug      bool
	readerChan chan PacketReader
}

func (p *PacketWriter) toBytes() []byte {
	packetBuffer := make([]byte, MaxPacketSize)
	copy(packetBuffer[:HeaderSize], p.Header[:])
	copy(packetBuffer[HeaderSize:], p.Payload)
	return packetBuffer[:MaxPacketSize]
}

func (s *FastAudioSocket) sendPacket(packet PacketWriter) {
	serialized := packet.toBytes()
	if s.debug {
		fmt.Printf(">>> Sending packet: Type=%#x, Length=%v\n", packet.Header[0], len(packet.Payload))
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

// Modify NewFastAudioSocket to accept a debug parameter
func NewFastAudioSocket(conn net.Conn, debug bool) (*FastAudioSocket, error) {
	s := &FastAudioSocket{
		conn:       conn,
		debug:      debug,
		readerChan: make(chan PacketReader),
	}

	uuid, err := s.readUUID()
	if err != nil {
		return nil, fmt.Errorf("failed to read UUID: %w", err)
	}

	s.uuid = uuid.String()

	return s, nil
}

// Read first package as uuid from s.conn, dont use ReadPacket
func (s *FastAudioSocket) readUUID() (uuid.UUID, error) {
	header := make([]byte, HeaderSize)
	if _, err := s.conn.Read(header); err != nil {
		return uuid.Nil, err
	}

	packetType := header[0]
	payloadLength := binary.BigEndian.Uint16(header[1:3])
	payload := make([]byte, payloadLength)
	if _, err := s.conn.Read(payload); err != nil {
		return uuid.Nil, err
	}

	if s.debug {
		fmt.Printf("<<< Received packet: Type=%#x, Length=%v, Payload: %v\n", packetType, payloadLength, payload)
	}

	if packetType != PacketTypeUUID {
		return uuid.Nil, fmt.Errorf("expected UUID packet, got %#x", packetType)
	}

	if len(payload) != 16 {
		return uuid.Nil, fmt.Errorf("invalid UUID packet")
	}

	return uuid.FromBytes(payload)
}

func (s *FastAudioSocket) readChunk() (PacketReader, error) {
	header := make([]byte, HeaderSize)
	if _, err := s.conn.Read(header); err != nil {
		return PacketReader{}, err
	}

	packetType := header[0]
	payloadLength := binary.BigEndian.Uint16(header[1:3])

	if packetType != PacketTypePCM {
		return PacketReader{
			Type:   packetType,
			Length: payloadLength,
		}, nil
	}

	payload := make([]byte, payloadLength)
	if _, err := s.conn.Read(payload); err != nil {
		return PacketReader{
			Type:   packetType,
			Length: payloadLength,
		}, err
	}

	if s.debug {
		fmt.Printf("<<< Received packet: Type=%#x, Length=%v\n", packetType, payloadLength)
	}

	return PacketReader{
		Type:    packetType,
		Length:  payloadLength,
		Payload: payload,
	}, nil
}

// StreamRead reads packets from the connection and sends them to the reader channel.
func (s *FastAudioSocket) StreamRead(ctx context.Context, cancel context.CancelFunc) {
	if s.debug {
		fmt.Println("-- StreamRead START --")
		defer fmt.Println("-- StreamRead STOP --")
	}

	defer func() {
		cancel()
		close(s.readerChan)
	}()

	for {
		select {
		case <-ctx.Done():
			return
		default:
			packet, err := s.readChunk()
			if err != nil {
				if s.debug {
					fmt.Printf("Failed to read packet: %v\n", err)
				}
				return
			}

			s.readerChan <- packet
		}
	}
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

		for range ticker.C {
			select {
			case packet, ok := <-packetChan:
				if !ok {
					return
				}
				s.sendPacket(packet)
			}
		}
	}()

	for i := 0; i < len(audioData); i += WriteChunkSize {
		end := i + WriteChunkSize
		if end > len(audioData) {
			end = len(audioData)
		}

		chunk := audioData[i:end]
		// complete the last chunk with silence if it is less than WriteChunkSize
		if len(chunk) < WriteChunkSize {
			chunk = append(chunk, getSilentPacket()[:WriteChunkSize-len(chunk)]...)
		}

		if bytes.Equal(chunk, getSilentPacket()) {
			continue // Skip empty chunks.
		}

		packet := newPCM8khzPacket(chunk)
		packetChan <- packet
	}

	close(packetChan) // Close the channel after sending all data.
	wg.Wait()         // Wait for the goroutine to finish.

	return nil
}

func newPCM8khzPacket(chunk []byte) PacketWriter {
	return PacketWriter{Header: writingHeader, Payload: chunk}
}

func (s *FastAudioSocket) GetUUID() string {
	return s.uuid
}

func (s *FastAudioSocket) Hangup() error {
	command := []byte{PacketTypeHangup, 0x00, 0x00}
	if _, err := s.conn.Write(command); err != nil {
		return fmt.Errorf("failed to send termination packet: %w", err)
	}
	return nil
}

func (s *FastAudioSocket) Close() error {
	return s.conn.Close()
}
