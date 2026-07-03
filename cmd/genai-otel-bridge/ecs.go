// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// resolveECSIdentity returns the ECS Task ARN from the task-metadata endpoint, or "" if metadataURI
// is empty/unreachable. metadataURI is the value of ECS_CONTAINER_METADATA_URI_V4 (a trusted, ECS-
// injected link-local URL) — NOT routed through httpx's SSRF egress guard, which would block 169.254/16.
// [#87] Every failure path where metadataURI IS set but resolution fails logs a WARN, so an empty
// identity is diagnosable (the empty result then trips the RequireIdentity fail-fast in buildHA rather
// than silently electing an empty-identity replica). The empty-metadataURI case is the normal non-ECS
// path and is intentionally silent.
func resolveECSIdentity(metadataURI string) string {
	if metadataURI == "" {
		return ""
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(strings.TrimRight(metadataURI, "/") + "/task")
	if err != nil {
		slog.Warn("ECS task-metadata request failed; leader-election identity unresolved", "err", err)
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		slog.Warn("ECS task-metadata body read failed; leader-election identity unresolved", "err", err)
		return ""
	}
	var meta struct {
		TaskARN string `json:"TaskARN"`
	}
	if err := json.Unmarshal(body, &meta); err != nil {
		slog.Warn("ECS task-metadata JSON parse failed; leader-election identity unresolved", "err", err)
		return ""
	}
	if meta.TaskARN == "" {
		slog.Warn("ECS task-metadata carried an empty TaskARN; leader-election identity unresolved")
	}
	return meta.TaskARN
}

// healthCheckCode GETs <base>/healthz and returns the process exit code (0 healthy, 1 otherwise). It
// backs the `-healthcheck` flag — the ECS container health check (distroless has no shell for curl).
func healthCheckCode(base string) int {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(strings.TrimRight(base, "/") + "/healthz")
	if err != nil {
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		return 0
	}
	return 1
}

// localHealthURL turns a -health-addr listen spec into a loopback probe base for -healthcheck. An
// unspecified bind host (":8080", "0.0.0.0:8080", "[::]:8080") becomes 127.0.0.1 so the probe targets
// this container's own listener.
func localHealthURL(healthAddr string) string {
	host, port, err := net.SplitHostPort(healthAddr)
	if err != nil {
		return "http://127.0.0.1:" + healthAddr // bare port (e.g. "8080") — treat it as the port
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}
