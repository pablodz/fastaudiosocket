package fastaudiosocket

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
)

// getframes extracts the audio frames from a WAV file's content.
// It validates the WAV file structure and returns the audio data.
//
// Parameters:
//   - wavContent: The byte content of the WAV file.
//
// Returns:
//   - A slice of bytes containing the audio frames.
//   - An error if the WAV file is invalid or the data cannot be extracted.
func getframes(wavContent []byte) ([]byte, error) {
	if len(wavContent) < 44 {
		return nil, errors.New("file too small")
	}
	if !bytes.HasPrefix(wavContent, []byte("RIFF")) || !bytes.HasPrefix(wavContent[8:], []byte("WAVE")) {
		return nil, errors.New("not a valid WAV file")
	}

	dataChunkPos := bytes.Index(wavContent, []byte("data"))
	if dataChunkPos == -1 {
		return nil, errors.New("no data chunk found")
	}

	if len(wavContent) < dataChunkPos+8 {
		return nil, errors.New("data chunk header too small")
	}

	dataSize := binary.LittleEndian.Uint32(wavContent[dataChunkPos+4 : dataChunkPos+8])
	if dataChunkPos+8+int(dataSize) > len(wavContent) {
		return nil, errors.New("data chunk size exceeds file length")
	}

	return wavContent[dataChunkPos+8 : dataChunkPos+8+int(dataSize)], nil
}

// PlayWav streams audio data extracted from a WAV file to the audiosocket.
//
// Parameters:
//   - playerCtx: The context for controlling the playback.
//   - audioData: The byte content of the WAV file.
//
// Returns:
//   - An error if the playback fails.
func (s *FastAudioSocket) PlayWav(playerCtx context.Context, audioData []byte) error {
	audioData, err := getframes(audioData)
	if err != nil {
		return fmt.Errorf("failed to get frames: %w", err)
	}

	err = s.Play(playerCtx, audioData)
	if err != nil {
		return fmt.Errorf("failed to stream write: %w", err)
	}

	return nil
}

// fileExists checks if a file exists at the given filepath.
//
// Parameters:
//   - filepath: The path to the file.
//
// Returns:
//   - True if the file exists and is not a directory, false otherwise.
func fileExists(filepath string) bool {
	fileinfo, err := os.Stat(filepath)
	if os.IsNotExist(err) {
		return false
	}
	// Return false if the fileinfo says the file path is a directory.
	return !fileinfo.IsDir()
}

// PlayWavFile reads a WAV file from the filesystem and streams its audio data.
//
// Parameters:
//   - playerCtx: The context for controlling the playback.
//   - filename: The path to the WAV file.
//
// Returns:
//   - An error if the file does not exist, is invalid, or playback fails.
func (s *FastAudioSocket) PlayWavFile(playerCtx context.Context, filename string) error {
	if !fileExists(filename) {
		return fmt.Errorf("file does not exist: %s", filename)
	}

	content, err := os.ReadFile(filename) // the file is inside the local directory
	if err != nil {
		return fmt.Errorf("failed to read file: %w", err)
	}
	if len(content) == 0 {
		return fmt.Errorf("file is empty: %s", filename)
	}

	err = s.PlayWav(playerCtx, content)
	if err != nil {
		return fmt.Errorf("failed to stream write wav: %w", err)
	}

	return nil
}
