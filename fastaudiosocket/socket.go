package fastaudiosocket

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

const (
	PacketTypeHangup = 0x00
	PacketTypeUUID   = 0x01
	PacketTypeAudio  = 0x10
	PacketTypeError  = 0xff
	PacketTypeDTMF   = 0x03 // New packet type for DTMF
	WriteChunkSize   = 320  // PCM audio chunk size (20ms of audio)
	HeaderSize       = 3    // Header: Type (1 byte) + Length (2 bytes)
	MaxPacketSize    = 323
	chunksPerSecond  = 50
)

const (
	LenAudioPacketUlaw = 160
	TickerUlaw8khz     = 20 * time.Millisecond
)

var (
	onceSilentPacket sync.Once
	silentPacket     []byte
	writingHeader    = [3]byte{PacketTypeAudio, 0x01, 0x40}
)

type PacketWriter struct {
	Header  [HeaderSize]byte
	Payload []byte
}
type PacketReader struct {
	SilenceSuppressed bool   // Silence suppression flag to avoid sending silence packets
	Sequence          uint32 // Sequence number of the packet
	Type              byte   // Type of the packet
	Length            uint16 // Length of the payload
	Payload           []byte // Payload of the packet
}
type MonitorResponse struct {
	Message              string
	ChunkCounterReceived int32
	ExpectedChunks       int32
}

// getSilentPacket generates a silent audio packet to be used when no audio is received.
//
// Returns:
//   - A byte slice representing a silent audio packet.
func getSilentPacket() []byte {
	onceSilentPacket.Do(func() {
		silentPacket = make([]byte, MaxPacketSize)
		silentPacket[0] = PacketTypeAudio
		silentPacket[1] = 0x01
		silentPacket[2] = 0x40
		for i := 3; i < MaxPacketSize; i++ {
			silentPacket[i] = 0xff // set 255 as silent packet
		}
	})
	return silentPacket
}

type FastAudioSocket struct {
	callCtx      context.Context
	cancel       context.CancelFunc
	conn         net.Conn
	uuid         string
	PacketChan   chan PacketReader
	AudioChan    chan PacketReader
	MonitorChan  chan MonitorResponse
	chunkCounter int32
	debug        bool
}

// NewFastAudioSocket initializes a new FastAudioSocket instance.
//
// Parameters:
//   - ctx: The context for managing the socket's lifecycle.
//   - conn: The network connection for the audiosocket.
//   - debug: Enables debug logging if true.
//   - monitorEnabled: Enables monitoring if true.
//
// Returns:
//   - A pointer to the FastAudioSocket instance.
//   - An error if initialization fails.
func NewFastAudioSocket(ctx context.Context, conn net.Conn, debug bool, monitorEnabled bool) (*FastAudioSocket, error) {
	ctx, cancel := context.WithCancel(ctx)

	s := &FastAudioSocket{
		callCtx:      ctx,
		cancel:       cancel,
		conn:         conn,
		PacketChan:   make(chan PacketReader, 25),
		AudioChan:    make(chan PacketReader, 25),
		MonitorChan:  make(chan MonitorResponse, 25),
		chunkCounter: int32(0),
		debug:        debug,
	}

	uuid, err := s.readUUID()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to read UUID: %w", err)
	}
	s.uuid = uuid.String()

	var wg sync.WaitGroup
	wg.Add(1)
	go s.streamRead(&wg)
	if monitorEnabled {
		wg.Add(1)
		go s.monitor(&wg)
	}

	go func() {
		wg.Wait()
		if s.debug {
			fmt.Println("Closing FastAudioSocket...")
		}
		close(s.PacketChan)
		close(s.MonitorChan)
		s.conn.Close()
	}()

	return s, nil
}

// readUUID reads the UUID packet from the audiosocket connection.
//
// Returns:
//   - A UUID object representing the call's unique identifier.
//   - An error if the UUID cannot be read or is invalid.
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

// readChunk reads a single packet (audio or DTMF) from the audiosocket connection.
//
// Returns:
//   - A PacketReader object containing the packet's data.
//   - An error if the packet cannot be read.
func (s *FastAudioSocket) readChunk() (PacketReader, error) {
	header := make([]byte, HeaderSize)
	if _, err := s.conn.Read(header); err != nil {
		return PacketReader{Type: PacketTypeError}, fmt.Errorf("failed to read header: %w", err)
	}

	packetType := header[0]
	payloadLength := binary.BigEndian.Uint16(header[1:3])

	switch packetType {
	case PacketTypeAudio:
		payload := make([]byte, payloadLength)
		if _, err := s.conn.Read(payload); err != nil {
			return PacketReader{Type: packetType, Length: payloadLength}, fmt.Errorf("failed to read payload: %w", err)
		}

		if s.debug {
			fmt.Printf("<<< Received packet: Type=%#x, Length=%v\n", packetType, payloadLength)
		}

		return PacketReader{Type: packetType, Length: payloadLength, Payload: payload}, nil
	case PacketTypeDTMF:
		payload := make([]byte, 1) // DTMF payload is 1 byte
		if _, err := s.conn.Read(payload); err != nil {
			return PacketReader{Type: packetType, Length: payloadLength}, fmt.Errorf("failed to read DTMF payload: %w", err)
		}

		if s.debug {
			fmt.Printf("<<< Received DTMF packet: Digit=%c\n", payload[0])
		}

		return PacketReader{Type: packetType, Length: payloadLength, Payload: payload}, nil
	default:
		if s.debug {
			fmt.Printf("<<< Received unknown packet: Type=%#x, Length=%v\n", packetType, payloadLength)
		}
		return PacketReader{Type: PacketTypeError}, fmt.Errorf("unknown packet type: %#x", packetType)

	}

}

// streamRead continuously reads audio packets from the connection and sends them to PacketChan.
//
// Parameters:
//   - wg: A WaitGroup to signal when the function completes.
func (s *FastAudioSocket) streamRead(wg *sync.WaitGroup) {
	defer wg.Done()
	if s.debug {
		fmt.Println("-- StreamRead START --")
		defer fmt.Println("-- StreamRead STOP --")
	}

	defer s.cancel()

	go func() {
		// recover from panic if the chunk reader is closed
		defer func() {
			if r := recover(); r != nil {
				if s.debug {
					fmt.Printf("Recovered in streamRead %x", r)
				}
			}
		}()

		for {
			select {
			case <-s.callCtx.Done():
				return
			default:
				packet, err := s.readChunk()
				if err != nil {
					if s.debug {
						fmt.Printf("Failed to read packet: %v\n", err)
					}

					s.PacketChan <- PacketReader{Type: PacketTypeError}
					return
				}
				atomic.AddInt32(&s.chunkCounter, 1)

				s.PacketChan <- packet
			}
		}
	}()

	// Send silence packets if no audio packets are received
	// In some scenarios, silence suppression may be enabled
	// on the other side, so we need to send silence packets
	// to ensure that a package is received every 20ms
	seqNumber := uint32(0)
	chunkTicker := time.NewTicker(TickerUlaw8khz)
	defer chunkTicker.Stop()
	lastPacketReceived := true
	for {
		select {
		case <-s.callCtx.Done():
			return
		case <-chunkTicker.C:
			if !lastPacketReceived {
				s.AudioChan <- PacketReader{Sequence: seqNumber, SilenceSuppressed: true}
				seqNumber++
			}
			lastPacketReceived = false
		case p, ok := <-s.PacketChan:
			if !ok {
				return
			}
			p.Sequence = seqNumber
			s.AudioChan <- p
			lastPacketReceived = true
			seqNumber++
		}
	}
}

// monitor periodically checks the connection's health and sends status updates to MonitorChan.
//
// Parameters:
//   - wg: A WaitGroup to signal when the function completes.
func (s *FastAudioSocket) monitor(wg *sync.WaitGroup) {
	defer wg.Done()
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
		case <-s.callCtx.Done():
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

// toBytes serializes a PacketWriter into a byte slice.
//
// Returns:
//   - A byte slice representing the serialized packet.
func (p *PacketWriter) toBytes() []byte {
	packetBuffer := make([]byte, MaxPacketSize)
	copy(packetBuffer[:HeaderSize], p.Header[:])
	copy(packetBuffer[HeaderSize:], p.Payload)
	return packetBuffer[:MaxPacketSize]
}

// sendPacket sends a serialized packet to the audiosocket connection.
//
// Parameters:
//   - packet: The PacketWriter object to be sent.
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

// Play streams audio data to the audiosocket in 20ms chunks.
//
// Parameters:
//   - playerCtx: The context for controlling the playback.
//   - audioData: The byte slice containing the audio data.
//
// Returns:
//   - An error if playback fails.
func (s *FastAudioSocket) Play(playerCtx context.Context, audioData []byte) error {
	if s.debug {
		fmt.Println("-- Play START --")
		defer fmt.Println("-- Play STOP --")
	}

	if len(audioData) < MaxPacketSize {
		return fmt.Errorf("audio data is too short: received %d bytes, need at least %d", len(audioData), MaxPacketSize)
	}

	packetChan := make(chan PacketWriter, 10)
	errorChan := make(chan error, 1)

	go func() {
		defer close(packetChan)
		for i := 0; i < len(audioData); i += WriteChunkSize {
			end := i + WriteChunkSize
			if end > len(audioData) {
				end = len(audioData)
			}

			chunk := padChunkWithSilence(audioData[i:end])

			if bytes.Equal(chunk, getSilentPacket()) {
				continue
			}

			packet := newPCM8khzPacket(chunk)

			select {
			case packetChan <- packet:
			case <-playerCtx.Done():
				errorChan <- playerCtx.Err()
				return
			case <-s.callCtx.Done():
				errorChan <- s.callCtx.Err()
				return
			}
		}
	}()

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-playerCtx.Done():
			return playerCtx.Err()
		case <-s.callCtx.Done():
			return s.callCtx.Err()
		case err := <-errorChan:
			if err != nil {
				return err
			}
		case <-ticker.C:
			packet, ok := <-packetChan
			if !ok {
				return nil
			}
			s.sendPacket(packet)
		}
	}
}

// PlayStreaming streams audio data from a channel to the audiosocket.
//
// Parameters:
//   - playerCtx: The context for controlling the playback.
//   - dataChan: A channel providing audio data in chunks.
//   - errChan: A channel for reporting errors.
//
// Returns:
//   - An error if playback fails.
func (s *FastAudioSocket) PlayStreaming(playerCtx context.Context, dataChan chan []byte, errChan chan error) error {
	if s.debug {
		fmt.Println("-- PlayStreaming START --")
		defer fmt.Println("-- PlayStreaming STOP --")
	}

	packetChan := make(chan PacketWriter, 10)

	go func() {
		defer close(packetChan)
		for audioData := range dataChan {
			if len(audioData) < MaxPacketSize {
				errChan <- fmt.Errorf("audio data is too short: received %d bytes, need at least %d", len(audioData), MaxPacketSize)
				return
			}

			for i := 0; i < len(audioData); i += WriteChunkSize {
				end := i + WriteChunkSize
				if end > len(audioData) {
					end = len(audioData)
				}

				chunk := padChunkWithSilence(audioData[i:end])

				if bytes.Equal(chunk, getSilentPacket()) {
					continue
				}

				packet := newPCM8khzPacket(chunk)

				select {
				case packetChan <- packet:
				case <-playerCtx.Done():
					errChan <- playerCtx.Err()
					return
				case <-s.callCtx.Done():
					errChan <- s.callCtx.Err()
					return
				}
			}
		}
	}()

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-playerCtx.Done():
			return playerCtx.Err()
		case <-s.callCtx.Done():
			return s.callCtx.Err()
		case err := <-errChan:
			if err != nil {
				return err
			}
		case <-ticker.C:
			packet, ok := <-packetChan
			if !ok {
				return nil
			}
			s.sendPacket(packet)
		}
	}
}

// padChunkWithSilence pads an audio chunk with silence to match the required size.
//
// Parameters:
//   - chunk: The audio chunk to be padded.
//
// Returns:
//   - A byte slice containing the padded audio chunk.
func padChunkWithSilence(chunk []byte) []byte {
	if len(chunk) < WriteChunkSize {
		silence := getSilentPacket()[:WriteChunkSize-len(chunk)]
		return append(chunk, silence...)
	}
	return chunk
}

// newPCM8khzPacket creates a new PCM audio packet with the given chunk.
//
// Parameters:
//   - chunk: The audio chunk to be included in the packet.
//
// Returns:
//   - A PacketWriter object representing the audio packet.
func newPCM8khzPacket(chunk []byte) PacketWriter {
	return PacketWriter{Header: writingHeader, Payload: chunk}
}

// GetUUID retrieves the UUID of the current audiosocket session.
//
// Returns:
//   - A string representing the UUID.
func (s *FastAudioSocket) GetUUID() string {
	return s.uuid
}

// Hangup sends a termination packet to the audiosocket connection.
//
// Returns:
//   - An error if the termination packet cannot be sent.
func (s *FastAudioSocket) Hangup() error {
	command := []byte{PacketTypeHangup, 0x00, 0x00}
	if _, err := s.conn.Write(command); err != nil {
		return fmt.Errorf("failed to send termination packet: %w", err)
	}
	return nil
}
