package fastaudiosocket

import (
	"encoding/binary"
	"io"

	"github.com/google/uuid"
	"github.com/pkg/errors"
)

// Constants for message types.
const (
	KindHangup  = 0x00
	KindID      = 0x01
	KindSilence = 0x02
	KindSlin    = 0x10
	KindError   = 0xff

	MaxMessageSize = 65535 // Maximum payload size
	HeaderSize     = 3     // Size of the header
)

// Message represents an audiosocket message.
type Message struct {
	Data []byte
	Len  int // Actual length of the message
}

// NextMessage reads the next message from the connection.
func NextMessage(r io.Reader) (Message, error) {
	// Allocate a buffer just for the message
	buf := make([]byte, HeaderSize+MaxMessageSize)

	// Read the 3-byte header.
	if _, err := io.ReadFull(r, buf[:HeaderSize]); err != nil {
		return Message{}, errors.Wrap(err, "failed to read header")
	}

	// Extract the payload length.
	payloadLen := int(binary.BigEndian.Uint16(buf[1:3]))
	if payloadLen < 0 || payloadLen > MaxMessageSize {
		return Message{}, errors.New("invalid payload size")
	}

	totalLen := HeaderSize + payloadLen

	// Read the payload if present.
	if payloadLen > 0 {
		if _, err := io.ReadFull(r, buf[HeaderSize:totalLen]); err != nil {
			return Message{}, errors.Wrap(err, "failed to read payload")
		}
	}

	return Message{Data: buf[:totalLen], Len: totalLen}, nil
}

// ID extracts the UUID from an ID message.
func (m Message) ID() (uuid.UUID, error) {
	if m.Len < HeaderSize || m.Data[0] != KindID {
		return uuid.Nil, errors.New("invalid ID message")
	}
	return uuid.FromBytes(m.Data[HeaderSize:])
}

// Optimized for performance: reuse buffers and minimize allocations
func (m *Message) Reset() {
	// Optionally clear the data slice to release references
	if m.Len > 0 {
		m.Data = m.Data[:0] // Reset slice length but keep underlying array
		m.Len = 0           // Reset the length
	}
}

func SlinMessage(payload []byte) Message {
	msg := make([]byte, 3+len(payload))
	msg[0] = KindSlin
	binary.BigEndian.PutUint16(msg[1:3], uint16(len(payload)))
	copy(msg[3:], payload)
	return Message{Data: msg, Len: len(msg)}
}

func HangupMessage() Message {
	return Message{Data: []byte{KindHangup}, Len: 1}
}
