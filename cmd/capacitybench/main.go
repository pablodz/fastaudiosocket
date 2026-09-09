//go:build linux

// capacitybench exercises many AudioSocket calls in one Go sender process.
// All audio is synthetic. Results go to stdout; no measurement files are needed.
package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/exec"
	"runtime"
	"runtime/metrics"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	fa "github.com/pablodz/fastaudiosocket/fastaudiosocket"
)

var (
	calls        = flag.Int("calls", 25, "simultaneous calls in one sender process")
	duration     = flag.Duration("duration", 10*time.Second, "audio per call, multiple of 20ms")
	lead         = flag.Duration("lead", 100*time.Millisecond, "playback lead; zero selects legacy deadlines")
	procs        = flag.Int("procs", 2, "GOMAXPROCS of the shared sender")
	senderCPUs   = flag.String("sender-cpus", "", "optional Linux CPU list for sender via taskset; keep controller on separate CPUs")
	peer         = flag.String("peer", "tcp", "tcp or asterisk")
	codec        = flag.String("codec", "ulaw", "Asterisk native channel codec: slin or ulaw")
	config       = flag.String("asterisk-config", "/lab/config/asterisk.conf", "configuration of the isolated Asterisk lab")
	astPID       = flag.Int("asterisk-pid", 0, "optional Asterisk PID for CPU/RSS measurements")
	aligned      = flag.Bool("aligned", false, "synchronize all playback starts instead of spreading them over 20ms")
	startSpread  = flag.Duration("start-spread", 20*time.Millisecond, "spread audio starts over this interval; aligned overrides it")
	duplex       = flag.Bool("duplex", true, "also send paced synthetic audio toward the library")
	worker       = flag.Bool("worker", false, "internal shared sender subprocess")
	detail       = flag.Bool("details", false, "include individual call measurements on stdout")
	load         = flag.Int("load", 0, "background CPU worker duty, percent of one sender core")
	stall        = flag.Duration("stall", 0, "optional whole-sender SIGSTOP, once halfway through audio")
	profileKind  = flag.String("profile", "", "optional sender diagnostic: cpu, allocs, block, mutex, or trace; use separately from timing comparisons")
	profilePath  = flag.String("profile-output", "", "explicit diagnostic file path; no file is written by default")
	contextScope = flag.String("context-scope", "call", "call gives each call its own cancellable context; shared reproduces the original harness")
)

func check(err error) {
	if err != nil {
		panic(err)
	}
}
func ms(t time.Duration) float64 { return float64(t) / float64(time.Millisecond) }
func cpu() time.Duration {
	var r syscall.Rusage
	check(syscall.Getrusage(syscall.RUSAGE_SELF, &r))
	return time.Duration(r.Utime.Nano() + r.Stime.Nano())
}
func rss(pid int) int64 {
	b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			v, _ := strconv.ParseInt(strings.Fields(line)[1], 10, 64)
			return v
		}
	}
	return 0
}

func affinity(pid int) string {
	b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "Cpus_allowed_list:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "Cpus_allowed_list:"))
		}
	}
	return ""
}

func swapBytes() int64 {
	b, err := os.ReadFile("/sys/fs/cgroup/memory.swap.current")
	if err != nil {
		return -1
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return -1
	}
	return n
}

func procMajorFaults(pid int) int64 {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	s := string(b)
	fields := strings.Fields(s[strings.LastIndex(s, ")")+1:])
	if len(fields) < 10 {
		return 0
	}
	n, _ := strconv.ParseInt(fields[9], 10, 64)
	return n
}
func procCPU(pid int, hz float64) float64 {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	return statCPU(string(b), hz)
}
func statCPU(s string, hz float64) float64 {
	f := strings.Fields(s[strings.LastIndex(s, ")")+1:])
	if len(f) < 13 || hz <= 0 {
		return 0
	}
	u, _ := strconv.ParseFloat(f[11], 64)
	v, _ := strconv.ParseFloat(f[12], 64)
	return (u + v) * 1000 / hz
}
func throttle() [2]uint64 {
	b, _ := os.ReadFile("/sys/fs/cgroup/cpu.stat")
	var v [2]uint64
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		n, _ := strconv.ParseUint(f[1], 10, 64)
		if f[0] == "nr_throttled" {
			v[0] = n
		}
		if f[0] == "throttled_usec" {
			v[1] = n
		}
	}
	return v
}
func tone(frames int) []byte {
	b := make([]byte, frames*320)
	for i := 0; i < len(b)/2; i++ {
		binary.LittleEndian.PutUint16(b[i*2:], uint16(int16(10000*math.Sin(2*math.Pi*437*float64(i)/8000))))
	}
	return b
}
func ulaw(b byte) int {
	b = ^b
	v := ((int(b&15) << 3) + 132) << ((b >> 4) & 7)
	if b&128 != 0 {
		return 132 - v
	}
	return v - 132
}
func encodeULaw(s int16) byte {
	v := int(s)
	mask := byte(0xff)
	if v < 0 {
		v = -v
		mask = 0x7f
	}
	v = min(v, 32635) + 132
	segment := 0
	for n := v >> 8; n > 0; n >>= 1 {
		segment++
	}
	return byte(segment<<4|((v>>(segment+3))&15)) ^ mask
}
func waitUntil(ctx context.Context, at time.Time) bool {
	t := time.NewTimer(max(0, time.Until(at)))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

type senderCall struct {
	Sent           uint64  `json:"sent"`
	Received       int64   `json:"received"`
	RXBytes        int64   `json:"rx_bytes"`
	RXMaxGapMS     float64 `json:"rx_max_gap_ms"`
	RXGapsOver40MS int64   `json:"rx_gaps_over_40ms"`
	Errors         int     `json:"errors"`
	Resyncs        uint64  `json:"resyncs"`
	MaxDispatchMS  float64 `json:"max_dispatch_ms"`
}
type senderStats struct {
	AllocatedBytes uint64         `json:"allocated_bytes"`
	Allocations    uint64         `json:"allocations"`
	Scheduler      schedulerStats `json:"scheduler"`
	MajorFaults    int64          `json:"major_faults"`
	CPUAffinity    string         `json:"cpu_affinity"`
	CPUms          float64        `json:"cpu_ms"`
	WallMS         float64        `json:"wall_ms"`
	RSSKB          int64          `json:"rss_kb"`
	PeakRSSKB      int64          `json:"peak_rss_kb"`
	GCs            uint32         `json:"gc_cycles"`
	GCPauseMS      float64        `json:"gc_pause_ms"`
	Calls          []senderCall   `json:"calls"`
}

func main() {
	flag.Parse()
	check(validateProfile(*profileKind, *profilePath))
	if *contextScope != "call" && *contextScope != "shared" {
		panic("context-scope must be call or shared")
	}
	if *calls < 1 || *duration < 20*time.Millisecond || *duration%(20*time.Millisecond) != 0 || *procs < 1 || *lead < 0 || *lead%(20*time.Millisecond) != 0 || *load < 0 || *load > 100 || *stall < 0 {
		panic("invalid settings")
	}
	if *startSpread < 0 || *startSpread > *duration {
		panic("start-spread must be between zero and duration")
	}
	if *peer != "tcp" && *peer != "asterisk" {
		panic("unsupported peer")
	}
	if *codec != "ulaw" && *codec != "slin" {
		panic("unsupported codec")
	}
	if *worker {
		runSender()
		return
	}
	runController()
}

func runSender() {
	runtime.GOMAXPROCS(*procs)
	ctx, cancel := context.WithTimeout(context.Background(), *duration+3*time.Minute)
	defer cancel()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	check(err)
	defer l.Close()
	context.AfterFunc(ctx, func() { _ = l.Close() })
	fmt.Println(l.Addr())
	var wg sync.WaitGroup
	ready := make(chan struct{}, *calls)
	start := make(chan struct{})
	finished := make(chan struct{}, *calls)
	r := senderStats{Calls: make([]senderCall, *calls)}
	rx := make([]atomic.Int64, *calls)
	rxBytes := make([]atomic.Int64, *calls)
	rxGap := make([]atomic.Int64, *calls)
	rxLong := make([]atomic.Int64, *calls)
	audio := tone(int(*duration / (20 * time.Millisecond)))
	for i := 0; i < *calls; i++ {
		conn, e := l.Accept()
		check(e)
		wg.Add(1)
		go func(index int, conn net.Conn) {
			defer wg.Done()
			defer conn.Close()
			// A shared Done channel in every playback/producer select introduces
			// cross-call contention. Keep that mode for historical comparisons,
			// but model independent call lifetimes by default.
			ctx := ctx
			if *contextScope == "call" {
				var cancelCall context.CancelFunc
				ctx, cancelCall = context.WithCancel(ctx)
				defer cancelCall()
			}
			s, e := fa.NewFastAudioSocket(ctx, conn, false, false)
			check(e)
			// The controller encodes its call index in the UUID; accept order can vary.
			id := strings.ReplaceAll(s.GetUUID(), "-", "")
			n, e := strconv.ParseUint(id[len(id)-8:], 16, 32)
			check(e)
			index = int(n) - 1
			if index < 0 || index >= *calls {
				panic("bad synthetic UUID")
			}
			check(s.SetPlaybackOptions(fa.PlaybackOptions{Lead: *lead}))
			rxDone := make(chan struct{})
			go func() {
				defer close(rxDone)
				var last time.Time
				for p := range s.AudioChan {
					if !p.SilenceSuppressed && p.Type == fa.PacketTypeAudio {
						now := time.Now()
						if !last.IsZero() {
							gap := now.Sub(last)
							if int64(gap) > rxGap[index].Load() {
								rxGap[index].Store(int64(gap))
							}
							if gap > 40*time.Millisecond {
								rxLong[index].Add(1)
							}
						}
						last = now
						rx[index].Add(1)
						rxBytes[index].Add(int64(len(p.Payload)))
					}
				}
			}()
			ready <- struct{}{}
			select {
			case <-ctx.Done():
				return
			case <-start:
			}
			if !*aligned {
				waitUntil(ctx, time.Now().Add(time.Duration(index)*(*startSpread)/time.Duration(*calls)))
			}
			chunks := make(chan []byte, 2)
			go func() {
				defer close(chunks)
				for pos := 0; pos < len(audio); {
					end := min(pos+3200, len(audio))
					for _, b := range [][]byte{audio[pos:min(pos+197, end)], audio[min(pos+197, end):end]} {
						select {
						case <-ctx.Done():
							return
						case chunks <- b:
						}
					}
					pos = end
				}
			}()
			e = s.PlayStreaming(ctx, chunks, nil)
			stats := s.PlaybackStats()
			r.Calls[index] = senderCall{Sent: stats.PacketsSent, Resyncs: stats.Resyncs, MaxDispatchMS: ms(stats.MaxScheduleLateness)}
			if e != nil {
				r.Calls[index].Errors++
			}
			finished <- struct{}{}
			<-ctx.Done()
			_ = conn.Close()
			<-rxDone
		}(i, conn)
	}
	for i := 0; i < *calls; i++ {
		select {
		case <-ready:
		case <-ctx.Done():
			panic("handshake timeout")
		}
	}
	time.Sleep(300 * time.Millisecond)
	fmt.Println("ready")
	scan := bufio.NewScanner(os.Stdin)
	if !scan.Scan() || scan.Text() != "start" {
		panic("missing start")
	}
	stopProfile := startProfile(*profileKind, *profilePath)
	defer stopProfile()
	loadCtx, stopLoad := context.WithCancel(ctx)
	loadDone := make(chan struct{})
	go func() {
		defer close(loadDone)
		if *load == 0 {
			return
		}
		for loadCtx.Err() == nil {
			at := time.Now()
			until := at.Add(time.Duration(*load) * time.Millisecond)
			for time.Now().Before(until) {
				if loadCtx.Err() != nil {
					return
				}
			}
			if !waitUntil(loadCtx, at.Add(100*time.Millisecond)) {
				return
			}
		}
	}()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	schedBefore := schedulerSnapshot()
	var usageBefore syscall.Rusage
	check(syscall.Getrusage(syscall.RUSAGE_SELF, &usageBefore))
	cpuStart := cpu()
	started := time.Now()
	fmt.Println("playing")
	close(start)
	for i := 0; i < *calls; i++ {
		select {
		case <-finished:
		case <-ctx.Done():
			panic("playback timeout")
		}
	}
	// Include inbound audio and a bounded 200ms drain in the CPU window.
	waitUntil(ctx, started.Add(*duration+200*time.Millisecond))
	r.CPUms = ms(cpu() - cpuStart)
	r.WallMS = ms(time.Since(started))
	stopLoad()
	<-loadDone
	runtime.ReadMemStats(&after)
	r.Scheduler = schedulerDelta(schedBefore, schedulerSnapshot())
	r.AllocatedBytes = after.TotalAlloc - before.TotalAlloc
	r.Allocations = after.Mallocs - before.Mallocs
	r.GCs = after.NumGC - before.NumGC
	r.GCPauseMS = float64(after.PauseTotalNs-before.PauseTotalNs) / 1e6
	r.RSSKB = rss(os.Getpid())
	var usage syscall.Rusage
	check(syscall.Getrusage(syscall.RUSAGE_SELF, &usage))
	r.MajorFaults = usage.Majflt - usageBefore.Majflt
	r.PeakRSSKB = max(usage.Maxrss, r.RSSKB)
	r.CPUAffinity = affinity(os.Getpid())
	fmt.Println("done")
	stopProfile()
	// Controller finishes inbound validation before allowing teardown.
	if !scan.Scan() || scan.Text() != "stop" {
		panic("missing stop")
	}
	for i := range r.Calls {
		r.Calls[i].Received = rx[i].Load()
		r.Calls[i].RXBytes = rxBytes[i].Load()
		r.Calls[i].RXMaxGapMS = ms(time.Duration(rxGap[i].Load()))
		r.Calls[i].RXGapsOver40MS = rxLong[i].Load()
	}
	check(json.NewEncoder(os.Stdout).Encode(r))
	cancel()
	wg.Wait()
}

type callMetrics struct {
	Packets            int     `json:"packets"`
	PayloadErrors      int     `json:"payload_errors"`
	SequenceMissing    int     `json:"sequence_missing"`
	TimestampJumps     int     `json:"timestamp_jumps"`
	ClockGapMS         float64 `json:"rtp_clock_gap_ms"`
	FIFOGapMS          float64 `json:"fifo_gap_ms"`
	PeakFIFOms         float64 `json:"peak_fifo_ms"`
	Late100            int     `json:"late_100ms"`
	KernelDrops        uint32  `json:"kernel_drops"`
	KernelTimestamps   int     `json:"kernel_timestamps"`
	ObserverDelayMaxMS float64 `json:"observer_delay_max_ms"`
	GapP99MS           float64 `json:"gap_p99_ms"`
	GapMaxMS           float64 `json:"gap_max_ms"`
	MaxGapAtMS         float64 `json:"max_gap_at_ms"`
	FirstUnderrunMS    float64 `json:"first_underrun_ms"`
	FirstClockJumpMS   float64 `json:"first_clock_jump_ms"`
	IncomingSent       int     `json:"incoming_sent"`
	IOErrors           int     `json:"io_errors"`
	first, last, end   time.Time
	seq                uint16
	ts                 uint32
	index              int
	gaps               []float64
}

func (m *callMetrics) consume(at time.Time, payload []byte, seq uint16, ts uint32, rtp bool, audio []byte) {
	index := m.Packets
	if m.Packets == 0 {
		m.first = at
		m.end = at
	} else {
		gap := ms(at.Sub(m.last))
		m.gaps = append(m.gaps, gap)
		if gap > m.GapMaxMS {
			m.MaxGapAtMS = ms(at.Sub(m.first))
		}
		m.GapMaxMS = max(m.GapMaxMS, gap)
		if rtp {
			delta := int(uint16(seq - m.seq))
			if delta != 1 {
				m.SequenceMissing += max(0, delta-1)
			}
			index = m.index + delta
			clock := int64(int32(ts-m.ts)) - 160*int64(delta)
			if clock != 0 {
				if m.TimestampJumps == 0 {
					m.FirstClockJumpMS = ms(at.Sub(m.first))
				}
				m.TimestampJumps++
				m.ClockGapMS += float64(max(0, clock)) / 8
			}
		}
		if at.After(m.end) {
			if m.FIFOGapMS == 0 {
				m.FirstUnderrunMS = ms(at.Sub(m.first))
			}
			m.FIFOGapMS += ms(at.Sub(m.end))
		}
	}
	if at.After(m.end) {
		m.end = at
	}
	m.end = m.end.Add(20 * time.Millisecond)
	m.PeakFIFOms = max(m.PeakFIFOms, ms(m.end.Sub(at)))
	if at.Sub(m.first.Add(time.Duration(index)*20*time.Millisecond)) > 100*time.Millisecond {
		m.Late100++
	}
	mismatch := index < 0 || (index+1)*320 > len(audio)
	if rtp {
		mismatch = mismatch || len(payload) != 160
		if !mismatch {
			for j, b := range payload {
				want := int(int16(binary.LittleEndian.Uint16(audio[index*320+j*2:])))
				if abs(ulaw(b)-want) > 256 {
					mismatch = true
					break
				}
			}
		}
	} else {
		mismatch = mismatch || len(payload) != 320
		if !mismatch {
			mismatch = string(payload) != string(audio[index*320:(index+1)*320])
		}
	}
	if mismatch {
		m.PayloadErrors++
	}
	m.Packets++
	m.last = at
	m.seq = seq
	m.ts = ts
	m.index = index
}
func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
func percentile(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	sort.Float64s(v)
	return v[max(0, min(len(v)-1, int(math.Ceil(float64(len(v))*p))-1))]
}
func kernelTime(data []byte) (time.Time, bool) {
	var sec, ns int64
	switch len(data) {
	case 16:
		sec = int64(binary.NativeEndian.Uint64(data))
		ns = int64(binary.NativeEndian.Uint64(data[8:]))
	case 8:
		sec = int64(int32(binary.NativeEndian.Uint32(data)))
		ns = int64(int32(binary.NativeEndian.Uint32(data[4:])))
	default:
		return time.Time{}, false
	}
	if ns < 0 || ns >= 1e9 {
		return time.Time{}, false
	}
	return time.Unix(sec, ns), true
}
func timestampSocket(c *net.UDPConn) {
	check(c.SetReadBuffer(4 << 20))
	raw, err := c.SyscallConn()
	check(err)
	var optionErr error
	check(raw.Control(func(fd uintptr) {
		optionErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_TIMESTAMPNS, 1)
		if optionErr == nil {
			optionErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, 40, 1)
		}
	}))
	check(optionErr)
}

type controllerCall struct {
	tcp          net.Conn
	udp          *net.UDPConn
	metrics      callMetrics
	receiverDone chan struct{}
	inputDone    chan struct{}
	inputStart   chan *net.UDPAddr
}

func receive(ctx context.Context, c *controllerCall, audio []byte) {
	defer close(c.receiverDone)
	buf := make([]byte, 2048)
	oob := make([]byte, 128)
	for {
		if c.tcp != nil {
			var h [3]byte
			if _, e := io.ReadFull(c.tcp, h[:]); e != nil {
				if ctx.Err() == nil && e != io.EOF {
					c.metrics.IOErrors++
				}
				return
			}
			n := int(binary.BigEndian.Uint16(h[1:]))
			if n > len(buf) {
				c.metrics.IOErrors++
				return
			}
			if _, e := io.ReadFull(c.tcp, buf[:n]); e != nil {
				if ctx.Err() == nil {
					c.metrics.IOErrors++
				}
				return
			}
			if h[0] == fa.PacketTypeAudio {
				c.metrics.consume(time.Now(), buf[:n], 0, 0, false, audio)
			}
		} else {
			n, on, flags, from, e := c.udp.ReadMsgUDP(buf, oob)
			if e != nil {
				if ctx.Err() == nil {
					c.metrics.IOErrors++
				}
				return
			}
			if flags&(syscall.MSG_TRUNC|syscall.MSG_CTRUNC) != 0 || n < 12 || buf[0] != 0x80 || buf[1]&0x7f != 0 {
				c.metrics.IOErrors++
				continue
			}
			now := time.Now()
			at := now
			controls, e := syscall.ParseSocketControlMessage(oob[:on])
			check(e)
			for _, control := range controls {
				if control.Header.Level != syscall.SOL_SOCKET {
					continue
				}
				if control.Header.Type == syscall.SCM_TIMESTAMPNS {
					if t, ok := kernelTime(control.Data); ok {
						at = t
						c.metrics.KernelTimestamps++
					}
				}
				if control.Header.Type == 40 && len(control.Data) >= 4 {
					c.metrics.KernelDrops = binary.NativeEndian.Uint32(control.Data)
				}
			}
			c.metrics.ObserverDelayMaxMS = max(c.metrics.ObserverDelayMaxMS, ms(now.Sub(at)))
			if c.metrics.Packets == 0 {
				c.inputStart <- from
			}
			c.metrics.consume(at, buf[12:n], binary.BigEndian.Uint16(buf[2:]), binary.BigEndian.Uint32(buf[4:]), true, audio)
		}
	}
}
func input(ctx context.Context, c *controllerCall, start <-chan struct{}, audio []byte) {
	defer close(c.inputDone)
	if !*duplex {
		return
	}
	select {
	case <-ctx.Done():
		return
	case <-start:
	}
	var target *net.UDPAddr
	if c.udp != nil {
		select {
		case <-ctx.Done():
			return
		case target = <-c.inputStart:
		}
	}
	at := time.Now()
	buf := make([]byte, 323)
	for i := 0; i < len(audio)/320; i++ {
		if !waitUntil(ctx, at.Add(time.Duration(i)*20*time.Millisecond)) {
			return
		}
		var err error
		if c.tcp != nil {
			copy(buf, []byte{0x10, 1, 64})
			copy(buf[3:], audio[i*320:(i+1)*320])
			_, err = c.tcp.Write(buf)
		} else {
			buf[0] = 0x80
			buf[1] = 0
			binary.BigEndian.PutUint16(buf[2:], uint16(i))
			binary.BigEndian.PutUint32(buf[4:], uint32(i*160))
			binary.BigEndian.PutUint32(buf[8:], 12345)
			for j := 0; j < 160; j++ {
				buf[12+j] = encodeULaw(int16(binary.LittleEndian.Uint16(audio[i*320+j*2:])))
			}
			_, err = c.udp.WriteToUDP(buf[:172], target)
		}
		if err != nil {
			return
		}
		c.metrics.IncomingSent++
	}
}

func runController() {
	frames := int(*duration / (20 * time.Millisecond))
	audio := tone(frames)
	args := []string{"-worker", "-calls", strconv.Itoa(*calls), "-duration", duration.String(), "-lead", lead.String(), "-procs", strconv.Itoa(*procs), "-load", strconv.Itoa(*load), "-aligned=" + strconv.FormatBool(*aligned), "-start-spread", startSpread.String(), "-profile", *profileKind, "-profile-output", *profilePath, "-context-scope", *contextScope}
	cmd := exec.Command(os.Args[0], args...)
	if *senderCPUs != "" {
		cmd = exec.Command("taskset", append([]string{"-c", *senderCPUs, os.Args[0]}, args...)...)
	}
	stdout, e := cmd.StdoutPipe()
	check(e)
	stdin, e := cmd.StdinPipe()
	check(e)
	cmd.Stderr = os.Stderr
	check(cmd.Start())
	defer func() { _ = cmd.Process.Signal(syscall.SIGCONT); _ = cmd.Process.Kill() }()
	scan := bufio.NewScanner(stdout)
	scan.Buffer(make([]byte, 4096), 8<<20)
	if !scan.Scan() {
		panic("sender failed to listen")
	}
	addr := scan.Text()
	ctx, cancel := context.WithTimeout(context.Background(), *duration+3*time.Minute)
	defer cancel()
	connections := make([]controllerCall, *calls)
	startInput := make(chan struct{})
	defer func() {
		cancel()
		for i := range connections {
			c := &connections[i]
			if c.tcp != nil {
				_ = c.tcp.Close()
			}
			if c.udp != nil {
				_ = c.udp.Close()
			}
		}
	}()
	setupStart := time.Now()
	for i := range connections {
		c := &connections[i]
		c.receiverDone = make(chan struct{})
		c.inputDone = make(chan struct{})
		c.inputStart = make(chan *net.UDPAddr, 1)
		c.metrics.gaps = make([]float64, 0, frames)
		if *peer == "tcp" {
			c.tcp, e = net.Dial("tcp", addr)
			check(e)
			uuid := make([]byte, 19)
			copy(uuid, []byte{1, 0, 16})
			uuid[9] = 0x40
			uuid[11] = 0x80
			binary.BigEndian.PutUint32(uuid[15:], uint32(i+1))
			_, e = c.tcp.Write(uuid)
			check(e)
		} else {
			c.udp, e = net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
			check(e)
			timestampSocket(c.udp)
			port := c.udp.LocalAddr().(*net.UDPAddr).Port
			id := fmt.Sprintf("00000000-0000-4000-8000-%012x", i+1)
			originate := fmt.Sprintf("channel originate AudioSocket/%s/%s/c(%s) extension 100*%d@bench", addr, id, *codec, port)
			out, err := exec.CommandContext(ctx, "asterisk", "-C", *config, "-rx", originate).CombinedOutput()
			if err != nil {
				panic(fmt.Sprintf("originate: %v: %s", err, out))
			}
		}
		go receive(ctx, c, audio)
		go input(ctx, c, startInput, audio)
	}
	if !scan.Scan() || scan.Text() != "ready" {
		panic("sender not ready")
	}
	setupMS := ms(time.Since(setupStart))
	hz := 100.0
	if *astPID > 0 {
		b, err := exec.Command("getconf", "CLK_TCK").Output()
		check(err)
		hz, e = strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
		check(e)
	}
	astBefore := procCPU(*astPID, hz)
	astFaultsBefore := procMajorFaults(*astPID)
	swapBefore := swapBytes()
	controllerBefore := cpu()
	throttleBefore := throttle()
	started := time.Now()
	_, e = fmt.Fprintln(stdin, "start")
	check(e)
	if !scan.Scan() || scan.Text() != "playing" {
		panic("sender did not start")
	}
	close(startInput)
	stopStall := make(chan struct{})
	stallDone := make(chan struct{})
	go func() {
		defer close(stallDone)
		if *stall == 0 {
			return
		}
		timer := time.NewTimer(*duration / 2)
		defer timer.Stop()
		select {
		case <-stopStall:
			return
		case <-timer.C:
		}
		check(cmd.Process.Signal(syscall.SIGSTOP))
		timer.Reset(*stall)
		select {
		case <-stopStall:
		case <-timer.C:
		}
		_ = cmd.Process.Signal(syscall.SIGCONT)
	}()
	if !scan.Scan() || scan.Text() != "done" {
		panic("sender did not complete playback")
	}
	close(stopStall)
	<-stallDone
	controllerCPU := ms(cpu() - controllerBefore)
	wall := ms(time.Since(started))
	astCPU := procCPU(*astPID, hz) - astBefore
	astRSS := rss(*astPID)
	astMajorFaults := procMajorFaults(*astPID) - astFaultsBefore
	swapAfter := swapBytes()
	throttleAfter := throttle()
	for i := range connections {
		<-connections[i].inputDone
	}
	waitUntil(ctx, time.Now().Add(100*time.Millisecond))
	_, e = fmt.Fprintln(stdin, "stop")
	check(e)
	if !scan.Scan() {
		panic("missing sender metrics")
	}
	var sender senderStats
	check(json.Unmarshal(scan.Bytes(), &sender))
	check(scan.Err())
	check(cmd.Wait())
	cancel()
	for i := range connections {
		c := &connections[i]
		if c.tcp != nil {
			c.tcp.Close()
		}
		if c.udp != nil {
			c.udp.Close()
		}
		<-c.receiverDone
		c.metrics.GapP99MS = percentile(c.metrics.gaps, .99)
	}
	summary := summarize(connections, sender, frames)
	summary["peer"] = *peer
	summary["codec"] = *codec
	summary["calls"] = *calls
	summary["frames_per_call"] = frames
	summary["lead_ms"] = ms(*lead)
	summary["duration_ms"] = ms(*duration)
	summary["sender_gomaxprocs"] = *procs
	summary["sender_affinity"] = sender.CPUAffinity
	summary["controller_affinity"] = affinity(os.Getpid())
	summary["asterisk_affinity"] = affinity(*astPID)
	summary["aligned"] = *aligned
	summary["start_spread_ms"] = ms(*startSpread)
	summary["duplex"] = *duplex
	summary["load_duty_pct"] = *load
	summary["stall_ms"] = ms(*stall)
	summary["sender_cpu_cores"] = sender.CPUms / sender.WallMS
	summary["sender_major_faults"] = sender.MajorFaults
	summary["asterisk_major_faults"] = astMajorFaults
	summary["cgroup_swap_before_bytes"] = swapBefore
	summary["cgroup_swap_after_bytes"] = swapAfter
	summary["sender_cpu_ms"] = sender.CPUms
	summary["sender_wall_ms"] = sender.WallMS
	summary["sender_rss_mb"] = float64(sender.RSSKB) / 1024
	summary["sender_peak_rss_mb"] = float64(sender.PeakRSSKB) / 1024
	summary["sender_gc_cycles"] = sender.GCs
	summary["sender_gc_pause_ms"] = sender.GCPauseMS
	summary["asterisk_cpu_cores"] = astCPU / wall
	summary["asterisk_rss_mb"] = float64(astRSS) / 1024
	summary["controller_cpu_cores"] = controllerCPU / wall
	summary["controller_rss_mb"] = float64(rss(os.Getpid())) / 1024
	summary["setup_ms"] = setupMS
	summary["cgroup_throttled_periods"] = throttleAfter[0] - throttleBefore[0]
	summary["cgroup_throttled_ms"] = float64(throttleAfter[1]-throttleBefore[1]) / 1000
	summary["go_version"] = runtime.Version()
	summary["profile"] = *profileKind
	summary["context_scope"] = *contextScope
	summary["sender_allocated_bytes"] = sender.AllocatedBytes
	summary["sender_allocations"] = sender.Allocations
	summary["sender_scheduler"] = sender.Scheduler
	if *detail {
		metrics := make([]callMetrics, len(connections))
		for i := range connections {
			metrics[i] = connections[i].metrics
		}
		summary["per_call"] = metrics
		summary["sender_per_call"] = sender.Calls
	}
	check(json.NewEncoder(os.Stdout).Encode(summary))
}

func schedulerSnapshot() metrics.Float64Histogram {
	samples := []metrics.Sample{{Name: "/sched/latencies:seconds"}}
	metrics.Read(samples)
	h := samples[0].Value.Float64Histogram()
	return metrics.Float64Histogram{Buckets: append([]float64(nil), h.Buckets...), Counts: append([]uint64(nil), h.Counts...)}
}

func summarize(connections []controllerCall, sender senderStats, frames int) map[string]any {
	packets, badPayload, missingSeq, tsJumps, callsShort, ioErrors, rxShort, rxFrames, inputFrames, late100, kernelTS := 0, 0, 0, 0, 0, 0, 0, int64(0), 0, 0, 0
	var drops uint64
	var sent, resyncs uint64
	fifo := make([]float64, 0, len(connections))
	gaps := make([]float64, 0, len(connections))
	dispatch := make([]float64, 0, len(connections))
	clockGap, observer, fifoSum, maxGap := 0.0, 0.0, 0.0, 0.0
	rxGaps := make([]float64, 0, len(connections))
	var rxLongGaps int64
	underrunCalls, startupUnderruns := 0, 0
	firstGaps, firstJumps := []float64{}, []float64{}
	for i, c := range connections {
		m := c.metrics
		s := sender.Calls[i]
		packets += m.Packets
		badPayload += m.PayloadErrors
		missingSeq += m.SequenceMissing
		tsJumps += m.TimestampJumps
		ioErrors += m.IOErrors + s.Errors
		late100 += m.Late100
		kernelTS += m.KernelTimestamps
		drops += uint64(m.KernelDrops)
		sent += s.Sent
		resyncs += s.Resyncs
		rxFrames += s.Received
		rxGaps = append(rxGaps, s.RXMaxGapMS)
		rxLongGaps += s.RXGapsOver40MS
		inputFrames += m.IncomingSent
		if m.Packets != frames {
			callsShort++
		}
		if *duplex && s.Received != int64(frames) {
			rxShort++
		}
		fifo = append(fifo, m.FIFOGapMS)
		gaps = append(gaps, m.GapP99MS)
		dispatch = append(dispatch, s.MaxDispatchMS)
		fifoSum += m.FIFOGapMS
		if m.FIFOGapMS > 0 {
			underrunCalls++
			firstGaps = append(firstGaps, m.FirstUnderrunMS)
			if m.FirstUnderrunMS <= 200 {
				startupUnderruns++
			}
		}
		if m.TimestampJumps > 0 {
			firstJumps = append(firstJumps, m.FirstClockJumpMS)
		}
		clockGap += m.ClockGapMS
		observer = max(observer, m.ObserverDelayMaxMS)
		maxGap = max(maxGap, m.GapMaxMS)
	}
	return map[string]any{
		"underrun_calls": underrunCalls, "first_underrun_within_200ms_calls": startupUnderruns,
		"first_underrun_p95_ms": percentile(firstGaps, .95), "first_clock_jump_p95_ms": percentile(firstJumps, .95),
		"packets":                  packets,
		"sent":                     sent,
		"calls_wrong_count":        callsShort,
		"payload_errors":           badPayload,
		"sequence_missing":         missingSeq,
		"timestamp_jumps":          tsJumps,
		"rtp_clock_gap_ms_sum":     clockGap,
		"io_errors":                ioErrors,
		"rx_frames":                rxFrames,
		"input_frames":             inputFrames,
		"rx_calls_wrong_count":     rxShort,
		"kernel_drops":             drops,
		"kernel_timestamps":        kernelTS,
		"observer_delay_max_ms":    observer,
		"fifo_gap_mean_ms":         fifoSum / float64(len(connections)),
		"fifo_gap_p95_ms":          percentile(fifo, .95),
		"fifo_gap_max_ms":          percentile(fifo, 1),
		"call_gap_p99_p95_ms":      percentile(gaps, .95),
		"gap_max_ms":               maxGap,
		"late_100ms":               late100,
		"resyncs":                  resyncs,
		"call_max_dispatch_p95_ms": percentile(dispatch, .95),
		"max_dispatch_ms":          percentile(dispatch, 1),
		"rx_call_max_gap_p95_ms":   percentile(rxGaps, .95),
		"rx_max_gap_ms":            percentile(rxGaps, 1),
		"rx_gaps_over_40ms":        rxLongGaps,
	}
}
