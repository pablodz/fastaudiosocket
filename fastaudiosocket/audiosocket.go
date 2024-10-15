package fastaudiosocket

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/google/uuid"
)

const (
	KindHangup  = 0x00
	KindID      = 0x01
	KindSilence = 0x02
	KindSlin    = 0x10
	KindError   = 0xff

	MaxMessageSize = 320
	HeaderSize     = 3
)

var (
	ErrInvalidHeader   = errors.New("failed to read header")
	ErrPayloadTooLarge = fmt.Errorf("payload size exceeds maximum of %d bytes", MaxMessageSize)
	ErrInvalidPayload  = errors.New("failed to read payload")
	ErrInvalidIDMsg    = errors.New("invalid ID message")
)

type Message struct {
	Data        []byte
	Len         int
	MessageType byte
}

var bufferPool = sync.Pool{
	New: func() any {
		return &[HeaderSize + MaxMessageSize]byte{}
	},
}

func NextMessage(r io.Reader) (Message, error) {
	buf := bufferPool.Get().(*[HeaderSize + MaxMessageSize]byte)

	if _, err := io.ReadFull(r, buf[:HeaderSize]); err != nil {
		return Message{}, fmt.Errorf("%w: %v", ErrInvalidHeader, err)
	}

	payloadLen := int(binary.BigEndian.Uint16(buf[1:3]))
	if payloadLen > MaxMessageSize {
		return Message{}, ErrPayloadTooLarge
	}

	totalLen := HeaderSize + payloadLen
	if payloadLen > 0 {
		if _, err := io.ReadFull(r, buf[HeaderSize:totalLen]); err != nil {
			return Message{}, fmt.Errorf("%w: %v", ErrInvalidPayload, err)
		}
	}

	return Message{
		Data:        buf[:totalLen],
		Len:         totalLen,
		MessageType: buf[0],
	}, nil
}

func (m Message) ID() (uuid.UUID, error) {
	if m.Len < HeaderSize || m.MessageType != KindID {
		return uuid.Nil, ErrInvalidIDMsg
	}
	return uuid.FromBytes(m.Data[HeaderSize:])
}

func (m *Message) Reset() {
	m.Data = m.Data[:0]
	m.Len = 0
	m.MessageType = 0
}

func SlinMessage(payload []byte) Message {
	if len(payload) > MaxMessageSize {
		panic("payload exceeds maximum allowed size")
	}

	msg := make([]byte, HeaderSize+len(payload))
	msg[0] = KindSlin
	binary.BigEndian.PutUint16(msg[1:3], uint16(len(payload)))
	copy(msg[3:], payload)

	return Message{
		Data:        msg,
		Len:         len(msg),
		MessageType: KindSlin,
	}
}

func HangupMessage() Message {
	return Message{
		Data:        []byte{KindHangup},
		Len:         HeaderSize - 2,
		MessageType: KindHangup,
	}
}
