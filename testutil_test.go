package main

import (
	"encoding/base64"
	"time"
)

// toBase64 encodes the inline YAML blocks used by tests.
func toBase64(value string) string {
	return base64.StdEncoding.EncodeToString([]byte(value))
}

// sleepShort yields to the stream pump goroutine between assertions.
func sleepShort() {
	time.Sleep(5 * time.Millisecond)
}
