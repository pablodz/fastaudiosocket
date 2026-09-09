//go:build linux

// jitterbench measures the actual library over TCP and, optionally, Asterisk RTP.
// All audio and identifiers are synthetic. No remote service is required.
package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	fa "github.com/pablodz/fastaudiosocket/fastaudiosocket"
)

var (
	asteriskCodec  = flag.String("asterisk-codec", "slin", "channel driver native codec: slin or ulaw; writer always sends standard PCM16")
	senderQuota    = flag.Float64("sender-quota", 0, "optional Docker CPU quota for sender only (e.g. 0.5); requires cached debian:bookworm-slim and no -stall")
	asteriskConfig = flag.String("asterisk-config", "/etc/asterisk/asterisk.conf", "Asterisk configuration path")
	worker         = flag.Bool("worker", false, "internal sender subprocess")
	peer           = flag.String("peer", "tcp", "tcp, asterisk-channel, asterisk-app, asterisk-fixed, asterisk-adaptive")
	modes          = flag.String("modes", "ticker,deadline,lead40,lead60,lead100,lead200", "comma separated mechanisms")
	loads          = flag.String("loads", "0,40,80", "CPU worker duty percentages, not host CPU percentages")
	frames         = flag.Int("frames", 250, "20 ms frames per run")
	repeat         = flag.Int("repeat", 3, "repetitions")
	procs          = flag.Int("procs", 1, "GOMAXPROCS in the sender subprocess")
	workers        = flag.Int("cpu-workers", 1, "CPU load goroutines in sender")
	period         = flag.Duration("load-period", 100*time.Millisecond, "CPU worker busy/sleep period")
	stall          = flag.Duration("stall", 0, "SIGSTOP duration imposed on sender only")
	stallAt        = flag.Duration("stall-at", time.Second, "first SIGSTOP relative to playback start")
	stallEvery     = flag.Duration("stall-every", time.Second, "interval between SIGSTOPs; zero means once")
	cancelAt       = flag.Duration("cancel-after", 0, "cancel playback after this duration; zero disables")
	chunkFrames    = flag.Int("chunk-frames", 0, "use sequential Play calls of this many frames; zero uses one stream")
	sourceGap      = flag.Duration("source-gap", 0, "delay between producer chunks of 10 frames")
	jb             = flag.Int("jb", 100, "Asterisk jitter buffer size in ms (dialplan extension prefix)")
	output         = flag.String("output", "", "optional CSV file; stdout always contains JSON results")
	traceDir       = flag.String("traces", "", "optional directory for packet arrival traces")
)

type arrival struct {
	At        time.Time
	Timestamp uint32
	Sequence  uint16
	Bytes     int
	Payload   []byte
}
type senderResult struct {
	ThrottledPeriods uint64           `json:"throttled_periods,omitempty"`
	ThrottledMS      float64          `json:"throttled_ms,omitempty"`
	Stats            fa.PlaybackStats `json:"stats"`
	CPUms            float64          `json:"cpu_ms"`
	Wallms           float64          `json:"wall_ms"`
	Error            string           `json:"error,omitempty"`
	Cancelms         float64          `json:"cancel_ms,omitempty"`
}
type result struct {
	RTPClockGap     float64      `json:"rtp_clock_gap_ms,omitempty"`
	RTPClockOverlap float64      `json:"rtp_clock_overlap_ms,omitempty"`
	RTPLate20       int          `json:"rtp_late_frames_20ms,omitempty"`
	FirstPacket     float64      `json:"first_packet_ms"`
	RTPTail         float64      `json:"rtp_tail_after_sender_ms"`
	Peer            string       `json:"peer"`
	Mode            string       `json:"mode"`
	Load            int          `json:"load_duty_pct"`
	Run             int          `json:"run"`
	Packets         int          `json:"packets"`
	PayloadErrors   int          `json:"payload_errors"`
	TimestampJumps  int          `json:"timestamp_jumps"`
	GapP99          float64      `json:"gap_p99_ms"`
	GapMax          float64      `json:"gap_max_ms"`
	Burst           int          `json:"burst_gaps_lt_1ms"`
	FIFOGap         float64      `json:"fifo_underrun_ms"`
	FIFOBuffer      float64      `json:"fifo_peak_ms"`
	Late20          int          `json:"late_frames_20ms"`
	Late60          int          `json:"late_frames_60ms"`
	Late100         int          `json:"late_frames_100ms"`
	Sender          senderResult `json:"sender"`
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
func main() {
	flag.Parse()
	if *worker {
		runWorker()
		return
	}
	if *frames < 1 || *repeat < 1 || *procs < 1 || *workers < 0 || *period <= 0 || *stall < 0 || *chunkFrames < 0 {
		panic("invalid benchmark settings")
	}
	if *senderQuota < 0 || (*senderQuota > 0 && *stall > 0) {
		panic("sender-quota must be positive and cannot be combined with SIGSTOP")
	}
	if *asteriskCodec != "slin" && *asteriskCodec != "ulaw" {
		panic("unsupported benchmark codec")
	}
	var cw *csv.Writer
	if *output != "" {
		f, err := os.Create(*output)
		must(err)
		defer f.Close()
		cw = csv.NewWriter(f)
		defer cw.Flush()
		must(cw.Write([]string{"peer", "mode", "load_duty_pct", "run", "packets", "payload_errors", "timestamp_jumps", "gap_p99_ms", "gap_max_ms", "burst_gaps_lt_1ms", "fifo_underrun_ms", "fifo_peak_ms", "late_frames_20ms", "late_frames_60ms", "late_frames_100ms", "sender_cpu_ms", "sender_wall_ms", "resyncs", "estimated_buffer_ms", "cancel_ms", "first_packet_ms", "rtp_tail_after_sender_ms"}))
	}
	fmt.Fprintf(os.Stderr, "go=%s os=%s arch=%s sender_GOMAXPROCS=%d workers=%d period=%s frames=%d stall=%s peer=%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH, *procs, *workers, *period, *frames, *stall, *peer)
	for run := 1; run <= *repeat; run++ {
		modeList := strings.Split(*modes, ",")
		// Rotate order to avoid always measuring the same mode on a cold/hot CPU.
		shift := (run - 1) % len(modeList)
		modeList = append(modeList[shift:], modeList[:shift]...)
		for _, l := range strings.Split(*loads, ",") {
			load, err := strconv.Atoi(l)
			must(err)
			if load < 0 || load > 100 {
				panic("load must be 0..100")
			}
			for _, mode := range modeList {
				r, trace := experiment(mode, load, run)
				must(json.NewEncoder(os.Stdout).Encode(r))
				if cw != nil {
					must(cw.Write([]string{r.Peer, r.Mode, strconv.Itoa(load), strconv.Itoa(run), strconv.Itoa(r.Packets), strconv.Itoa(r.PayloadErrors), strconv.Itoa(r.TimestampJumps), fmt.Sprint(r.GapP99), fmt.Sprint(r.GapMax), strconv.Itoa(r.Burst), fmt.Sprint(r.FIFOGap), fmt.Sprint(r.FIFOBuffer), strconv.Itoa(r.Late20), strconv.Itoa(r.Late60), strconv.Itoa(r.Late100), fmt.Sprint(r.Sender.CPUms), fmt.Sprint(r.Sender.Wallms), fmt.Sprint(r.Sender.Stats.Resyncs), fmt.Sprint(ms(r.Sender.Stats.EstimatedBuffered)), fmt.Sprint(r.Sender.Cancelms), fmt.Sprint(r.FirstPacket), fmt.Sprint(r.RTPTail)}))
					cw.Flush()
					must(cw.Error())
				}
				if *traceDir != "" {
					must(os.MkdirAll(*traceDir, 0755))
					f, e := os.Create(fmt.Sprintf("%s/%s-%s-load%d-run%d.csv", *traceDir, *peer, mode, load, run))
					must(e)
					w := csv.NewWriter(f)
					must(w.Write([]string{"elapsed_ns", "rtp_timestamp", "rtp_sequence", "payload_bytes"}))
					for _, a := range trace {
						must(w.Write([]string{strconv.FormatInt(a.At.Sub(trace[0].At).Nanoseconds(), 10), strconv.FormatUint(uint64(a.Timestamp), 10), strconv.Itoa(int(a.Sequence)), strconv.Itoa(a.Bytes)}))
					}
					w.Flush()
					must(w.Error())
					must(f.Close())
				}
			}
		}
	}
}

func pcm(n int) []byte {
	b := make([]byte, n*fa.WriteChunkSize)
	for i := 0; i < len(b)/2; i++ {
		v := int16(10000 * math.Sin(2*math.Pi*437*float64(i)/8000))
		binary.LittleEndian.PutUint16(b[i*2:], uint16(v))
	}
	return b
}
func cpuLoad(ctx context.Context, pct int, wg *sync.WaitGroup) {
	defer wg.Done()
	if pct == 0 {
		return
	}
	busy := time.Duration(int64(*period) * int64(pct) / 100)
	for ctx.Err() == nil {
		start := time.Now()
		until := start.Add(busy)
		for time.Now().Before(until) {
			if ctx.Err() != nil {
				return
			}
		}
		timer := time.NewTimer(max(0, *period-time.Since(start)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
func cpuTime() time.Duration {
	var u syscall.Rusage
	must(syscall.Getrusage(syscall.RUSAGE_SELF, &u))
	return time.Duration(u.Utime.Nano() + u.Stime.Nano())
}

func runWorker() {
	runtime.GOMAXPROCS(*procs)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	must(err)
	defer l.Close()
	fmt.Println(l.Addr().String())
	conn, err := l.Accept()
	must(err)
	defer conn.Close()
	must(conn.SetDeadline(time.Now().Add(time.Minute)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := fa.NewFastAudioSocket(ctx, conn, false, false)
	must(err)
	// Consume RX independently so the measurement cannot be blocked by AudioChan.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-s.AudioChan:
				if !ok {
					return
				}
			}
		}
	}()
	lead := time.Duration(0)
	if strings.HasPrefix(*modes, "lead") {
		n, e := strconv.Atoi(strings.TrimPrefix(*modes, "lead"))
		must(e)
		lead = time.Duration(n) * time.Millisecond
	}
	must(s.SetPlaybackOptions(fa.PlaybackOptions{Lead: lead}))
	// Asterisk's channel driver sends UUID before its bridge is established.
	time.Sleep(300 * time.Millisecond)
	fmt.Println("playing")
	playCtx, stop := context.WithCancel(ctx)
	defer stop()
	var cancellation time.Time
	var cancelDone chan struct{}
	if *cancelAt > 0 {
		cancelDone = make(chan struct{})
		go func() {
			timer := time.NewTimer(*cancelAt)
			defer timer.Stop()
			select {
			case <-playCtx.Done():
			case <-timer.C:
				cancellation = time.Now()
				stop()
			}
			close(cancelDone)
		}()
	}
	loadCtx, loadStop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	load, e := strconv.Atoi(*loads)
	must(e)
	for i := 0; i < *workers; i++ {
		wg.Add(1)
		go cpuLoad(loadCtx, load, &wg)
	}
	audio := pcm(*frames)
	cpuStart := cpuTime()
	throttleStart := throttling()
	started := time.Now()
	if *modes == "ticker" {
		tick := time.NewTicker(fa.TickerInterval)
	loop:
		for i := 0; i < *frames; i++ {
			select {
			case <-playCtx.Done():
				err = playCtx.Err()
				break loop
			case <-tick.C:
			}
			p := append([]byte{fa.PacketTypeAudio, 1, 64}, audio[i*320:(i+1)*320]...)
			_, err = conn.Write(p)
			if err != nil {
				break
			}
		}
		tick.Stop()
	} else if *chunkFrames > 0 {
		for i := 0; i < *frames; i += *chunkFrames {
			err = s.Play(playCtx, audio[i*320:min(i+*chunkFrames, *frames)*320])
			if err != nil {
				break
			}
		}
	} else {
		chunks := make(chan []byte, 4)
		go func() {
			defer close(chunks)
			for i := 0; i < len(audio); {
				end := min(i+3200, len(audio)) // Deliberately split on a non-frame boundary.
				for _, b := range [][]byte{audio[i:min(i+197, end)], audio[min(i+197, end):end]} {
					select {
					case <-playCtx.Done():
						return
					case chunks <- b:
					}
				}
				i = end
				if *sourceGap > 0 {
					timer := time.NewTimer(*sourceGap)
					select {
					case <-playCtx.Done():
						timer.Stop()
						return
					case <-timer.C:
					}
				}
			}
		}()
		err = s.PlayStreaming(playCtx, chunks, nil)
	}
	ended := time.Now()
	r := senderResult{Stats: s.PlaybackStats(), CPUms: ms(cpuTime() - cpuStart), Wallms: ms(ended.Sub(started))}
	throttleEnd := throttling()
	r.ThrottledPeriods = throttleEnd[0] - throttleStart[0]
	r.ThrottledMS = float64(throttleEnd[1]-throttleStart[1]) / 1000
	stop()
	if cancelDone != nil {
		<-cancelDone
		if !cancellation.IsZero() {
			r.Cancelms = ms(ended.Sub(cancellation))
		}
	}
	loadStop()
	wg.Wait()
	if err != nil {
		r.Error = err.Error()
	}
	must(json.NewEncoder(os.Stdout).Encode(r))
	// Keep the call alive long enough to drain downstream queues, including JB.
	time.Sleep(max(time.Second, lead+500*time.Millisecond))
}

func experiment(mode string, load, run int) (result, []arrival) {
	switch mode {
	case "ticker", "deadline":
	default:
		if !strings.HasPrefix(mode, "lead") {
			panic("unknown mechanism")
		}
	}
	switch *peer {
	case "tcp", "asterisk-channel", "asterisk-app", "asterisk-fixed", "asterisk-adaptive":
	default:
		panic("unknown peer")
	}
	cmd := exec.Command(os.Args[0], "-worker", "-modes", mode, "-loads", strconv.Itoa(load), "-frames", strconv.Itoa(*frames), "-procs", strconv.Itoa(*procs), "-cpu-workers", strconv.Itoa(*workers), "-load-period", period.String(), "-cancel-after", cancelAt.String(), "-chunk-frames", strconv.Itoa(*chunkFrames), "-source-gap", sourceGap.String())
	if *senderQuota > 0 {
		binaryPath, e := os.Executable()
		must(e)
		tmp, e := os.MkdirTemp("", "jitterbench-quota-")
		must(e)
		defer os.RemoveAll(tmp)
		cid := tmp + "/cid"
		args := []string{"run", "--rm", "--network", "host", "--cpus", fmt.Sprint(*senderQuota), "--memory", "128m", "--cap-drop", "ALL", "--cidfile", cid, "-v", binaryPath + ":/jitterbench:ro", "--entrypoint", "/jitterbench", "debian:bookworm-slim"}
		cmd = exec.Command("docker", append(args, cmd.Args[1:]...)...)
		defer func() {
			if id, e := os.ReadFile(cid); e == nil {
				_ = exec.Command("docker", "rm", "-f", strings.TrimSpace(string(id))).Run()
			}
		}()
	}
	stdout, err := cmd.StdoutPipe()
	must(err)
	cmd.Stderr = os.Stderr
	must(cmd.Start())
	defer func() { _ = cmd.Process.Signal(syscall.SIGCONT); _ = cmd.Process.Kill() }()
	scan := bufio.NewScanner(stdout)
	if !scan.Scan() {
		panic("sender did not start")
	}
	addr := scan.Text()
	var arrivals []arrival
	var receiveErr error
	var conn net.Conn
	var udp *net.UDPConn
	received := make(chan struct{})
	id := "00000000-0000-4000-8000-000000000001"
	if *peer == "tcp" {
		conn, err = net.Dial("tcp", addr)
		must(err)
		defer conn.Close()
		_, err = conn.Write([]byte{1, 0, 16, 0, 0, 0, 0, 0, 0, 64, 0, 128, 0, 0, 0, 0, 0, 0, 1})
		must(err)
		go func() {
			defer close(received)
			for {
				var h [3]byte
				_, e := io.ReadFull(conn, h[:])
				if e == io.EOF {
					return
				}
				if e != nil {
					receiveErr = e
					return
				}
				n := int(binary.BigEndian.Uint16(h[1:]))
				b := make([]byte, n)
				_, e = io.ReadFull(conn, b)
				if e != nil {
					receiveErr = e
					return
				}
				if h[0] == fa.PacketTypeAudio {
					arrivals = append(arrivals, arrival{At: time.Now(), Bytes: n, Payload: b})
				}
			}
		}()
	} else {
		udp, err = net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
		must(err)
		defer udp.Close()
		port := udp.LocalAddr().(*net.UDPAddr).Port
		go func() {
			defer close(received)
			b := make([]byte, 2048)
			for {
				n, _, e := udp.ReadFromUDP(b)
				if e != nil {
					if !strings.Contains(e.Error(), "closed") {
						receiveErr = e
					}
					return
				}
				at := time.Now()
				if n < 12 || b[0]>>6 != 2 {
					receiveErr = fmt.Errorf("invalid RTP packet")
					return
				}
				off := 12 + int(b[0]&15)*4
				if b[0]&0x10 != 0 {
					if n < off+4 {
						receiveErr = io.ErrUnexpectedEOF
						return
					}
					off += 4 + int(binary.BigEndian.Uint16(b[off+2:]))*4
				}
				end := n
				if b[0]&0x20 != 0 {
					end -= int(b[n-1])
				}
				if end < off {
					receiveErr = io.ErrUnexpectedEOF
					return
				}
				if b[1]&0x7f != 0 {
					continue
				}
				arrivals = append(arrivals, arrival{At: at, Timestamp: binary.BigEndian.Uint32(b[4:8]), Sequence: binary.BigEndian.Uint16(b[2:4]), Bytes: end - off, Payload: append([]byte(nil), b[off:end]...)})
			}
		}()
		var originate string
		if *peer == "asterisk-app" {
			originate = fmt.Sprintf("channel originate UnicastRTP/127.0.0.1:%d/c(ulaw) application AudioSocket %s,%s", port, id, addr)
		} else {
			ctx := "bench"
			if *peer == "asterisk-fixed" {
				ctx = "bench-fixed"
			}
			if *peer == "asterisk-adaptive" {
				ctx = "bench-adaptive"
			}
			originate = fmt.Sprintf("channel originate AudioSocket/%s/%s/c(%s) extension %d*%d@%s", addr, id, *asteriskCodec, *jb, port, ctx)
		}
		out, e := exec.Command("asterisk", "-C", *asteriskConfig, "-rx", originate).CombinedOutput()
		if e != nil {
			panic(fmt.Sprintf("originate: %v: %s", e, out))
		}
	}
	if !scan.Scan() || scan.Text() != "playing" {
		panic("sender did not start playback")
	}
	playbackStarted := time.Now()
	stopStalls := make(chan struct{})
	stallsDone := make(chan struct{})
	go func() {
		defer close(stallsDone)
		if *stall <= 0 {
			return
		}
		timer := time.NewTimer(*stallAt)
		defer timer.Stop()
		for {
			select {
			case <-stopStalls:
				return
			case <-timer.C:
			}
			_ = cmd.Process.Signal(syscall.SIGSTOP)
			t := time.NewTimer(*stall)
			select {
			case <-stopStalls:
				t.Stop()
				_ = cmd.Process.Signal(syscall.SIGCONT)
				return
			case <-t.C:
			}
			_ = cmd.Process.Signal(syscall.SIGCONT)
			if *stallEvery <= 0 {
				return
			}
			timer.Reset(max(0, *stallEvery-*stall))
		}
	}()
	var sr senderResult
	if !scan.Scan() {
		panic("sender did not return metrics")
	}
	must(json.Unmarshal(scan.Bytes(), &sr))
	close(stopStalls)
	<-stallsDone
	must(cmd.Wait())
	if udp != nil {
		udp.Close()
	}
	<-received
	must(receiveErr)
	r := analyze(arrivals, *peer != "tcp")
	r.Peer = *peer
	r.Mode = mode
	r.Load = load
	r.Run = run
	r.Sender = sr
	if r.Packets == 0 {
		panic("no audio received")
	}
	r.FirstPacket = ms(arrivals[0].At.Sub(playbackStarted))
	r.RTPTail = max(0, ms(arrivals[len(arrivals)-1].At.Sub(playbackStarted))-sr.Wallms)
	if sr.Error != "" && (*cancelAt == 0 || sr.Error != "context canceled") {
		panic(sr.Error)
	}
	return r, arrivals
}

func throttling() [2]uint64 {
	var result [2]uint64
	b, err := os.ReadFile("/sys/fs/cgroup/cpu.stat")
	if err != nil {
		return result
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		v, _ := strconv.ParseUint(f[1], 10, 64)
		if f[0] == "nr_throttled" {
			result[0] = v
		}
		if f[0] == "throttled_usec" {
			result[1] = v
		}
	}
	return result
}
func analyze(a []arrival, rtp bool) result {
	r := result{Packets: len(a)}
	if len(a) == 0 {
		return r
	}
	end := a[0].At
	gaps := make([]float64, 0, len(a)-1)
	audio := pcm(*frames)
	for i, v := range a {
		duration := time.Duration(v.Bytes/2) * time.Second / 8000
		if rtp {
			duration = time.Duration(v.Bytes) * time.Second / 8000
		}
		if i > 0 {
			gap := v.At.Sub(a[i-1].At)
			gaps = append(gaps, ms(gap))
			if gap < time.Millisecond {
				r.Burst++
			}
			if rtp && v.Timestamp-a[i-1].Timestamp != uint32(a[i-1].Bytes) {
				r.TimestampJumps++
				delta := int64(int32(v.Timestamp-a[i-1].Timestamp)) - int64(a[i-1].Bytes)
				if delta > 0 {
					r.RTPClockGap += float64(delta) / 8
				} else {
					r.RTPClockOverlap += float64(-delta) / 8
				}
			}
		}
		if v.At.After(end) {
			if i > 0 {
				r.FIFOGap += ms(v.At.Sub(end))
			}
			end = v.At
		}
		end = end.Add(duration)
		r.FIFOBuffer = max(r.FIFOBuffer, ms(end.Sub(v.At)))
		// A fixed receiver preserves original sample order; late frames are lost,
		// even when an RTP sender masks a gap by changing its timestamps.
		lateness := v.At.Sub(a[0].At.Add(time.Duration(i) * 20 * time.Millisecond))
		if lateness > 20*time.Millisecond {
			r.Late20++
		}
		if lateness > 60*time.Millisecond {
			r.Late60++
		}
		if lateness > 100*time.Millisecond {
			r.Late100++
		}
		if rtp {
			due := a[0].At.Add(time.Duration(uint32(v.Timestamp-a[0].Timestamp)) * time.Second / 8000)
			if v.At.Sub(due) > 20*time.Millisecond {
				r.RTPLate20++
			}
		}
		if !rtp {
			if v.Bytes != 320 || i*320+v.Bytes > len(audio) || string(v.Payload) != string(audio[i*320:i*320+v.Bytes]) {
				r.PayloadErrors++
			}
		} else {
			mismatch := v.Bytes != 160 || (i+1)*320 > len(audio)
			if !mismatch {
				for j, b := range v.Payload {
					want := int(int16(binary.LittleEndian.Uint16(audio[(i*160+j)*2:])))
					if math.Abs(float64(decodeULaw(b)-want)) > 256 {
						mismatch = true
						break
					}
				}
			}
			if mismatch {
				r.PayloadErrors++
			}
		}
	}
	if len(gaps) > 0 {
		sort.Float64s(gaps)
		r.GapP99 = gaps[min(len(gaps)-1, int(math.Ceil(float64(len(gaps))*.99))-1)]
		r.GapMax = gaps[len(gaps)-1]
	}
	return r
}

// G.711 mu-law expansion; 256 is the worst quantization tolerance used for
// the synthetic 10000-amplitude signal. Mismatches also expose dropped frames.
func decodeULaw(b byte) int {
	b = ^b
	v := ((int(b&15) << 3) + 132) << ((b >> 4) & 7)
	if b&128 != 0 {
		return 132 - v
	}
	return v - 132
}
