package fastaudiosocket

import (
	"bytes"
	"time"
)

const (
	UlawChunkSize        = 160
	UlawChunkDuration    = 20 * time.Millisecond
	UlawSilenceByte      = 0xFF
	UlawTelcoSilenceByte = 0xFD
)

var (
	ulawSilentChunk      = bytes.Repeat([]byte{UlawSilenceByte}, UlawChunkSize)
	ulawTelcoSilentChunk = bytes.Repeat([]byte{UlawTelcoSilenceByte}, UlawChunkSize)
)

func SilentUlawChunk() []byte {
	return ulawSilentChunk
}

func UlawSilence(d time.Duration) []byte {
	chunks := int(d / UlawChunkDuration)
	if chunks <= 0 {
		return nil
	}

	silence := make([]byte, chunks*UlawChunkSize)
	for offset := 0; offset < len(silence); offset += UlawChunkSize {
		copy(silence[offset:offset+UlawChunkSize], ulawSilentChunk)
	}
	return silence
}

func IsSilentUlawChunk(chunk []byte) bool {
	if len(chunk) != UlawChunkSize {
		return false
	}
	return bytes.Equal(chunk, ulawSilentChunk) || bytes.Equal(chunk, ulawTelcoSilentChunk)
}
