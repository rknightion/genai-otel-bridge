// SPDX-License-Identifier: Apache-2.0

package otlp

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/emit"
)

// TestTransportErrorIsRetried [#399]: a transport error (nothing listening) is retryable, so the emitter
// makes more than one attempt within its budget and returns a RetryableError, on which the runner does
// not advance. Classifying it as non-retryable would give up after one attempt.
func TestTransportErrorIsRetried(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // the port is now closed: every dial is refused
	var attempts atomic.Int32
	e := New(Config{
		Endpoint: "http://" + addr, InstanceID: "123", Token: "secret-token", MaxBytes: 1 << 20,
		Retry: RetryPolicy{InitialDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond, Multiplier: 1.5, MaxElapsed: 200 * time.Millisecond},
		Observer: func(_ string, status int, err error, _ time.Duration) {
			if err != nil && status == 0 {
				attempts.Add(1)
			}
		},
	})
	err = e.Emit(context.Background(), oneBatch())
	var re *emit.RetryableError
	if !errors.As(err, &re) {
		t.Fatalf("transport failure returned %v (%T), want a RetryableError", err, err)
	}
	if n := attempts.Load(); n < 2 {
		t.Fatalf("transport failure made %d attempt(s), want more than one (it is retryable)", n)
	}
}
