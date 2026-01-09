package fastaudiosocket

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
)

// getframes parses the WAV headers to extract the PCM data chunk, ensuring format compatibility.
func getframes(wavContent []byte) ([]byte, error) {
	if len(wavContent) < 44 {
		return nil, errors.New("wav file too small")
	}

	// 1. Verify RIFF and WAVE headers
	if !bytes.Equal(wavContent[0:4], []byte("RIFF")) || !bytes.Equal(wavContent[8:12], []byte("WAVE")) {
		return nil, errors.New("invalid WAV header")
	}

	// 2. Verify Audio Format (PCM = 1)
	// We verify this because Play() expects raw PCM.
	audioFormat := binary.LittleEndian.Uint16(wavContent[20:22])
	if audioFormat != 1 {
		return nil, fmt.Errorf("unsupported WAV format: %d (expected 1 for PCM)", audioFormat)
	}

	// 3. Verify Bit Depth (Must be 16-bit for 320-byte chunks)
	bitsPerSample := binary.LittleEndian.Uint16(wavContent[34:36])
	if bitsPerSample != 16 {
		return nil, fmt.Errorf("unsupported bit depth: %d (expected 16-bit)", bitsPerSample)
	}

	// 4. Robust Chunk Searching
	// Iterate through chunks starting after the WAVE header to find "data"
	pos := 12
	for pos+8 <= len(wavContent) {
		chunkID := string(wavContent[pos : pos+4])
		chunkSize := binary.LittleEndian.Uint32(wavContent[pos+4 : pos+8])

		if chunkID == "data" {
			// Found data chunk
			start := pos + 8
			end := start + int(chunkSize)
			if end > len(wavContent) {
				// If the header claims more data than available, read until EOF to be safe
				return wavContent[start:], nil
			}
			return wavContent[start:end], nil
		}

		// Move to next chunk
		pos += 8 + int(chunkSize)

		// FIX: RIFF chunks must be word-aligned (2 bytes).
		// If the chunk size is odd, there is a padding byte that is NOT included in chunkSize.
		if chunkSize%2 != 0 {
			pos++
		}
	}

	return nil, errors.New("data chunk not found")
}

// PlayWav parses the audio data and streams it via Play.
func (s *FastAudioSocket) PlayWav(playerCtx context.Context, audioData []byte) error {
	rawFrames, err := getframes(audioData)
	if err != nil {
		return fmt.Errorf("failed to parse wav: %w", err)
	}

	return s.Play(playerCtx, rawFrames)
}

// fileExists checks if the path points to an existing file.
func fileExists(filepath string) bool {
	info, err := os.Stat(filepath)
	if os.IsNotExist(err) {
		return false
	}
	return !info.IsDir()
}

// PlayWavFile reads a WAV file from disk and streams it.
func (s *FastAudioSocket) PlayWavFile(playerCtx context.Context, filename string) error {
	if !fileExists(filename) {
		return fmt.Errorf("file not found: %s", filename)
	}

	content, err := os.ReadFile(filename)
	if err != nil {
		return fmt.Errorf("read error: %w", err)
	}

	return s.PlayWav(playerCtx, content)
}
