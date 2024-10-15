package fastaudiosocket

import (
	"bytes"
	// Adjust the import path accordingly
)

func createSampleMessages() (*bytes.Reader, error) {
	var buf bytes.Buffer

	// Create a sample message.
	payload := make([]byte, 320) // Example payload of 320 bytes.
	msg := SlinMessage(payload)

	// Write the message to the buffer multiple times.
	for i := 0; i < 1000; i++ {
		buf.Write(msg.Data)
	}

	return bytes.NewReader(buf.Bytes()), nil
}
