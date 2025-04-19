# FastAudioSocket

FastAudioSocket is a Go package for handling audio and DTMF packets over a custom audiosocket protocol.

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

All packages sent by asterisk to audiosocket are read by the function `ReadPackage` in the file `audiosocket.go`. Now the package payload in ulaw contains 160 bytes and for pcm 320 bytes.

- Write packages

All write packages are 320 bytes + 3 headers. The main difference here is that we need to handle from audiosocket server the timing to sent each packet (head+payload) to asterisk on each 20ms. Write only accepts 320 bytes of payload in pcm linear 16 format. (Remember to remove the headers if you are sending the chunk in streaming)

## Examples

Check the examples folder to see how to use the package.
