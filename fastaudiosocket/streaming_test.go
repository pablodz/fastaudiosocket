package fastaudiosocket

import (
	"context"
	"net"
	"testing"
	"time"
)

func newStreamingSocket(t *testing.T) (*FastAudioSocket, net.Conn, context.CancelFunc) {
	t.Helper()

	server, client := net.Pipe()
	callCtx, cancel := context.WithCancel(context.Background())

	t.Cleanup(func() {
		cancel()
		server.Close()
		client.Close()
	})

	return &FastAudioSocket{conn: server, callCtx: callCtx}, client, cancel
}

func TestPlayStreamingRealignsAcrossChunkBoundaries(t *testing.T) {
	socket, client, _ := newStreamingSocket(t)

	dataChan := make(chan []byte, 4)
	done := make(chan error, 1)

	go func() {
		done <- socket.PlayStreaming(context.Background(), dataChan, nil)
	}()

	first := make([]byte, 200)
	second := make([]byte, 440)
	for i := range first {
		first[i] = 0x11
	}
	for i := range second {
		second[i] = 0x22
	}

	dataChan <- first
	dataChan <- second

	packets := readPackets(t, client, 2)

	if got := len(packets[0]); got != MaxPacketSize {
		t.Fatalf("first packet is %d bytes, expected %d", got, MaxPacketSize)
	}

	payload := packets[0][HeaderSize:]
	for i := range 200 {
		if payload[i] != 0x11 {
			t.Fatalf("byte %d of the first packet is %#x; the first chunk was not carried whole", i, payload[i])
		}
	}
	for i := 200; i < WriteChunkSize; i++ {
		if payload[i] != 0x22 {
			t.Fatalf("byte %d of the first packet is %#x; silence was injected at the chunk boundary", i, payload[i])
		}
	}

	close(dataChan)
	if err := <-done; err != nil {
		t.Fatalf("streaming failed: %v", err)
	}
}

func TestPlayStreamingFlushesTheTailOnClose(t *testing.T) {
	socket, client, _ := newStreamingSocket(t)

	dataChan := make(chan []byte, 2)
	done := make(chan error, 1)

	go func() {
		done <- socket.PlayStreaming(context.Background(), dataChan, nil)
	}()

	tail := make([]byte, 100)
	for i := range tail {
		tail[i] = 0x33
	}
	dataChan <- tail
	close(dataChan)

	packets := readPackets(t, client, 1)

	payload := packets[0][HeaderSize:]
	if len(payload) != WriteChunkSize {
		t.Fatalf("tail packet payload is %d bytes, expected %d", len(payload), WriteChunkSize)
	}
	for i := range 100 {
		if payload[i] != 0x33 {
			t.Fatalf("tail byte %d is %#x, expected the buffered audio", i, payload[i])
		}
	}
	for i := 100; i < WriteChunkSize; i++ {
		if payload[i] != 0x00 {
			t.Fatalf("tail byte %d is %#x, expected silence padding", i, payload[i])
		}
	}

	if err := <-done; err != nil {
		t.Fatalf("streaming failed: %v", err)
	}
}

func TestPlayStreamingHoldsAPartialChunkUntilItFills(t *testing.T) {
	socket, client, _ := newStreamingSocket(t)

	dataChan := make(chan []byte, 2)
	done := make(chan error, 1)

	go func() {
		done <- socket.PlayStreaming(context.Background(), dataChan, nil)
	}()

	dataChan <- make([]byte, 100)

	client.SetReadDeadline(time.Now().Add(60 * time.Millisecond))
	buffer := make([]byte, MaxPacketSize)
	if _, err := client.Read(buffer); err == nil {
		t.Fatal("a partial chunk was sent padded instead of waiting for the rest")
	}

	client.SetReadDeadline(time.Time{})
	close(dataChan)

	if packets := readPackets(t, client, 1); len(packets[0]) != MaxPacketSize {
		t.Fatalf("the held chunk was flushed as %d bytes, expected %d", len(packets[0]), MaxPacketSize)
	}

	if err := <-done; err != nil {
		t.Fatalf("streaming failed: %v", err)
	}
}

func readPackets(t *testing.T, conn net.Conn, count int) [][]byte {
	t.Helper()

	packets := make([][]byte, 0, count)
	buffer := make([]byte, MaxPacketSize)

	for range count {
		conn.SetReadDeadline(time.Now().Add(time.Second))
		n, err := conn.Read(buffer)
		if err != nil {
			t.Fatalf("reading packet %d: %v", len(packets), err)
		}
		packet := make([]byte, n)
		copy(packet, buffer[:n])
		packets = append(packets, packet)
	}

	return packets
}
