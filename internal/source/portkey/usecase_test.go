// SPDX-License-Identifier: AGPL-3.0-only

package portkey

import (
	"testing"

	"github.com/rknightion/genai-otel-bridge/internal/config"
	"github.com/rknightion/genai-otel-bridge/internal/model"
)

func TestSlugifyUseCase(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"Data Gen", "data_gen", false},
		{"Content Gen", "content_gen", false},
		{"LA", "la", false},
		{"  Data   Gen  ", "data_gen", false},
		{"data-gen/v2", "data_gen_v2", false},
		{"Already_ok", "already_ok", false},
		{"café", "caf", false},
		{"", "", true},
		{"   ", "", true},
		{"!!!", "", true},
	}
	for _, c := range cases {
		got, err := slugifyUseCase(c.in)
		if (err != nil) != c.wantErr {
			t.Fatalf("slugifyUseCase(%q) err=%v wantErr=%v", c.in, err, c.wantErr)
		}
		if !c.wantErr && got != c.want {
			t.Errorf("slugifyUseCase(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func cfgWith(ucs []config.APIKeyUseCase, loopAPIKeyIDs string, loopEnabled bool) config.SourceConfig {
	lc := config.LoopConfig{Enabled: loopEnabled}
	if loopAPIKeyIDs != "" {
		lc.Settings = map[string]string{"api_key_ids": loopAPIKeyIDs}
	}
	return config.SourceConfig{APIKeyUseCases: ucs, Loops: map[string]config.LoopConfig{"analytics": lc}}
}

func TestResolveUseCases(t *testing.T) {
	t.Run("empty ⇒ none", func(t *testing.T) {
		got, err := resolveUseCases(cfgWith(nil, "", true))
		if err != nil || len(got) != 0 {
			t.Fatalf("got %v err %v", got, err)
		}
	})
	t.Run("happy slugs+csv", func(t *testing.T) {
		got, err := resolveUseCases(cfgWith([]config.APIKeyUseCase{
			{Label: "Data Gen", APIKeyIDs: []string{"uuid-a"}},
			{Label: "LA", APIKeyIDs: []string{"uuid-b", " uuid-c "}},
		}, "", true))
		if err != nil {
			t.Fatal(err)
		}
		if got[0].slug != "data_gen" || got[0].apiKeyIDsCSV != "uuid-a" {
			t.Fatalf("0 %+v", got[0])
		}
		if got[1].slug != "la" || got[1].apiKeyIDsCSV != "uuid-b,uuid-c" {
			t.Fatalf("1 %+v", got[1])
		}
	})
	t.Run("no api_key_ids ⇒ err", func(t *testing.T) {
		if _, err := resolveUseCases(cfgWith([]config.APIKeyUseCase{{Label: "X"}}, "", true)); err == nil {
			t.Fatal("want err")
		}
	})
	t.Run("dup slug ⇒ err", func(t *testing.T) {
		if _, err := resolveUseCases(cfgWith([]config.APIKeyUseCase{
			{Label: "Data Gen", APIKeyIDs: []string{"a"}},
			{Label: "data-gen", APIKeyIDs: []string{"b"}},
		}, "", true)); err == nil {
			t.Fatal("want dup-slug err")
		}
	})
	t.Run("dup uuid ⇒ err", func(t *testing.T) {
		if _, err := resolveUseCases(cfgWith([]config.APIKeyUseCase{
			{Label: "A", APIKeyIDs: []string{"shared"}},
			{Label: "B", APIKeyIDs: []string{"shared"}},
		}, "", true)); err == nil {
			t.Fatal("want dup-uuid err")
		}
	})
	t.Run("+ enabled-loop api_key_ids ⇒ err", func(t *testing.T) {
		if _, err := resolveUseCases(cfgWith([]config.APIKeyUseCase{{Label: "X", APIKeyIDs: []string{"a"}}}, "u", true)); err == nil {
			t.Fatal("want mutual-exclusion err")
		}
	})
	t.Run("+ DISABLED-loop api_key_ids ⇒ ok", func(t *testing.T) {
		if _, err := resolveUseCases(cfgWith([]config.APIKeyUseCase{{Label: "X", APIKeyIDs: []string{"a"}}}, "u", false)); err != nil {
			t.Fatalf("disabled loop should not trip mutual-exclusion: %v", err)
		}
	})
}

func TestStampUseCase(t *testing.T) {
	t.Run("stamps slug on samples", func(t *testing.T) {
		samples := []model.Sample{{Labels: map[string]string{"x": "1"}}, {}}
		stampUseCase(samples, "data_gen")
		for i, s := range samples {
			if s.Labels[useCaseLabelKey] != "data_gen" {
				t.Errorf("sample[%d] missing label: %v", i, s.Labels)
			}
		}
	})
	t.Run("no-op on empty slug", func(t *testing.T) {
		samples := []model.Sample{{Labels: map[string]string{"x": "1"}}}
		stampUseCase(samples, "")
		if _, ok := samples[0].Labels[useCaseLabelKey]; ok {
			t.Error("should not stamp empty slug")
		}
	})
}

func TestStampUseCaseRecord(t *testing.T) {
	t.Run("stamps slug on record attributes", func(t *testing.T) {
		lr := &model.LogRecord{}
		stampUseCaseRecord(lr, "content_gen")
		if lr.RecordAttributes[useCaseLabelKey] != "content_gen" {
			t.Errorf("record attr missing: %v", lr.RecordAttributes)
		}
	})
	t.Run("no-op on empty slug", func(t *testing.T) {
		lr := &model.LogRecord{}
		stampUseCaseRecord(lr, "")
		if lr.RecordAttributes != nil {
			t.Error("should not initialise map for empty slug")
		}
	})
}
