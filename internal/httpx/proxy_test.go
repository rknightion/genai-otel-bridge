// SPDX-License-Identifier: AGPL-3.0-only

package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestReviewProxyDoesNotBypassMetadataBlock(t *testing.T) {
	var proxied bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied = true
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")

	c := New(Config{Timeout: 2 * time.Second, AllowPrivate: true})
	req, err := http.NewRequest(http.MethodGet, "http://169.254.169.254/latest/meta-data/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err == nil {
		defer resp.Body.Close()
		t.Fatalf("metadata request succeeded via proxy with status %d", resp.StatusCode)
	}
	if proxied {
		t.Fatal("metadata request reached proxy instead of being blocked before egress")
	}
}
