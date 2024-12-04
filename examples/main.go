package main

import (
	"context"
	"log"
	"net"

	"github.com/pablodz/fastaudiosocket/fastaudiosocket"
)

func main() {
	dummyConn := &net.TCPConn{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fa, err := fastaudiosocket.NewFastAudioSocket(ctx, dummyConn, false, true)
	if err != nil {
		log.Printf("failed to create fast audio socket: %v", err)
		return
	}
	defer fa.Hangup()

	uuid := fa.GetUUID()
	log.Printf("UUID: %s", uuid)
	for {
		select {
		case <-ctx.Done():
			return

		case rPacket, ok := <-fa.PacketChan:
			// AudioChan is the channel where the audio packets are received
			if !ok {
				return
			}

			switch rPacket.Type {
			// this package is ulaw (160b) or pcm (320b)
			case fastaudiosocket.PacketTypeAudio:
				// do something with rPacket.Payload
			case fastaudiosocket.PacketTypeError:
				return
			case fastaudiosocket.PacketTypeHangup:
				return
			default:
				return
			}
		case m, ok := <-fa.MonitorChan:
			// Monitor helps to know the status of the connection, if rtp packets are being received
			if !ok {
				return
			}

			if m.ExpectedChunks == m.ChunkCounterReceived {
				log.Printf("[%s][%d/%d]", m.Message, m.ChunkCounterReceived, m.ExpectedChunks)
			} else {
				log.Printf("[%s][%d/%d]", m.Message, m.ChunkCounterReceived, m.ExpectedChunks)
			}

		}
	}
}
