package fastaudiosocket

import (
	"bytes"
	"testing"
	"time"
)

func TestSilentUlawChunkIsOneFrameOfSilence(t *testing.T) {
	chunk := SilentUlawChunk()

	if len(chunk) != UlawChunkSize {
		t.Fatalf("SilentUlawChunk() length = %d, want %d", len(chunk), UlawChunkSize)
	}
	if !bytes.Equal(chunk, bytes.Repeat([]byte{UlawSilenceByte}, UlawChunkSize)) {
		t.Fatal("SilentUlawChunk() is not filled with the ulaw silence byte")
	}
}

func TestUlawSilenceLength(t *testing.T) {
	tests := []struct {
		name     string
		duration time.Duration
		want     int
	}{
		{name: "negative", duration: -time.Second, want: 0},
		{name: "zero", duration: 0, want: 0},
		{name: "shorter than a chunk", duration: 19 * time.Millisecond, want: 0},
		{name: "one chunk", duration: UlawChunkDuration, want: UlawChunkSize},
		{name: "truncates the partial chunk", duration: 39 * time.Millisecond, want: UlawChunkSize},
		{name: "one second", duration: time.Second, want: 50 * UlawChunkSize},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := len(UlawSilence(tt.duration)); got != tt.want {
				t.Fatalf("len(UlawSilence(%s)) = %d, want %d", tt.duration, got, tt.want)
			}
		})
	}
}

func TestUlawSilenceIsSilentOnEveryChunk(t *testing.T) {
	silence := UlawSilence(100 * time.Millisecond)

	for offset := 0; offset < len(silence); offset += UlawChunkSize {
		if !IsSilentUlawChunk(silence[offset : offset+UlawChunkSize]) {
			t.Fatalf("chunk at offset %d is not silent", offset)
		}
	}
}

func TestIsSilentUlawChunk(t *testing.T) {
	audio := bytes.Repeat([]byte{0x7F}, UlawChunkSize)

	tests := []struct {
		name  string
		chunk []byte
		want  bool
	}{
		{name: "nil", chunk: nil},
		{name: "empty", chunk: []byte{}},
		{name: "shorter than a chunk", chunk: bytes.Repeat([]byte{UlawSilenceByte}, UlawChunkSize-1)},
		{name: "longer than a chunk", chunk: bytes.Repeat([]byte{UlawSilenceByte}, UlawChunkSize+1)},
		{name: "pcm16 chunk of the same call", chunk: bytes.Repeat([]byte{UlawSilenceByte}, WriteChunkSize)},
		{name: "audio", chunk: audio},
		{name: "one audio byte among silence", chunk: append(bytes.Repeat([]byte{UlawSilenceByte}, UlawChunkSize-1), 0x7F)},
		{name: "silence", chunk: bytes.Repeat([]byte{UlawSilenceByte}, UlawChunkSize), want: true},
		{name: "telco silence", chunk: bytes.Repeat([]byte{UlawTelcoSilenceByte}, UlawChunkSize), want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsSilentUlawChunk(tt.chunk); got != tt.want {
				t.Fatalf("IsSilentUlawChunk(%s) = %t, want %t", tt.name, got, tt.want)
			}
		})
	}
}
