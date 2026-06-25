// SPDX-License-Identifier: AGPL-3.0-only

// Package emit defines the Emitter seam (ARCHITECTURE.md §5) and the error taxonomy the
// scheduler uses to choose advance-past-with-gap (F9) vs retry vs skip+alert.
package emit

import (
	"context"
	"fmt"

	"github.com/rknightion/genai-otel-bridge/internal/model"
)

// Emitter ships a Batch to a backend. v1: hand-encoded OTLP/HTTP. FROZEN.
type Emitter interface {
	Emit(ctx context.Context, b model.Batch) error
}

type RejectReason int

const (
	ReasonNone RejectReason = iota
	ReasonDuplicateTimestamp
	ReasonTooOld
	ReasonPayloadTooLarge // 413 after splitting to a minimal chunk
	ReasonBadEncoding     // POSITIVELY-identified malformed payload — a real bug, halt + alert (no advance)
	ReasonUnknown         // [CP-C7] unrecognised request-level 4xx — HALT + alert (degrade), NOT advance-past
) //         (advancing past a generic 400 = silent data loss on misconfig; the loop backs off)

// RejectError is a non-retryable gateway rejection (a 400/413). The scheduler advances the
// monotonic watermark past the offending bucket iff AdvancesPast() (F9/Cdx-C2).
type RejectError struct {
	Reason RejectReason
	Status int
	Msg    string
}

func (e *RejectError) Error() string {
	return fmt.Sprintf("emit rejected (status=%d reason=%d): %s", e.Status, e.Reason, e.Msg)
}

// AdvancesPast: duplicate-timestamp/too-old/413 advance past with a counted gap; bad-encoding
// must NOT advance (it is a bug that would silently lose data forever if advanced).
func (e *RejectError) AdvancesPast() bool {
	switch e.Reason {
	case ReasonDuplicateTimestamp, ReasonTooOld, ReasonPayloadTooLarge:
		return true // known per-sample Mimir rejects → skip-with-gap, loop progresses (F9)
	default: // [CP-C7] ReasonBadEncoding + ReasonUnknown → halt + degrade (no silent advance/loss)
		return false
	}
}

// RetryableError is a transient failure whose retry budget was exhausted (5xx/429/transport).
// The scheduler does NOT advance the watermark; the window is re-pulled next tick.
type RetryableError struct {
	Status int
	Err    error
}

func (e *RetryableError) Error() string {
	return fmt.Sprintf("emit retryable-exhausted (status=%d): %v", e.Status, e.Err)
}
func (e *RetryableError) Unwrap() error { return e.Err }
