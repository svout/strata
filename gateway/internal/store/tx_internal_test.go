package store

import (
	"testing"
	"time"
)

// The external tests hard-code the attempt count; keep them honest.
func TestDefaultRetryAttempts(t *testing.T) {
	t.Parallel()
	const externalMaxAttempts = 5 // maxAttempts in tx_test.go
	if defaultRetry.attempts != externalMaxAttempts {
		t.Fatalf("defaultRetry.attempts = %d, tx_test.go maxAttempts = %d: update the test", defaultRetry.attempts, externalMaxAttempts)
	}
}

func TestBackoffStaysWithinBounds(t *testing.T) {
	t.Parallel()
	p := defaultRetry
	const lo, hi = 10 * time.Millisecond, 200 * time.Millisecond
	if p.minBackoff != lo || p.maxBackoff != hi {
		t.Fatalf("defaultRetry backoff = [%v, %v], want [%v, %v]", p.minBackoff, p.maxBackoff, lo, hi)
	}

	// Retries are numbered 1..attempts-1.
	lastRetry := p.attempts - 1
	if got := p.ceiling(lastRetry); got != hi {
		t.Errorf("ceiling(last retry %d) = %v, want %v", lastRetry, got, hi)
	}

	var worst time.Duration
	for n := 1; n <= lastRetry; n++ {
		ceil := p.ceiling(n)
		worst += ceil
		if ceil < lo || ceil > hi {
			t.Errorf("ceiling(%d) = %v, want within [%v, %v]", n, ceil, lo, hi)
		}
		for range 10_000 {
			if d := p.backoff(n); d < lo || d > ceil {
				t.Fatalf("backoff(%d) = %v, want within [%v, %v]", n, d, lo, ceil)
			}
		}
	}
	// tx.go's comment states this figure.
	if want := 480 * time.Millisecond; worst != want {
		t.Errorf("worst-case total wait = %v, want %v", worst, want)
	}
}
