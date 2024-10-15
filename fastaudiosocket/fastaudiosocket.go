package fastaudiosocket

import (
	"encoding/binary"
	"errors"
	"io"

	"github.com/google/uuid"
)

// Constants for message types.
const (
	KindHangup  = 0x00
	KindID      = 0x01
	KindSilence = 0x02
	KindSlin    = 0x10
	KindError   = 0xff

	MaxMessageSize   = 320                         // Maximum payload size
	HeaderSize       = 3                           // Size of the header
	TotalMessageSize = HeaderSize + MaxMessageSize // Total size of the message
)

// Message represents an audiosocket message.
type Message struct {
	Data        []byte
	Len         int  // Actual length of the message
	MessageType byte // Type of the message
}

// NextMessage reads the next message from the connection.
func NextMessage(r io.Reader) (Message, error) {
	// Allocate a buffer just for the message
	buf := make([]byte, TotalMessageSize)

	// Read the 3-byte header.
	if _, err := io.ReadFull(r, buf[:HeaderSize]); err != nil {
		return Message{}, errors.New("failed to read header")
	}

	// Extract the payload length (ensure it's always 320)
	payloadLen := int(binary.BigEndian.Uint16(buf[1:3]))
	if payloadLen != MaxMessageSize {
		return Message{}, errors.New("invalid payload size, must be 320 bytes")
	}

	// Read the payload (always 320 bytes)
	if _, err := io.ReadFull(r, buf[HeaderSize:]); err != nil {
		return Message{}, errors.New("failed to read payload")
	}

	// Create a Message and set the type based on the first byte of the data
	messageType := buf[0]
	return Message{Data: buf, Len: TotalMessageSize, MessageType: messageType}, nil
}

// ID extracts the UUID from an ID message.
func (m Message) ID() (uuid.UUID, error) {
	if m.Len < HeaderSize || m.MessageType != KindID {
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
		m.MessageType = 0   // Reset the message type
	}
}

func SlinMessage(payload []byte) Message {
	msg := make([]byte, TotalMessageSize)
	msg[0] = KindSlin
	binary.BigEndian.PutUint16(msg[1:3], uint16(MaxMessageSize)) // Always 320
	copy(msg[3:], payload)

	// Pad with zeros if the payload is less than 320 bytes
	for i := len(payload) + 3; i < TotalMessageSize; i++ {
		msg[i] = 0
	}

	return Message{Data: msg, Len: TotalMessageSize, MessageType: KindSlin}
}

func HangupMessage() Message {
	msg := make([]byte, TotalMessageSize)
	msg[0] = KindHangup
	// No payload to set, rest will be padded with zeros
	return Message{Data: msg, Len: TotalMessageSize, MessageType: KindHangup}
}
