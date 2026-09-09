# FastAudioSocket

FastAudioSocket is a Go package for handling audio and DTMF packets over the Asterisk AudioSocket protocol.

## Install

A better way to manage audiosocket protocol.

```bash
go get github.com/pablodz/fastaudiosocket
```

## Requirements for DTMF support

This package requires Asterisk versions released after **Mar 28, 2025**, as it depends on the changes introduced in the following commit:

[Asterisk Commit a5bc39fa326b9936064d9ee81a5ec4678b97aa24](https://github.com/asterisk/asterisk/commit/a5bc39fa326b9936064d9ee81a5ec4678b97aa24)

## Info

- Read packages

Incoming packets are read from a TCP byte stream in `fastaudiosocket/socket.go` and delivered through `AudioChan`. Payload size follows the protocol header; a standard 20 ms PCM16 frame at 8 kHz has 320 bytes.

- Write packages

All write packages are 320 bytes + 3 headers. The main difference here is that we need to handle from audiosocket server the timing to send each packet (header and payload) to Asterisk, normally every 20 ms. Catch-up and optional lead are described below. Write only accepts 320 bytes of payload in pcm linear 16 format. (Remember to remove the headers if you are sending the chunk in streaming)

## Examples

Check the examples folder to see how to use the package.

## Playback under scheduler jitter

`SetPlaybackOptions` enables a bounded amount of audio ahead of estimated
playout. It applies to `Play`, `PlayControlled`, `PlayWav`, `PlayWavControlled`,
`PlayWavFile`, and `PlayStreaming`. The default `Lead: 0` keeps deadline pacing.

```go
// Example starting point; measure the receiver and interruption budget first.
if err := socket.SetPlaybackOptions(fastaudiosocket.PlaybackOptions{
    Lead: 100 * time.Millisecond,
}); err != nil {
    return err
}

// One channel/PlayStreaming call per utterance preserves partial PCM frames
// across producer chunks. Send raw 8 kHz mono PCM16, without WAV headers.
// The producer must close chunks to flush the final, silence-padded frame.
if err := socket.PlayStreaming(playbackCtx, chunks, nil); err != nil {
    return err
}
stats := socket.PlaybackStats()
```

The lead must be a nonnegative multiple of 20 ms. A 100 ms target sends five
frames immediately, then one every 20 ms. It includes the frame currently
playing: the steady-state reserve immediately before replenishment is about
80 ms. A delayed writer consumes this reserve. If it runs out, playback refills
only the configured target; it cannot recover an already audible gap. An
undersized target can therefore perform worse than unrestricted catch-up.

Use one continuous `PlayStreaming` call for a streamed utterance. With a positive
lead, sequential playback calls also share the estimate of outstanding audio,
so every new chunk cannot add another buffer to the socket. Separate WAV/Play
calls still pad their individual tails. Data must be available ahead of playout:
lead cannot compensate for a producer permanently slower than real time.

**Asterisk compatibility matters.** AudioSocket accepts framed audio over TCP;
that does not promise a timed playback buffer. The tested channel driver and
`AudioSocket()` application forwarded early frames to RTP in bursts. The
receiver must retain those frames and play them on its media clock. Simply
setting `JITTERBUFFER()` on the AudioSocket channel did not smooth this path.
See the [full benchmark report and Asterisk findings](https://github.com/pablodz/fastaudiosocket/issues/3#issuecomment-5600048776)
before enabling lead for a deployment.

The single-stream experiments covered 219 runs and 22,902 received audio frames,
with zero payload mismatch frames. At approximately 80% of one sender core,
the mean modeled FIFO underrun was 30.29 ms with deadlines and 0 ms with a
100 ms lead. Evaluate 100–120 ms when the receiver and interruption budget
permit it; these short local measurements do not guarantee acoustic quality.
The Go benchmark harness and tests are in `cmd/jitterbench`; the full tables,
environment, and reproduction instructions are in the linked issue comment.

For concurrent-call sizing, `cmd/capacitybench` runs bidirectional calls in one
shared Go sender process and measures actual Asterisk RTP arrival times using
kernel timestamps. The [capacity report](https://github.com/pablodz/fastaudiosocket/issues/3#issuecomment-5605932926)
records 99 runs, including CPU affinity, runtime parallelism, memory limits,
and scheduler-stall comparisons. Local transport reference points were 200 calls
on two CPU threads and 400 on four, with a 4 GiB container limit. Validate the
complete application on the intended hardware before using these as capacity
limits; all frames can arrive successfully while playback timing still fails.
The Go source and tests are retained; full results and the temporary Asterisk
lab recipe are in the report.

The [performance follow-up](https://github.com/pablodz/fastaudiosocket/issues/8)
adds sender allocation and scheduler-delay measurements, plus optional CPU,
allocation, blocking, mutex, and execution-trace profiles. Profiles require
`-profile` and an explicit `-profile-output` path; collect them separately from
timing comparisons. The harness now defaults to `-context-scope call` for
independent call lifetimes. Use `-context-scope shared` to reproduce the shared
cancellation context used by the earlier capacity experiments.

Normal 320-byte outbound frame serialization reuses storage owned by the socket
after each write returns. This reduces allocation work; incoming payloads retain
their independent ownership. Measured improvements and unsuccessful experiments
are recorded in the performance issue.

Cancellation stops further writes and interrupts a blocked write/control wait.
It cannot retract audio already handed to TCP, Asterisk, or the RTP receiver.
At a 100 ms target, roughly that much audio can still be outstanding locally;
network and receiver buffering can add more. `Play*` returns after sending, so
immediately hanging up can truncate this tail. `PlaybackControl.Played(d)` means
**successfully sent audio**, including padding, not elapsed time or confirmed
playout. Pausing a control cannot pause audio already sent either.

`PlaybackStats()` is safe to poll. `MaxScheduleLateness` reports the largest
observed dispatch delay, including delays covered by lead; `MaxLateness` compares
against media deadlines. Long stalls remain observable after resynchronization.
`EstimatedBuffered` decays with wall time and assumes immediate delivery and
8 kHz consumption. AudioSocket provides no playout acknowledgements, so this is
an estimate, not a receiver measurement or a bound on interruption latency.

Only one playback call may write to a socket at a time; overlapping calls return
`ErrPlaybackInProgress`. Option changes affect subsequent calls. Errors are
returned, including short writes; the historical `errChan` argument is unused.
A partially written protocol frame closes the connection because the next frame
could not safely resume that byte stream. FastAudioSocket owns the connection
and its cancellation deadlines; do not independently modify its deadlines while
playback is running.

## Receiving audio

TCP can fragment a header or payload across reads. The reader uses `io.ReadFull`
to preserve framing, including fragmented UUID handshakes and zero-length hangups.
It preserves payload bytes and does not infer their codec from their length.
The standard AudioSocket `0x10` format is PCM16 at 8 kHz; integrations with
nonstandard payload formats must configure their decoder explicitly.

Consume `AudioChan` until closed and handle `PacketTypeHangup` and
`PacketTypeError`. `PacketChan` is the internal reader queue; consuming it in
parallel with `AudioChan` steals packets from the audio delivery path.
`SilenceSuppressed` is a local idle hint, not proof of silence at the sender:
network/scheduler delay can also cause an idle interval. AudioSocket has no
source timestamps or sequence numbers to distinguish these cases. The generated
`Sequence` field is local to this library.

Call cancellation closes the owned connection and releases readers blocked by a
slow consumer. It also closes output channels. This prevents backpressure from
leaving goroutines behind; it does not make an arbitrarily slow audio consumer
keep up with real time.
