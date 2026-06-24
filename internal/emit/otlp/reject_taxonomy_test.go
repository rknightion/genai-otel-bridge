// SPDX-License-Identifier: AGPL-3.0-only

package otlp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/grafana-ps/aip-oi/internal/emit"
)

func TestReviewUnauthorizedIsTerminalReject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	err := testEmitter(t, srv.URL).Emit(context.Background(), oneBatch())
	var re *emit.RejectError
	if !errors.As(err, &re) || re.Reason != emit.ReasonUnknown {
		t.Fatalf("401 must be terminal unknown RejectError, got %T %v", err, err)
	}
}

func TestReviewRejectErrorRedactsEchoedAuthorization(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("proxy echoed Authorization: Basic MTIzOnNlY3JldC10b2tlbg== token=secret-token"))
	}))
	defer srv.Close()

	err := testEmitter(t, srv.URL).Emit(context.Background(), oneBatch())
	if err == nil {
		t.Fatal("expected 400 error")
	}
	if strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), "MTIzOnNlY3JldC10b2tlbg==") {
		t.Fatalf("secret leaked in reject error: %v", err)
	}
}
