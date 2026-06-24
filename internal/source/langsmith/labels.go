// SPDX-License-Identifier: AGPL-3.0-only

package langsmith

// AllowedLabelKeys returns the content-free label / indexed-attribute keys this source emits (see
// portkey.AllowedLabelKeys for the rationale — keys live in the vendor package, not in internal/app, so
// no vendor label key leaks into core code or defaults).
//   - quantile: sessions latency / first-token percentile gauges ({quantile} label)
//   - session / feedback_key: sessions per-session dimension + numeric-feedback gauges
//   - run_type / status: runs logs indexed attrs (Loki stream-label candidates)
//
// All are content-free operational identifiers; none is a message body or injected PII. (`quantile` is
// also emitted by portkey; the composition root dedupes the union.)
func AllowedLabelKeys() []string {
	return []string{"quantile", "session", "feedback_key", "run_type", "status"}
}
