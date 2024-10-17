package fastaudiosocket

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
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
	chunksPerSecond  = 50
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
type MonitorResponse struct {
	Message              string
	ChunkCounterReceived int32
	ExpectedChunks       int32
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
	ctx          context.Context
	cancel       context.CancelFunc
	conn         net.Conn
	uuid         string
	AudioChan    chan PacketReader
	MonitorChan  chan MonitorResponse
	chunkCounter int32
	debug        bool
}

// Constructor que recibe el contexto y configura el socket
func NewFastAudioSocket(ctx context.Context, conn net.Conn, debug bool, monitorEnabled bool) (*FastAudioSocket, error) {
	ctx, cancel := context.WithCancel(ctx)

	s := &FastAudioSocket{
		ctx:          ctx,
		cancel:       cancel,
		conn:         conn,
		AudioChan:    make(chan PacketReader),
		MonitorChan:  make(chan MonitorResponse),
		chunkCounter: int32(0),
		debug:        debug,
	}

	uuid, err := s.readUUID()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to read UUID: %w", err)
	}
	s.uuid = uuid.String()

	go s.streamRead()
	if monitorEnabled {
		go s.monitor()
	}

	go func() {
		<-ctx.Done()
		s.closeInternal()
	}()

	return s, nil
}

func (s *FastAudioSocket) closeInternal() {
	if s.debug {
		fmt.Println("Closing FastAudioSocket...")
	}
	close(s.AudioChan)
	close(s.MonitorChan)
	s.conn.Close()
}

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

func (s *FastAudioSocket) streamRead() {
	if s.debug {
		fmt.Println("-- StreamRead START --")
		defer fmt.Println("-- StreamRead STOP --")
	}

	defer s.cancel()

	for {
		select {
		case <-s.ctx.Done():
			return
		default:
			packet, err := s.readChunk()
			if err != nil {
				if s.debug {
					fmt.Printf("Failed to read packet: %v\n", err)
				}
				return
			}
			atomic.AddInt32(&s.chunkCounter, 1)

			if packet.Type != PacketTypePCM {
				if s.debug {
					fmt.Printf("Received packet with type %#x\n", packet.Type)
				}
				return
			}

			s.AudioChan <- packet
		}
	}
}

func (s *FastAudioSocket) monitor() {
	if s.debug {
		fmt.Println("-- Monitor START --")
		defer fmt.Println("-- Monitor STOP --")
	}

	monitorInterval := 2 * time.Second
	ticker := time.NewTicker(monitorInterval)
	defer ticker.Stop()

	lastCounter := int32(0)
	chunksExpected := int32(chunksPerSecond * monitorInterval.Seconds())
	intermitentFactor := 0.5
	minimalIntermitentChunks := int32(chunksPerSecond * monitorInterval.Seconds() * intermitentFactor)

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			if s.debug {
				fmt.Printf("Chunk counter: %v\n", s.chunkCounter)
			}
			currentCounter := atomic.LoadInt32(&s.chunkCounter)
			chunksReceived := currentCounter - lastCounter

			switch {
			case chunksReceived == chunksExpected:
				s.MonitorChan <- MonitorResponse{
					Message:              "Monitor: ✅ Expected chunks received",
					ChunkCounterReceived: chunksReceived,
					ExpectedChunks:       chunksExpected,
				}
			case chunksReceived == 0:
				s.MonitorChan <- MonitorResponse{
					Message:              "Monitor: 🚨 No chunks received",
					ChunkCounterReceived: chunksReceived,
					ExpectedChunks:       chunksExpected,
				}
			case chunksReceived < minimalIntermitentChunks:
				s.MonitorChan <- MonitorResponse{
					Message:              "Monitor: 🚨 Intermitent chunks received",
					ChunkCounterReceived: chunksReceived,
					ExpectedChunks:       chunksExpected,
				}
			case chunksReceived > chunksExpected:
				s.MonitorChan <- MonitorResponse{
					Message:              "Monitor: ⚡ Too many chunks received",
					ChunkCounterReceived: chunksReceived,
					ExpectedChunks:       chunksExpected,
				}
			}

			lastCounter = currentCounter
		}
	}
}

func (s *FastAudioSocket) StreamWritePCM8khz(audioData []byte) error {
	if s.debug {
		fmt.Println("-- StreamWritePCM8khz START --")
		defer fmt.Println("-- StreamWritePCM8khz STOP --")
	}

	if len(audioData) < MaxPacketSize {
		return fmt.Errorf("audio data is too short")
	}

	packetChan := make(chan PacketWriter, 10)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()

		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-s.ctx.Done():
				return
			case <-ticker.C:
				packet, ok := <-packetChan
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

		if len(chunk) < WriteChunkSize {
			silence := getSilentPacket()[:WriteChunkSize-len(chunk)]
			chunk = append(chunk, silence...)
		}

		if bytes.Equal(chunk, getSilentPacket()) {
			continue
		}

		packet := newPCM8khzPacket(chunk)

		select {
		case packetChan <- packet:
		case <-s.ctx.Done():
			close(packetChan)
			wg.Wait()
			return s.ctx.Err()
		}
	}

	close(packetChan) // Cerrar el canal después de enviar todos los paquetes.
	wg.Wait()         // Esperar a que la goroutine termine.

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
