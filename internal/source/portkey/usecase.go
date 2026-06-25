// SPDX-License-Identifier: AGPL-3.0-only

package portkey

import (
	"fmt"
	"strings"

	"github.com/rknightion/genai-otel-bridge/internal/config"
	"github.com/rknightion/genai-otel-bridge/internal/model"
)

// useCaseLabelKey carries the per-api-key use-case name (metrics label + logs record attribute).
// Distinct from the per-request `use_case` METADATA dimension (metadata_key="use_case") — different concept.
const useCaseLabelKey = "api_key_use_case"

// slugifyUseCase normalises a label: lowercase, collapse each non-[a-z0-9] run to one underscore, trim
// underscores. "Data Gen" → "data_gen". Empty-after-slug is rejected (never emit an empty label value).
func slugifyUseCase(label string) (string, error) {
	var b strings.Builder
	prevUnderscore := false
	for _, r := range strings.ToLower(label) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevUnderscore = false
			continue
		}
		if !prevUnderscore {
			b.WriteByte('_')
			prevUnderscore = true
		}
	}
	slug := strings.Trim(b.String(), "_")
	if slug == "" {
		return "", fmt.Errorf("portkey: api_key_use_cases label %q is empty after normalisation", label)
	}
	return slug, nil
}

type resolvedUseCase struct {
	slug         string
	apiKeyIDsCSV string
}

// resolveUseCases validates cfg.APIKeyUseCases → {slug, apiKeyIDsCSV} entries. Empty ⇒ nil (caller keeps
// the single-pass path). Fail-fast on: an entry with no UUIDs, a duplicate slug or UUID across entries,
// or api_key_use_cases combined with an ENABLED loop's settings.api_key_ids (ambiguous double-filter).
func resolveUseCases(cfg config.SourceConfig) ([]resolvedUseCase, error) {
	if len(cfg.APIKeyUseCases) == 0 {
		return nil, nil
	}
	for name, lp := range cfg.Loops {
		if lp.Enabled && cleanAPIKeyIDs(lp.Settings["api_key_ids"]) != "" {
			return nil, fmt.Errorf("portkey: api_key_use_cases is set, so enabled loop %q must not also set settings.api_key_ids (ambiguous double-filter)", name)
		}
	}
	out := make([]resolvedUseCase, 0, len(cfg.APIKeyUseCases))
	seenSlug := map[string]string{}
	seenUUID := map[string]string{}
	for _, uc := range cfg.APIKeyUseCases {
		slug, err := slugifyUseCase(uc.Label)
		if err != nil {
			return nil, err
		}
		csv := cleanAPIKeyIDs(strings.Join(uc.APIKeyIDs, ","))
		if csv == "" {
			return nil, fmt.Errorf("portkey: api_key_use_cases entry %q has no api_key_ids", uc.Label)
		}
		if prev, dup := seenSlug[slug]; dup {
			return nil, fmt.Errorf("portkey: api_key_use_cases labels %q and %q both normalise to %q", prev, uc.Label, slug)
		}
		seenSlug[slug] = uc.Label
		for _, id := range strings.Split(csv, ",") {
			if prev, dup := seenUUID[id]; dup {
				return nil, fmt.Errorf("portkey: api_key_use_cases api_key_id %q appears in both %q and %q (would double-count)", id, prev, uc.Label)
			}
			seenUUID[id] = uc.Label
		}
		out = append(out, resolvedUseCase{slug: slug, apiKeyIDsCSV: csv})
	}
	return out, nil
}

// stampUseCase adds the api_key_use_case label (value = slug) to every sample. No-op when slug is empty.
func stampUseCase(samples []model.Sample, slug string) {
	if slug == "" {
		return
	}
	for i := range samples {
		if samples[i].Labels == nil {
			samples[i].Labels = map[string]string{}
		}
		samples[i].Labels[useCaseLabelKey] = slug
	}
}

// stampUseCaseRecord sets the api_key_use_case record attribute on a log record. No-op when slug empty.
func stampUseCaseRecord(lr *model.LogRecord, slug string) {
	if slug == "" {
		return
	}
	if lr.RecordAttributes == nil {
		lr.RecordAttributes = map[string]string{}
	}
	lr.RecordAttributes[useCaseLabelKey] = slug
}
