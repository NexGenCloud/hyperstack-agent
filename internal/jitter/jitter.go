package jitter

import (
	cryptorand "crypto/rand"
	"math/big"
	"time"
)

// Duration returns a random duration in [0, max). It returns 0 when max is
// non-positive or the OS random source is unavailable.
func Duration(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	n, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(max)))
	if err != nil {
		return 0
	}
	return time.Duration(n.Int64())
}
