package fastaudiosocket

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
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
	callCtx         context.Context
	cancel          context.CancelFunc
	conn            net.Conn
	readHeader      [HeaderSize]byte
	writeFrame      [MaxPacketSize]byte
	uuid            string
	PacketChan      chan PacketReader
	AudioChan       chan PacketReader
	MonitorChan     chan MonitorResponse
	chunkCounter    int32
	debug           bool
	playbackMu      sync.Mutex
	playbackStateMu sync.Mutex
	playbackOptions PlaybackOptions
	playbackStats   PlaybackStats
	playoutEnd      time.Time
}

type PlaybackControl interface {
	Wait(context.Context) error
	// Played reports audio successfully handed to the socket, including tail
	// padding. It does not report wall time or confirmed audible playback.
	Played(time.Duration)
}

// NewFastAudioSocket initializes a new FastAudioSocket instance, performs the UUID handshake, and starts background listeners.
func NewFastAudioSocket(ctx context.Context, conn net.Conn, debug bool, monitorEnabled bool) (*FastAudioSocket, error) {
	ctx, cancel := context.WithCancel(ctx)
	// Closing the owned connection unblocks handshake and reader on cancellation.
	context.AfterFunc(ctx, func() { _ = conn.Close() })

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
		close(s.AudioChan)
		close(s.MonitorChan)
		s.conn.Close()
	}()

	return s, nil
}

// readUUID reads the initial handshake packet containing the call UUID.
func (s *FastAudioSocket) readUUID() (uuid.UUID, error) {
	header := s.readHeader[:]
	if _, err := io.ReadFull(s.conn, header); err != nil {
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
	if _, err := io.ReadFull(s.conn, payload); err != nil {
		return uuid.Nil, err
	}

	if s.debug {
		fmt.Printf("<<< Received UUID packet: %x\n", payload)
	}

	return uuid.FromBytes(payload)
}

// readChunk reads a single frame from the socket, handling variable payload lengths dynamically.
func (s *FastAudioSocket) readChunk() (PacketReader, error) {
	header := s.readHeader[:]
	if _, err := io.ReadFull(s.conn, header); err != nil {
		return PacketReader{Type: PacketTypeError}, err
	}

	packetType := header[0]
	payloadLength := binary.BigEndian.Uint16(header[1:3])
	payload := make([]byte, payloadLength)

	if _, err := io.ReadFull(s.conn, payload); err != nil {
		return PacketReader{Type: packetType, Length: payloadLength}, err
	}

	if s.debug && packetType != PacketTypeAudio {
		fmt.Printf("<<< Received packet: Type=%#x, Len=%d\n", packetType, payloadLength)
	}

	return PacketReader{Type: packetType, Length: payloadLength, Payload: payload}, nil
}

// streamRead continuously pulls data from the socket and manages the silence suppression logic.
func (s *FastAudioSocket) streamRead(wg *sync.WaitGroup) {
	defer wg.Done()
	defer s.cancel()

	if s.debug {
		fmt.Println("-- StreamRead START --")
		defer fmt.Println("-- StreamRead STOP --")
	}

	// The reader owns PacketChan. Join it before closing public output channels.
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		defer close(s.PacketChan)
		for {
			packet, err := s.readChunk()
			if err != nil {
				packet = PacketReader{Type: PacketTypeError}
			} else {
				atomic.AddInt32(&s.chunkCounter, 1)
			}
			select {
			case <-s.callCtx.Done():
				return
			case s.PacketChan <- packet:
			}
			if err != nil || packet.Type == PacketTypeHangup || packet.Type == PacketTypeError {
				return
			}
		}
	}()
	defer func() {
		s.cancel()
		s.conn.Close()
		<-readerDone
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
				select {
				case <-s.callCtx.Done():
					return
				case s.AudioChan <- PacketReader{Sequence: seqNumber, SilenceSuppressed: true}:
				}
				seqNumber++
			}
			lastPacketReceived = false
		case p, ok := <-s.PacketChan:
			if !ok {
				return
			}

			p.Sequence = seqNumber

			// Deliver terminal frames before closing AudioChan.
			select {
			case <-s.callCtx.Done():
				return
			case s.AudioChan <- p:
			}

			if p.Type == PacketTypeError || p.Type == PacketTypeHangup {
				return
			}

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
				select {
				case <-s.callCtx.Done():
					return
				case s.MonitorChan <- MonitorResponse{
					Message: msg, ChunkCounterReceived: chunksReceived, ExpectedChunks: chunksExpected,
				}:
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

// sendPacket reports failed and short writes; callers must not count them as audio.
func (s *FastAudioSocket) sendPacket(packet PacketWriter) error {
	// Playback owns playbackMu until the write completes. net.Conn.Write must
	// not retain this slice, so the next frame can reuse it after returning.
	var serialized []byte
	if len(packet.Payload) <= WriteChunkSize {
		serialized = s.writeFrame[:HeaderSize+len(packet.Payload)]
		copy(serialized, packet.Header[:])
		copy(serialized[HeaderSize:], packet.Payload)
	} else {
		serialized = packet.toBytes()
	}
	n, err := s.conn.Write(serialized)
	if n > 0 && n < len(serialized) {
		// A partly written frame cannot be replaced by a new playback frame.
		// Close instead of leaving the next caller with a corrupt byte stream.
		_ = s.conn.Close()
	}
	if err != nil {
		return err
	}
	if n != len(serialized) {
		return io.ErrShortWrite
	}
	return nil
}

// Play sends raw 8 kHz mono PCM16 audio synchronously with deadline pacing.
func (s *FastAudioSocket) Play(playerCtx context.Context, audioData []byte) error {
	return s.PlayControlled(playerCtx, audioData, nil)
}

func (s *FastAudioSocket) PlayControlled(playerCtx context.Context, audioData []byte, control PlaybackControl) error {
	if s.debug {
		fmt.Println("-- Play START --")
		defer fmt.Println("-- Play STOP --")
	}

	if len(audioData) == 0 {
		return nil
	}

	pace, err := s.beginPlayback()
	if err != nil {
		return err
	}
	defer s.finishPlayback(pace)
	waitCtx, stopWaiting := s.playbackContext(playerCtx)
	defer stopWaiting()

	for i := 0; i < len(audioData); i += WriteChunkSize {
		if control != nil {
			if err := control.Wait(waitCtx); err != nil {
				return err
			}
		}
		end := i + WriteChunkSize
		var chunk []byte

		if end > len(audioData) {
			chunk = padChunkWithSilence(audioData[i:])
		} else {
			chunk = audioData[i:end]
		}

		if err := pace.wait(playerCtx, s.callCtx); err != nil {
			return err
		}

		err := s.sendPacket(PacketWriter{
			Header:  writingHeader,
			Payload: chunk,
		})
		s.recordWrite(pace, time.Now(), err)
		if err != nil {
			if ctxErr := pendingContextError(playerCtx, s.callCtx); ctxErr != nil {
				return ctxErr
			}
			return err
		}

		if control != nil {
			control.Played(TickerInterval)
		}
	}
	return nil
}

// PlayStreaming reads from a data channel and streams it to the socket as a
// single realigned byte stream. The caller must close dataChan to flush the tail.
// errChan is retained for source compatibility and is unused; errors are returned.
func (s *FastAudioSocket) PlayStreaming(playerCtx context.Context, dataChan chan []byte, errChan chan error) error {
	if s.debug {
		fmt.Println("-- PlayStreaming START --")
		defer fmt.Println("-- PlayStreaming STOP --")
	}

	pace, err := s.beginPlayback()
	if err != nil {
		return err
	}
	defer s.finishPlayback(pace)
	_, stopWaiting := s.playbackContext(playerCtx)
	defer stopWaiting()
	var carry []byte

	for {
		select {
		case <-playerCtx.Done():
			return playerCtx.Err()
		case <-s.callCtx.Done():
			return s.callCtx.Err()
		case audioChunk, ok := <-dataChan:
			if !ok {
				return s.flushTail(playerCtx, pace, carry)
			}

			carry = append(carry, audioChunk...)

			for len(carry) >= WriteChunkSize {
				if err := pace.wait(playerCtx, s.callCtx); err != nil {
					return err
				}

				err := s.sendPacket(PacketWriter{
					Header:  writingHeader,
					Payload: carry[:WriteChunkSize],
				})
				s.recordWrite(pace, time.Now(), err)
				if err != nil {
					if ctxErr := pendingContextError(playerCtx, s.callCtx); ctxErr != nil {
						return ctxErr
					}
					return err
				}

				carry = carry[WriteChunkSize:]
			}
		}
	}
}

func (s *FastAudioSocket) flushTail(playerCtx context.Context, pace *pacer, carry []byte) error {
	if len(carry) == 0 {
		return nil
	}

	if err := pace.wait(playerCtx, s.callCtx); err != nil {
		return err
	}

	err := s.sendPacket(PacketWriter{
		Header:  writingHeader,
		Payload: padChunkWithSilence(carry),
	})
	s.recordWrite(pace, time.Now(), err)
	if err != nil {
		if ctxErr := pendingContextError(playerCtx, s.callCtx); ctxErr != nil {
			return ctxErr
		}
	}
	return err
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
