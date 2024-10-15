package fastaudiosocket

import (
	"bytes"
	"sync"
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

func BenchmarkConcurrentNextMessage(b *testing.B) {
	rFastaudio, err := createSampleMessages()
	if err != nil {
		b.Fatal(err)
	}

	rAudio, err := createSampleMessages()
	if err != nil {
		b.Fatal(err)
	}

	var msg Message // Declare a variable to hold the message.
	var wg sync.WaitGroup
	wg.Add(2) // We will have two concurrent benchmarks

	b.Run("Concurrent Fastaudio and Audio NextMessage", func(b *testing.B) {
		b.ResetTimer() // Reset timer before the actual benchmarking.

		for i := 0; i < b.N; i++ {
			// Use goroutines to read messages concurrently
			wg.Add(2) // Two goroutines to wait for
			go func() {
				defer wg.Done() // Mark this goroutine as done
				var err error
				msg, err = NextMessage(rFastaudio)
				if err != nil {
					b.Fatal(err) // Fail the benchmark if there's an error.
				}
				msg.Reset()           // Clear the message after processing.
				rFastaudio.Seek(0, 0) // Reset the reader for the next iteration.
			}()

			go func() {
				defer wg.Done() // Mark this goroutine as done
				_, err := audiosocket.NextMessage(rAudio)
				if err != nil {
					b.Fatal(err) // Fail the benchmark if there's an error.
				}
				rAudio.Seek(0, 0) // Reset the reader for the next iteration.
			}()

			wg.Wait() // Wait for both goroutines to finish
		}
	})
}
