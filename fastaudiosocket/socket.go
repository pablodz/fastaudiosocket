package fastaudiosocket

import (
	"context"
	"encoding/binary"
	"errors"
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
	PacketTypeDTMF   = 0x03

	// Write configuration (Sending PCM 16-bit, 8kHz)
	WriteChunkSize = 320 // 20ms of 16-bit PCM
	HeaderSize     = 3
	MaxPacketSize  = HeaderSize + WriteChunkSize

	// Read configuration (Receiving usually U-law or PCM)
	TickerInterval = 20 * time.Millisecond
)

var (
	ErrFailedToReadUUID = errors.New("failed to read UUID")
)

var (
	// Pre-allocated header for audio packets (Type 0x10, Length 320)
	writingHeader = [3]byte{PacketTypeAudio, 0x01, 0x40}

	// Pre-allocated silence payload (all zeros for 16-bit PCM)
	silencePayload = make([]byte, WriteChunkSize)
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

// NewFastAudioSocket initializes a new FastAudioSocket instance, performs the UUID handshake, and starts background listeners.
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

	uuidObj, err := s.readUUID()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%w: %w", ErrFailedToReadUUID, err)
	}
	s.uuid = uuidObj.String()

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
			fmt.Println("Closing FastAudioSocket resources...")
		}
		close(s.PacketChan)
		close(s.MonitorChan)
		s.conn.Close()
	}()

	return s, nil
}

// readUUID reads the initial handshake packet containing the call UUID.
func (s *FastAudioSocket) readUUID() (uuid.UUID, error) {
	header := make([]byte, HeaderSize)
	if _, err := s.conn.Read(header); err != nil {
		return uuid.Nil, err
	}

	packetType := header[0]
	payloadLength := binary.BigEndian.Uint16(header[1:3])

	if packetType != PacketTypeUUID {
		return uuid.Nil, fmt.Errorf("expected UUID packet, got %#x", packetType)
	}

	if payloadLength != 16 {
		return uuid.Nil, fmt.Errorf("invalid UUID payload length: %d", payloadLength)
	}

	payload := make([]byte, payloadLength)
	if _, err := s.conn.Read(payload); err != nil {
		return uuid.Nil, err
	}

	if s.debug {
		fmt.Printf("<<< Received UUID packet: %x\n", payload)
	}

	return uuid.FromBytes(payload)
}

// readChunk reads a single frame from the socket, handling variable payload lengths dynamically.
func (s *FastAudioSocket) readChunk() (PacketReader, error) {
	header := make([]byte, HeaderSize)
	if _, err := s.conn.Read(header); err != nil {
		return PacketReader{Type: PacketTypeError}, err
	}

	packetType := header[0]
	payloadLength := binary.BigEndian.Uint16(header[1:3])
	payload := make([]byte, payloadLength)

	if _, err := s.conn.Read(payload); err != nil {
		return PacketReader{Type: packetType, Length: payloadLength}, err
	}

	if s.debug && packetType != PacketTypeAudio {
		fmt.Printf("<<< Received packet: Type=%#x, Len=%d\n", packetType, payloadLength)
	}

	return PacketReader{Type: packetType, Length: payloadLength, Payload: payload}, nil
}

// streamRead continuously pulls data from the socket and manages the silence suppression logic to ensure a steady stream on AudioChan.
func (s *FastAudioSocket) streamRead(wg *sync.WaitGroup) {
	defer wg.Done()
	defer s.cancel()

	if s.debug {
		fmt.Println("-- StreamRead START --")
		defer fmt.Println("-- StreamRead STOP --")
	}

	go func() {
		defer func() {
			if r := recover(); r != nil && s.debug {
				fmt.Printf("Recovered in streamRead: %v\n", r)
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
						fmt.Printf("Read error: %v\n", err)
					}
					// Avoid sending error packet if context is already done
					if s.callCtx.Err() == nil {
						s.PacketChan <- PacketReader{Type: PacketTypeError}
					}
					return
				}
				atomic.AddInt32(&s.chunkCounter, 1)
				s.PacketChan <- packet
			}
		}
	}()

	seqNumber := uint32(0)
	chunkTicker := time.NewTicker(TickerInterval)
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
			if p.Type == PacketTypeError {
				return
			}
			p.Sequence = seqNumber
			s.AudioChan <- p
			lastPacketReceived = true
			seqNumber++
		}
	}
}

// monitor checks packet flow statistics and reports anomalies.
func (s *FastAudioSocket) monitor(wg *sync.WaitGroup) {
	defer wg.Done()

	monitorInterval := 2 * time.Second
	ticker := time.NewTicker(monitorInterval)
	defer ticker.Stop()

	chunksPerSec := int32(1000 / TickerInterval.Milliseconds()) // 50
	lastCounter := int32(0)
	chunksExpected := int32(float64(chunksPerSec) * monitorInterval.Seconds())
	minimalIntermitentChunks := int32(float64(chunksExpected) * 0.5)

	for {
		select {
		case <-s.callCtx.Done():
			return
		case <-ticker.C:
			currentCounter := atomic.LoadInt32(&s.chunkCounter)
			chunksReceived := currentCounter - lastCounter
			lastCounter = currentCounter

			var msg string
			switch {
			case chunksReceived == chunksExpected:
				msg = "Monitor: ✅ Perfect stream"
			case chunksReceived == 0:
				msg = "Monitor: 🚨 No audio"
			case chunksReceived < minimalIntermitentChunks:
				msg = "Monitor: 🚨 Poor quality (Intermittent)"
			case chunksReceived > chunksExpected:
				msg = "Monitor: ⚡ Fast stream (Clock drift?)"
			default:
				// Near expected, tolerable
				continue
			}

			if msg != "" {
				s.MonitorChan <- MonitorResponse{
					Message:              msg,
					ChunkCounterReceived: chunksReceived,
					ExpectedChunks:       chunksExpected,
				}
			}
		}
	}
}

// toBytes serializes the packet efficiently using append to avoid unnecessary zeroing.
func (p *PacketWriter) toBytes() []byte {
	buf := make([]byte, 0, HeaderSize+len(p.Payload))
	buf = append(buf, p.Header[:]...)
	buf = append(buf, p.Payload...)
	return buf
}

// sendPacket writes data to the connection, ignoring broken pipe errors common in hangup scenarios.
func (s *FastAudioSocket) sendPacket(packet PacketWriter) {
	serialized := packet.toBytes()
	if _, err := s.conn.Write(serialized); err != nil {
		if strings.Contains(err.Error(), "broken pipe") || strings.Contains(err.Error(), "connection reset") {
			return
		}
		if s.debug {
			fmt.Printf("Tx Error: %v\n", err)
		}
	}
}

// Play streams audio data synchronously using a ticker, avoiding unnecessary goroutines.
func (s *FastAudioSocket) Play(playerCtx context.Context, audioData []byte) error {
	if s.debug {
		fmt.Println("-- Play START --")
		defer fmt.Println("-- Play STOP --")
	}

	if len(audioData) == 0 {
		return nil
	}

	ticker := time.NewTicker(TickerInterval)
	defer ticker.Stop()

	for i := 0; i < len(audioData); i += WriteChunkSize {
		end := i + WriteChunkSize
		var chunk []byte

		if end > len(audioData) {
			chunk = padChunkWithSilence(audioData[i:])
		} else {
			chunk = audioData[i:end]
		}

		// Skip sending if it's pure silence (optional optimization, depends on requirements)
		// For keep-alive purposes, usually better to send it.

		select {
		case <-playerCtx.Done():
			return playerCtx.Err()
		case <-s.callCtx.Done():
			return s.callCtx.Err()
		case <-ticker.C:
			s.sendPacket(PacketWriter{
				Header:  writingHeader,
				Payload: chunk,
			})
		}
	}
	return nil
}

// PlayStreaming reads from a data channel and streams to the socket.
func (s *FastAudioSocket) PlayStreaming(playerCtx context.Context, dataChan chan []byte, errChan chan error) error {
	if s.debug {
		fmt.Println("-- PlayStreaming START --")
		defer fmt.Println("-- PlayStreaming STOP --")
	}

	ticker := time.NewTicker(TickerInterval)
	defer ticker.Stop()

	for {
		select {
		case <-playerCtx.Done():
			return playerCtx.Err()
		case <-s.callCtx.Done():
			return s.callCtx.Err()
		case audioChunk, ok := <-dataChan:
			if !ok {
				return nil // Channel closed, playback finished
			}

			// Handle chunks larger than WriteChunkSize by splitting them
			for i := 0; i < len(audioChunk); i += WriteChunkSize {
				end := i + WriteChunkSize
				var finalChunk []byte

				if end > len(audioChunk) {
					finalChunk = padChunkWithSilence(audioChunk[i:])
				} else {
					finalChunk = audioChunk[i:end]
				}

				select {
				case <-playerCtx.Done():
					return playerCtx.Err()
				case <-s.callCtx.Done():
					return s.callCtx.Err()
				case <-ticker.C:
					s.sendPacket(PacketWriter{
						Header:  writingHeader,
						Payload: finalChunk,
					})
				}
			}
		}
	}
}

// padChunkWithSilence appends the silence payload to the chunk to reach the required block size.
func padChunkWithSilence(chunk []byte) []byte {
	length := len(chunk)
	if length == WriteChunkSize {
		return chunk
	}
	if length < WriteChunkSize {
		// Use pre-allocated silence buffer to avoid allocation
		return append(chunk, silencePayload[:WriteChunkSize-length]...)
	}
	return chunk[:WriteChunkSize]
}

// GetUUID returns the call session UUID.
func (s *FastAudioSocket) GetUUID() string {
	return s.uuid
}

// Hangup sends the hangup command.
func (s *FastAudioSocket) Hangup() error {
	command := []byte{PacketTypeHangup, 0x00, 0x00}
	_, err := s.conn.Write(command)
	return err
}
