package fastaudiosocket

import (
	"bytes"
	"testing"

	"github.com/pablodz/audiosocket" // Adjust the import path accordingly
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

// BenchmarkNextMessage compares the NextMessage function of fastaudiosocket and audiosocket.
func BenchmarkNextMessage(b *testing.B) {
	rFastaudio, err := createSampleMessages()
	if err != nil {
		b.Fatal(err)
	}

	rAudio, err := createSampleMessages()
	if err != nil {
		b.Fatal(err)
	}

	var msg Message // Declare a variable to hold the message.

	b.Run("Fastaudio NextMessage", func(b *testing.B) {
		b.ResetTimer() // Reset timer before the actual benchmarking.
		for i := 0; i < b.N; i++ {
			var err error
			msg, err = NextMessage(rFastaudio)
			if err != nil {
				b.Fatal(err) // Fail the benchmark if there's an error.
			}
			msg.Reset()           // Clear the message after processing.
			rFastaudio.Seek(0, 0) // Reset the reader for the next iteration.
		}
	})

	b.Run("Audio NextMessage", func(b *testing.B) {
		b.ResetTimer() // Reset timer before the actual benchmarking.
		for i := 0; i < b.N; i++ {
			_, err := audiosocket.NextMessage(rAudio)
			if err != nil {
				b.Fatal(err) // Fail the benchmark if there's an error.
			}
			rAudio.Seek(0, 0) // Reset the reader for the next iteration.
		}
	})
}
