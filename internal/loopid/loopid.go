// SPDX-License-Identifier: Apache-2.0

// Package loopid holds the self-metrics identity of one loop instance. It is a leaf package (no
// imports) so both the scheduler, which records the identity, and selfobs, which turns it into metric
// attributes, can share the type without selfobs depending on the scheduler or the config. [#387]
package loopid

// ID identifies one loop instance on the self plane. Source is the configured source_instance and Loop
// the loop's metrics name. Both come from configuration, never from upstream data, so the
// {source, loop} label set is bounded by configuration.
type ID struct {
	Source string
	Loop   string
}
