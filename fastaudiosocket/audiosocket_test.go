package fastaudiosocket

// import (
// 	"bytes"
// 	"encoding/binary"
// 	"errors"
// 	"io"
// 	"sync"
// 	"testing"

// 	"github.com/google/uuid"
// 	"github.com/pablodz/audiosocket"
// )

// // Helper function to create a message with the specified kind and payload.
// func createMessage(kind byte, payload []byte) []byte {
// 	payloadLen := len(payload)
// 	msg := make([]byte, 3+payloadLen)
// 	msg[0] = kind
// 	binary.BigEndian.PutUint16(msg[1:3], uint16(payloadLen))
// 	copy(msg[3:], payload)
// 	return msg
// }

// // Benchmark for comparing NextMessage and NextReader functions.
// func BenchmarkMessageReading(b *testing.B) {
// 	// Create a buffer to simulate a connection with multiple messages.
// 	var buffer bytes.Buffer

// 	// Populate the buffer with some test messages.
// 	for i := 0; i < 1000; i++ {
// 		// Create a UUID and its byte representation for the ID message.
// 		id := uuid.New()
// 		message := createMessage(KindSlin, id[:])
// 		buffer.Write(message)
// 	}

// 	// Benchmark for NextMessage from audiosocket.
// 	b.Run("AudiosocketOne", func(b *testing.B) {
// 		b.ResetTimer()
// 		for i := 0; i < b.N; i++ {
// 			// Reset the buffer reader for each iteration.
// 			reader := bytes.NewReader(buffer.Bytes())
// 			for {
// 				_, err := audiosocket.NextMessage(reader)
// 				if err != nil {
// 					if errors.Is(err, io.EOF) {
// 						break
// 					}
// 					b.Fatal(err) // Fail the benchmark on error
// 				}
// 			}
// 		}
// 	})

// 	// Benchmark for NextReader (assumed to be defined elsewhere).
// 	b.Run("FastAudiosocketOne", func(b *testing.B) {
// 		b.ResetTimer()
// 		for i := 0; i < b.N; i++ {
// 			// Reset the buffer reader for each iteration.
// 			reader := bytes.NewReader(buffer.Bytes())
// 			for {
// 				_, err := NextMessage(reader) // Assuming NextReader is implemented.
// 				if err != nil {
// 					if errors.Is(err, io.EOF) {
// 						break
// 					}
// 					b.Fatal(err) // Fail the benchmark on error
// 				}
// 			}
// 		}
// 	})
// }

// // Concurrent benchmark for comparing NextMessage and NextReader functions.
// func BenchmarkConcurrentMessageReading(b *testing.B) {
// 	// Create a buffer to simulate a connection with multiple messages.
// 	var buffer bytes.Buffer

// 	// Populate the buffer with some test messages.
// 	for i := 0; i < 1000; i++ {
// 		// Create a UUID and its byte representation for the ID message.
// 		id := uuid.New()
// 		message := createMessage(KindSlin, id[:])
// 		buffer.Write(message)
// 	}

// 	// Define the number of concurrent readers.
// 	numReaders := 100

// 	// Benchmark for NextMessage from audiosocket.
// 	b.Run("Concurrent Audiosocket", func(b *testing.B) {
// 		b.ResetTimer()

// 		var wg sync.WaitGroup
// 		for i := 0; i < numReaders; i++ {
// 			wg.Add(1)
// 			go func() {
// 				defer wg.Done()
// 				for j := 0; j < b.N; j++ {
// 					// Reset the buffer reader for each iteration.
// 					reader := bytes.NewReader(buffer.Bytes())
// 					for {
// 						_, err := audiosocket.NextMessage(reader)
// 						if err != nil {
// 							if errors.Is(err, io.EOF) {
// 								break
// 							}
// 							b.Fatal(err) // Fail the benchmark on error
// 						}
// 					}
// 				}
// 			}()
// 		}
// 		wg.Wait()
// 	})

// 	// Benchmark for NextReader (assumed to be defined elsewhere).
// 	b.Run("Concurrent FastAudiosocket", func(b *testing.B) {
// 		b.ResetTimer()

// 		var wg sync.WaitGroup
// 		for i := 0; i < numReaders; i++ {
// 			wg.Add(1)
// 			go func() {
// 				defer wg.Done()
// 				for j := 0; j < b.N; j++ {
// 					// Reset the buffer reader for each iteration.
// 					reader := bytes.NewReader(buffer.Bytes())
// 					for {
// 						_, err := NextMessage(reader) // Assuming NextReader is implemented.
// 						if err != nil {
// 							if errors.Is(err, io.EOF) {
// 								break
// 							}
// 							b.Fatal(err) // Fail the benchmark on error
// 						}
// 					}
// 				}
// 			}()
// 		}
// 		wg.Wait()
// 	})
// }
