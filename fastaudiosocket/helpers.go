package fastaudiosocket

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
)

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

func (s *FastAudioSocket) PlayWav(audioData []byte) error {
	audioData, err := getframes(audioData)
	if err != nil {
		return fmt.Errorf("failed to get frames: %w", err)
	}

	err = s.Play(audioData)
	if err != nil {
		return fmt.Errorf("failed to stream write: %w", err)
	}

	return nil
}

func fileExists(filepath string) bool {
	fileinfo, err := os.Stat(filepath)
	if os.IsNotExist(err) {
		return false
	}
	// Return false if the fileinfo says the file path is a directory.
	return !fileinfo.IsDir()
}

func (s *FastAudioSocket) PlayFileOnAsterisk(filename string) error {
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

	err = s.PlayWav(content)
	if err != nil {
		return fmt.Errorf("failed to stream write wav: %w", err)
	}

	return nil
}
