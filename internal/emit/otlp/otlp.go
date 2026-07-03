// SPDX-License-Identifier: AGPL-3.0-only

package otlp

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rknightion/genai-otel-bridge/internal/emit"
	"github.com/rknightion/genai-otel-bridge/internal/model"
)

type RetryPolicy struct {
	InitialDelay, MaxDelay, MaxElapsed time.Duration
	Multiplier, Jitter                 float64
}

// DefaultRetryPolicy matches the Alloy policy validated against the GC gateway.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{InitialDelay: 5 * time.Second, MaxDelay: 30 * time.Second, MaxElapsed: 5 * time.Minute, Multiplier: 1.5, Jitter: 0.5}
}

type Config struct {
	Endpoint   string
	InstanceID string
	Token      string
	Identity   map[string]string
	MaxBytes   int
	Retry      RetryPolicy
}

type Emitter struct {
	cfg     Config
	url     string
	logsURL string
	auth    string
	hc      *http.Client
	// now/sleep are injectable so the retry loop (backoff + Retry-After honouring) is deterministically
	// testable without real wall-clock waits. Production uses time.Now and a ctx-aware timer sleep.
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

func New(cfg Config) *Emitter {
	if cfg.Retry == (RetryPolicy{}) {
		cfg.Retry = DefaultRetryPolicy()
	}
	base := strings.TrimRight(cfg.Endpoint, "/")
	// [CP-M7] No credential, no header. A token-less in-cluster hop (emit to an Alloy that itself holds
	// the Grafana Cloud credentials) sends NO Authorization rather than a useless "Basic Og==" (base64 of
	// ":") over cleartext — so nothing credential-shaped ever rides the wire.
	auth := ""
	if cfg.InstanceID != "" || cfg.Token != "" {
		auth = "Basic " + base64.StdEncoding.EncodeToString([]byte(cfg.InstanceID+":"+cfg.Token))
	}
	return &Emitter{
		cfg:     cfg,
		url:     base + "/v1/metrics",
		logsURL: base + "/v1/logs",
		auth:    auth,
		// [#29] The emit leg carries the actual telemetry payload, so it must NOT follow redirects.
		// Go's default policy follows up to 10 redirects and rewrites a 301/302/303 POST into a body-less
		// GET; a 2xx from the redirect target would then be classified as a successful emit and the
		// epoch-fenced watermark would advance past a window whose bytes never reached the gateway
		// (permanent silent data loss — the exact hazard the operational-honesty rule forbids).
		// ErrUseLastResponse surfaces the 3xx as its own status code, which post() classifies as a
		// non-retryable RetryableError → no advance, alertable via emit_errors_total. (The vendor-pull
		// httpx client already blocks cross-host redirects; this closes the same gap on the emit leg.)
		hc: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		now: time.Now,
		sleep: func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		},
	}
}

// Emit encodes the batch's samples and/or logs and POSTs them. Samples go to /v1/metrics, logs to
// /v1/logs. A batch is normally one or the other; if both are present, samples are emitted first and
// the first error is returned (F40).
func (e *Emitter) Emit(ctx context.Context, b model.Batch) error {
	if len(b.Samples) > 0 {
		if err := e.emitSamples(ctx, b.Samples); err != nil {
			return err
		}
	}
	if len(b.Logs) > 0 {
		if err := e.emitLogs(ctx, b.Logs); err != nil {
			return err
		}
	}
	return nil
}

func (e *Emitter) emitSamples(ctx context.Context, samples []model.Sample) error {
	body, err := Encode(e.cfg.Identity, samples)
	if err != nil {
		return &emit.RejectError{Reason: emit.ReasonBadEncoding, Status: 0, Msg: err.Error()}
	}
	gz, err := gzipBytes(body)
	if err != nil {
		return fmt.Errorf("otlp: gzip: %w", err)
	}
	// [CP-C11] Proactively split when the encoded payload exceeds MaxBytes, rather than only
	// reacting to a gateway 413 — bounds request size + memory before transmit.
	if e.cfg.MaxBytes > 0 && len(gz) > e.cfg.MaxBytes && len(samples) > 1 {
		mid := len(samples) / 2
		if err := e.emitSamples(ctx, samples[:mid]); err != nil {
			return err
		}
		return e.emitSamples(ctx, samples[mid:])
	}
	rerr := e.post(ctx, e.url, gz)
	var re *emit.RejectError
	if errors.As(rerr, &re) && re.Reason == emit.ReasonPayloadTooLarge && len(samples) > 1 {
		mid := len(samples) / 2
		if err := e.emitSamples(ctx, samples[:mid]); err != nil {
			return err
		}
		return e.emitSamples(ctx, samples[mid:])
	}
	return rerr
}

// emitLogs encodes log records and POSTs them to /v1/logs, splitting on 413 (F40).
// NOTE: the logs reject taxonomy re-uses classify() unchanged. classify() already matches Loki's
// distributor ordering/age reject strings ("entry out of order", "too far behind", "greater than
// max age") alongside the Mimir err-mimir-* codes — see followup.md's Portkey logs_export row
// (BUILT, regression-tested). An unrecognised gateway reject still maps to ReasonUnknown, on which
// the runner halts+degrades rather than silently advancing.
func (e *Emitter) emitLogs(ctx context.Context, logs []model.LogRecord) error {
	body, err := EncodeLogs(e.cfg.Identity, logs)
	if err != nil {
		return &emit.RejectError{Reason: emit.ReasonBadEncoding, Status: 0, Msg: err.Error()}
	}
	gz, err := gzipBytes(body)
	if err != nil {
		return fmt.Errorf("otlp: gzip logs: %w", err)
	}
	// [CP-C11] Proactively split when the encoded payload exceeds MaxBytes.
	if e.cfg.MaxBytes > 0 && len(gz) > e.cfg.MaxBytes && len(logs) > 1 {
		mid := len(logs) / 2
		if err := e.emitLogs(ctx, logs[:mid]); err != nil {
			return err
		}
		return e.emitLogs(ctx, logs[mid:])
	}
	rerr := e.post(ctx, e.logsURL, gz)
	var re *emit.RejectError
	if errors.As(rerr, &re) && re.Reason == emit.ReasonPayloadTooLarge && len(logs) > 1 {
		mid := len(logs) / 2
		if err := e.emitLogs(ctx, logs[:mid]); err != nil {
			return err
		}
		return e.emitLogs(ctx, logs[mid:])
	}
	return rerr
}

// post runs the retry loop and classifies the terminal outcome.
func (e *Emitter) post(ctx context.Context, url string, gz []byte) error {
	p := e.cfg.Retry
	delay := p.InitialDelay
	deadline := e.now().Add(p.MaxElapsed)
	var lastStatus int
	var lastErr error
	for {
		status, respBody, retryAfter, err := e.doOnce(ctx, url, gz)
		respBody = e.redactSecrets(respBody) // [ext-review-7] never let an echoed credential reach an error string
		lastStatus, lastErr = status, err
		switch {
		case err == nil && status >= 200 && status < 300:
			// Any 2xx is success per OTLP/HTTP. Grafana Cloud returns 200 for /v1/metrics but 204 No
			// Content for /v1/logs — accepting only 200 misclassified every successful logs POST as a
			// retryable failure, so the logs loop never advanced (caught by the live soak). A 200 may carry
			// a partial-success body; we treat the whole batch as accepted (unchanged pre-existing behaviour).
			return nil
		case status >= 400 && status < 500 && status != 429:
			// [ext-review-12] Any non-429 4xx is a terminal request-level reject — 400/413 (classified
			// to the sample-reject taxonomy) AND auth/permission/not-found/unprocessable (401/403/404/
			// 422/…). classify() maps an unrecognised 4xx to ReasonUnknown, on which the runner
			// halts+degrades+backs off — instead of treating it as retryable and re-pulling the SAME
			// window every tick forever (a persistent bad token would never degrade). 429 = rate limit
			// (retryable, below); 5xx = transient (retryable).
			return classify(status, respBody)
		case isRetryable(status, err):
			// fallthrough to backoff
		default: // 5xx not in the retryable set (e.g. 500/501) — re-pull next cadence, no inline retry
			return &emit.RetryableError{Status: status, Err: fmt.Errorf("non-retryable-inline status %d", status)}
		}
		now := e.now()
		if now.After(deadline) {
			return &emit.RetryableError{Status: lastStatus, Err: fmt.Errorf("retry budget exhausted: %v", lastErr)}
		}
		sleep := jittered(delay, p.Jitter)
		// [#122] Honour a 429 Retry-After: retry no sooner than the gateway's advertised throttle window
		// (seconds or HTTP-date, parsed in doOnce), so we don't burn attempts firing guaranteed-429s
		// inside the window. Take max(computed backoff, Retry-After). If the advertised wait is longer
		// than the REMAINING budget, sleeping would only blow past the deadline for no gain — short-circuit
		// to a RetryableError now (re-pull next cadence) rather than waiting it out. Retry-After only
		// applies to 429; the 502/503/504/transport backoff is unchanged (retryAfter is 0 on those). The
		// normal-backoff sleep is intentionally NOT capped to the remaining budget — it may overshoot the
		// deadline by up to one MaxDelay, exactly as before (F8); the next iteration's `now.After(deadline)`
		// then returns. (Capping to `remaining` would let sleep hit 0 at the boundary and busy-loop, since
		// `After` is strict.)
		if status == 429 && retryAfter > 0 {
			if remaining := deadline.Sub(now); retryAfter > remaining {
				return &emit.RetryableError{Status: lastStatus, Err: fmt.Errorf("429 Retry-After %v exceeds remaining retry budget %v", retryAfter, remaining)}
			}
			if retryAfter > sleep {
				sleep = retryAfter
			}
		}
		if err := e.sleep(ctx, sleep); err != nil { // ctx cancelled mid-wait
			return &emit.RetryableError{Status: lastStatus, Err: err}
		}
		if delay = time.Duration(float64(delay) * p.Multiplier); delay > p.MaxDelay {
			delay = p.MaxDelay
		}
	}
}

// redactSecrets scrubs our own credentials from a response body before it is stored in a
// RejectError.Msg (which classify() truncates into the error string). [ext-review-7] A normal gateway
// 4xx body carries err-mimir-* codes, not auth — but a misconfigured/transparent proxy in the egress
// path can echo the request's Authorization header into its error body, which would otherwise leak
// the Basic credential through the error string. Defence-in-depth for the secrets-never-logged invariant.
// The instance_id is NOT redacted on its own — it is a stack identifier (appears in URLs/stack names),
// not a credential, and a bare numeric id would over-redact legitimate body substrings. Only the token,
// the instance_id:token pair, and the base64 Basic credential are scrubbed.
func (e *Emitter) redactSecrets(s string) string {
	if s == "" {
		return s
	}
	for _, secret := range []string{
		strings.TrimPrefix(e.auth, "Basic "), // base64(instance_id:token) — what an echoed Authorization carries
		e.cfg.Token,                          // the raw token, in case it appears un-encoded
		e.cfg.InstanceID + ":" + e.cfg.Token, // the pre-encoding credential pair
	} {
		if len(secret) > 1 { // never ReplaceAll with "" (inserts everywhere); skip the degenerate ":" pair
			s = strings.ReplaceAll(s, secret, "[REDACTED]")
		}
	}
	return s
}

// doOnce sends one POST and returns the status, a (limited) body, and the parsed Retry-After delay
// (0 when absent/unparseable — only meaningful on a 429). [#122] The header must be surfaced here
// because post()'s backoff cannot otherwise see it — resp is not visible to the retry loop.
func (e *Emitter) doOnce(ctx context.Context, url string, gz []byte) (int, string, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(gz))
	if err != nil {
		return 0, "", 0, err
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "gzip")
	if e.auth != "" { // [CP-M7] omit the header entirely for a token-less endpoint
		req.Header.Set("Authorization", e.auth)
	}
	resp, err := e.hc.Do(req)
	if err != nil {
		return 0, "", 0, err // transport error
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, string(body), parseRetryAfter(resp.Header.Get("Retry-After"), e.now()), nil
}

func isRetryable(status int, err error) bool {
	if err != nil {
		return true // transport
	}
	return status == 429 || status == 502 || status == 503 || status == 504
}

// classify maps a 400/413 body to the reject taxonomy by matching Mimir's stable err-mimir-*
// codes (plus human-string fallbacks). Known per-sample rejects (duplicate/too-old/out-of-order/
// out-of-bounds) advance-past. [CP-C7] An UNRECOGNISED non-retryable 4xx is ReasonUnknown, on which
// the RUNNER halts + degrades + backs off (NOT advance-past — that would silently drop valid data on
// a request-level misconfig; NOT bad-encoding — that implies our own malformed payload). A genuinely
// new err-mimir SAMPLE code thus halts loudly (rare, actionable) rather than silently losing data.
// NOTE: error strings never include auth.
func classify(status int, body string) error {
	if status == 413 {
		return &emit.RejectError{Reason: emit.ReasonPayloadTooLarge, Status: 413, Msg: trunc(body)}
	}
	low := strings.ToLower(body)
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(low, s) {
				return true
			}
		}
		return false
	}
	switch {
	case has("err-mimir-sample-duplicate-timestamp", "duplicate-timestamp", "duplicate sample"):
		return &emit.RejectError{Reason: emit.ReasonDuplicateTimestamp, Status: status, Msg: trunc(body)}
	case has("err-mimir-sample-out-of-order", "err-mimir-sample-timestamp-too-old", "err-mimir-sample-out-of-bounds",
		"out-of-order", "out of order", "too old", "too-old", "too far in past",
		// Loki distributor ordering/age rejects (logs path) — same advance-past semantics as Mimir's:
		// a single out-of-order/too-old log chunk is a counted skip-with-gap, NOT a whole-loop degrade.
		"entry out of order", "too far behind", "greater than max age", "max_age"):
		return &emit.RejectError{Reason: emit.ReasonTooOld, Status: status, Msg: trunc(body)}
	case has("failed to parse", "malformed", "proto:", "invalid request body", "bad request body"):
		return &emit.RejectError{Reason: emit.ReasonBadEncoding, Status: status, Msg: trunc(body)} // our bug → halt + alert
	default:
		return &emit.RejectError{Reason: emit.ReasonUnknown, Status: status, Msg: trunc(body)} // [CP-C7] runner halts + degrades (no silent advance)
	}
}

func trunc(s string) string {
	if len(s) > 256 {
		return s[:256]
	}
	return s
}

// parseRetryAfter parses an RFC 7231 Retry-After value: either delay-seconds (a non-negative integer)
// or an HTTP-date. It returns the wait as a duration relative to now, clamped to >= 0; a missing,
// malformed, or past value yields 0 (fall back to the computed backoff). [#122]
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

func jittered(d time.Duration, j float64) time.Duration {
	if j <= 0 {
		return d
	}
	return d + time.Duration(float64(d)*j*(rand.Float64()*2-1))
}

func gzipBytes(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if _, err := zw.Write(b); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

var _ emit.Emitter = (*Emitter)(nil)
