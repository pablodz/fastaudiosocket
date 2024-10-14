# fastaudiosocket

## Install

```bash
go get github.com/pablodz/fastaudiosocket
```

## Benchmarks

- one tcp connection

```bash
goos: linux
goarch: amd64
pkg: github.com/pablodz/fastaudiosocket/fastaudiosocket
cpu: 13th Gen Intel(R) Core(TM) i9-13900H
BenchmarkMessageReading/AudiosocketOne-20         	   11870	     95138 ns/op	   43587 B/op	    3006 allocs/op
BenchmarkMessageReading/FastAudiosocketOne-20     	      90	  17924946 ns/op	73802507 B/op	    1009 allocs/op
PASS
ok  	github.com/pablodz/fastaudiosocket/fastaudiosocket	3.580s
```

- 100 concurrent tcp connections

```bash
goos: linux
goarch: amd64
pkg: github.com/pablodz/fastaudiosocket/fastaudiosocket
cpu: 13th Gen Intel(R) Core(TM) i9-13900H
BenchmarkConcurrentMessageReading/Concurrent_Audiosocket-20         	     606	   2211287 ns/op	 4359221 B/op	  300602 allocs/op
BenchmarkConcurrentMessageReading/Concurrent_FastAudiosocket-20     	       1	2768144937 ns/op	7380447888 B/op	  102880 allocs/op
PASS
ok  	github.com/pablodz/fastaudiosocket/fastaudiosocket	5.238s
```
