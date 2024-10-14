package fastaudiosocket

import (
	"encoding/binary"
	"io"
	"sync"

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
)

// Buffer pool to minimize allocations.
var bufferPool = sync.Pool{
	New: func() interface{} {
		return make([]byte, MaxMessageSize+3) // 3-byte header + max payload size
	},
}

// Message represents an audiosocket message.
type Message []byte

// NextMessage reads the next message from the connection.
func NextMessage(r io.Reader) (Message, error) {
	// Get a buffer from the pool.
	buf := bufferPool.Get().([]byte)
	defer bufferPool.Put(buf)

	// Read the 3-byte header.
	if _, err := io.ReadFull(r, buf[:3]); err != nil {
		return nil, errors.Wrap(err, "failed to read header")
	}

	// Extract the payload length.
	payloadLen := binary.BigEndian.Uint16(buf[1:3])
	totalLen := int(3 + payloadLen)

	// Read the payload if present.
	if payloadLen > 0 {
		if _, err := io.ReadFull(r, buf[3:totalLen]); err != nil {
			return nil, errors.Wrap(err, "failed to read payload")
		}
	}

	return buf[:totalLen], nil
}

// ID extracts the UUID from an ID message.
func (m Message) ID() (uuid.UUID, error) {
	if len(m) < 3 || m[0] != KindID {
		return uuid.Nil, errors.New("invalid ID message")
	}
	return uuid.FromBytes(m[3:])
}
