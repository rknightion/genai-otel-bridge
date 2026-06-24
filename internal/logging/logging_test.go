// SPDX-License-Identifier: AGPL-3.0-only

package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestNewHandlerLogfmtQuotesSpecialValues(t *testing.T) {
	var buf bytes.Buffer
	h, err := NewHandler("logfmt", "", &buf)
	if err != nil {
		t.Fatal(err)
	}
	slog.New(h).Info("hello world", "detail", "a b=c")
	out := buf.String()
	// logfmt = key=value pairs; a value with spaces/'=' must be quoted so Loki's logfmt parser round-trips it.
	if !strings.Contains(out, "msg=") || !strings.Contains(out, `detail="a b=c"`) {
		t.Fatalf("expected logfmt key=value with quoted special value, got: %q", out)
	}
	if strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Fatalf("logfmt must not be JSON: %q", out)
	}
}

func TestNewHandlerJSON(t *testing.T) {
	var buf bytes.Buffer
	h, err := NewHandler("json", "", &buf)
	if err != nil {
		t.Fatal(err)
	}
	slog.New(h).Info("hi", "k", "v")
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("json handler output not valid JSON (%v): %q", err, buf.String())
	}
	if m["msg"] != "hi" || m["k"] != "v" {
		t.Fatalf("unexpected json fields: %v", m)
	}
}

func TestNewHandlerEmptyDefaultsToLogfmt(t *testing.T) {
	var buf bytes.Buffer
	h, err := NewHandler("", "", &buf)
	if err != nil {
		t.Fatal(err)
	}
	slog.New(h).Info("x")
	if strings.HasPrefix(strings.TrimSpace(buf.String()), "{") {
		t.Fatalf("empty format should default to logfmt (not json): %q", buf.String())
	}
}

func TestNewHandlerUnknownFormatErrors(t *testing.T) {
	if _, err := NewHandler("xml", "", &bytes.Buffer{}); err == nil {
		t.Fatal("expected error for unknown log format")
	}
}

func TestNewHandlerLevel(t *testing.T) {
	cases := []struct {
		level        string
		wantErr      bool
		debugEnabled bool
		infoEnabled  bool
		warnEnabled  bool
		errorEnabled bool
	}{
		{level: "", debugEnabled: false, infoEnabled: true, warnEnabled: true, errorEnabled: true}, // empty ⇒ info
		{level: "info", debugEnabled: false, infoEnabled: true, warnEnabled: true, errorEnabled: true},
		{level: "debug", debugEnabled: true, infoEnabled: true, warnEnabled: true, errorEnabled: true},
		{level: "warn", debugEnabled: false, infoEnabled: false, warnEnabled: true, errorEnabled: true},
		{level: "error", debugEnabled: false, infoEnabled: false, warnEnabled: false, errorEnabled: true},
		{level: "bogus", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.level, func(t *testing.T) {
			h, err := NewHandler("logfmt", tc.level, &bytes.Buffer{})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("level %q: expected error, got nil", tc.level)
				}
				return
			}
			if err != nil {
				t.Fatalf("level %q: unexpected error: %v", tc.level, err)
			}
			ctx := context.Background()
			if got := h.Enabled(ctx, slog.LevelDebug); got != tc.debugEnabled {
				t.Errorf("level %q: Debug enabled=%v, want %v", tc.level, got, tc.debugEnabled)
			}
			if got := h.Enabled(ctx, slog.LevelInfo); got != tc.infoEnabled {
				t.Errorf("level %q: Info enabled=%v, want %v", tc.level, got, tc.infoEnabled)
			}
			if got := h.Enabled(ctx, slog.LevelWarn); got != tc.warnEnabled {
				t.Errorf("level %q: Warn enabled=%v, want %v", tc.level, got, tc.warnEnabled)
			}
			if got := h.Enabled(ctx, slog.LevelError); got != tc.errorEnabled {
				t.Errorf("level %q: Error enabled=%v, want %v", tc.level, got, tc.errorEnabled)
			}
		})
	}
}
