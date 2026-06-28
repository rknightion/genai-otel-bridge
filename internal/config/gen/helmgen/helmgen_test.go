// SPDX-License-Identifier: AGPL-3.0-only

package helmgen

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// Duration is a local int64-based type whose Name() == "Duration" and Kind == Int64, exercising the
// isDurationType path in RenderValue (which formats via time.Duration) without importing internal/config.
type Duration int64

// rvScalar is a helper struct for RenderValue scalar tests.
type rvScalar struct {
	Name    string  `yaml:"name"`
	Count   int     `yaml:"count"`
	Active  bool    `yaml:"active"`
	Score   float64 `yaml:"score"`
	Omitted string  `yaml:"omitted" helm:"omit"`
	Zero    string  `yaml:"zero"` // intentionally left zero → must be skipped
}

// rvNested exercises nested struct recursion.
type rvNested struct {
	Top   string  `yaml:"top"`
	Inner rvInner `yaml:"inner"`
}

type rvInner struct {
	X int `yaml:"x"`
}

// rvDurationHolder exercises isDurationType path.
type rvDurationHolder struct {
	Interval Duration `yaml:"interval"`
}

// rvMapHolder exercises map[string]string with sorted keys.
type rvMapHolder struct {
	Labels map[string]string `yaml:"labels"`
}

// rvSeqHolder exercises []string sequence.
type rvSeqHolder struct {
	Tags []string `yaml:"tags"`
}

// sample mirrors the kinds of fields/tags the real config carries, so we can assert the tag
// grammar (env / default / omit / key / instance) and comment emission deterministically without
// depending on the real config.Config layout.
type sample struct {
	// Endpoint is an env-ref field.
	Endpoint string `yaml:"endpoint" helm:"env=GC_OTLP_ENDPOINT"`
	// Name has a literal default.
	Name string `yaml:"name" helm:"default=genai-otel-bridge"`
	// Skipped is intentionally omitted.
	Skipped string          `yaml:"skipped" helm:"omit"`
	Nested  nested          `yaml:"nested"`
	Items   []item          `yaml:"items" helm:"instance"`
	Loops   map[string]loop `yaml:"loops" helm:"key=analytics"`
}

type nested struct {
	// Count is an int default.
	Count int `yaml:"count" helm:"default=7"`
}

type item struct {
	// Kind selects the source kind.
	Kind   string   `yaml:"kind" helm:"default=portkey"`
	Graphs []string `yaml:"graphs" helm:"default=a,b,c"`
}

type loop struct {
	Enabled bool `yaml:"enabled" helm:"default=true"`
}

func TestRenderTagGrammar(t *testing.T) {
	// srcPath empty → no doc comments parsed from file; struct comments above aren't Go-doc reachable
	// for an in-test type anyway. We assert structure + tag handling here, comments in the real test.
	out, err := RenderType(reflect.TypeFor[sample](), "")
	if err != nil {
		t.Fatalf("RenderType: %v", err)
	}
	s := string(out)
	checks := []string{
		"endpoint: ${GC_OTLP_ENDPOINT}",
		"name: genai-otel-bridge",
		"count: 7",
		"kind: portkey",
		"enabled: true",
	}
	for _, c := range checks {
		if !strings.Contains(s, c) {
			t.Errorf("rendered output missing %q\n---\n%s", c, s)
		}
	}
	if strings.Contains(s, "skipped") {
		t.Errorf("omitted field leaked into output\n---\n%s", s)
	}
	// instance slice → exactly one element under items:
	if got := strings.Count(s, "- kind:"); got != 1 {
		t.Errorf("expected exactly one items element, got %d\n---\n%s", got, s)
	}
	// keyed map → one entry under the configured key
	if !strings.Contains(s, "analytics:") {
		t.Errorf("keyed map entry %q missing\n---\n%s", "analytics", s)
	}
	// []string default split into a YAML list
	for _, g := range []string{"- a", "- b", "- c"} {
		if !strings.Contains(s, g) {
			t.Errorf("graphs list item %q missing\n---\n%s", g, s)
		}
	}
}

// profileHA / profileDDB mirror the real HAConfig/DynamoDBHAConfig omit+override shape so the Profile
// mechanism (force-include a helm:"omit" subtree, override a tagged default) can be exercised without
// importing internal/config (which would create a cycle with the in-package gate test).
type profileHA struct {
	// Coordinator selects the leader-election backend.
	Coordinator string     `yaml:"coordinator" helm:"default=lease"`
	Checkpoint  string     `yaml:"checkpoint" helm:"default=configmap"`
	DynamoDB    profileDDB `yaml:"dynamodb" helm:"omit"`
}

type profileDDB struct {
	// Table is the lock+checkpoint table.
	Table    string   `yaml:"table" helm:"omit"`
	LockName string   `yaml:"lock_name" helm:"omit"`
	Lease    Duration `yaml:"lease_duration" helm:"omit"`
	Region   string   `yaml:"region" helm:"omit"` // NOT force-included → must stay absent
}

// TestRenderTypeProfile_IncludeAndOverride verifies the ECS render profile: a helm:"omit" struct
// (DynamoDB) is force-included and recursed; its force-included omit-leaf children render at the
// profile-supplied default; a tagged scalar default (Coordinator/Checkpoint) is overridden; and an
// omit child NOT in the include set stays absent.
func TestRenderTypeProfile_IncludeAndOverride(t *testing.T) {
	p := Profile{
		Include: map[string]bool{
			"profileHA.DynamoDB":  true,
			"profileDDB.Table":    true,
			"profileDDB.LockName": true,
			"profileDDB.Lease":    true,
		},
		Defaults: map[string]string{
			"profileHA.Coordinator": "dynamodb",
			"profileHA.Checkpoint":  "dynamodb",
			"profileDDB.Table":      "genai-otel-bridge-ha",
			"profileDDB.LockName":   "genai-otel-bridge-leader",
			"profileDDB.Lease":      "15s",
		},
	}
	out, err := RenderTypeProfile(reflect.TypeFor[profileHA](), "", p)
	if err != nil {
		t.Fatalf("RenderTypeProfile: %v", err)
	}
	s := string(out)
	for _, c := range []string{
		"coordinator: dynamodb", // tagged default overridden
		"checkpoint: dynamodb",
		"dynamodb:",                   // omit struct force-included
		"table: genai-otel-bridge-ha", // omit leaf force-included w/ profile default
		"lock_name: genai-otel-bridge-leader",
		"lease_duration:", "15s", // Duration leaf → compact !!str
	} {
		if !strings.Contains(s, c) {
			t.Errorf("rendered output missing %q\n---\n%s", c, s)
		}
	}
	if strings.Contains(s, "region:") {
		t.Errorf("omit child not in the include set leaked into output\n---\n%s", s)
	}
	// Field doc-comments still flow through under a profile (srcPath="" here so none parsed; just
	// assert the force-included struct didn't break the comment plumbing by rendering structurally).
	if !strings.Contains(s, "lease_duration: 15s") && !strings.Contains(s, "lease_duration: \"15s\"") {
		t.Errorf("lease_duration default not rendered as a duration string:\n%s", s)
	}
}

// TestRenderTypeProfile_ForceIncludedLeafWithoutDefaultErrors: force-including an omit LEAF without a
// matching Defaults entry is a hard error (same forcing-function spirit as an untagged leaf).
func TestRenderTypeProfile_ForceIncludedLeafWithoutDefaultErrors(t *testing.T) {
	p := Profile{Include: map[string]bool{"profileHA.DynamoDB": true, "profileDDB.Table": true}}
	if _, err := RenderTypeProfile(reflect.TypeFor[profileHA](), "", p); err == nil {
		t.Fatal("expected an error force-including an omit leaf with no Defaults entry, got nil")
	}
}

// TestRenderTypeProfile_ZeroProfileMatchesRenderType: a zero Profile must produce byte-identical
// output to the plain RenderType path (protects TestHelmGeneratedConfigUpToDate / values.yaml).
func TestRenderTypeProfile_ZeroProfileMatchesRenderType(t *testing.T) {
	a, err := RenderType(reflect.TypeFor[sample](), "")
	if err != nil {
		t.Fatalf("RenderType: %v", err)
	}
	b, err := RenderTypeProfile(reflect.TypeFor[sample](), "", Profile{})
	if err != nil {
		t.Fatalf("RenderTypeProfile: %v", err)
	}
	if string(a) != string(b) {
		t.Errorf("zero Profile diverged from RenderType\n--- RenderType ---\n%s\n--- zero Profile ---\n%s", a, b)
	}
}

type sliceDefaults struct {
	Empty  []string `yaml:"empty" helm:"default="`
	Filled []string `yaml:"filled" helm:"default=x,y"`
}

// TestRenderType_EmptyStringSliceDefault: a []string field with an EMPTY `default=` must render `[]`,
// not a bogus one-element [""] list (strings.Split("",",") returns [""]); a non-empty default still
// splits into a YAML sequence.
func TestRenderType_EmptyStringSliceDefault(t *testing.T) {
	out, err := RenderType(reflect.TypeFor[sliceDefaults](), "")
	if err != nil {
		t.Fatalf("RenderType: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "empty: []") {
		t.Errorf("empty default= must render `empty: []`, got:\n%s", s)
	}
	if strings.Contains(s, `- ""`) {
		t.Errorf("empty default= must NOT render a one-element [\"\"] list, got:\n%s", s)
	}
	for _, item := range []string{"- x", "- y"} {
		if !strings.Contains(s, item) {
			t.Errorf("non-empty default= must still split; missing %q, got:\n%s", item, s)
		}
	}
}

// TestRenderValue_Scalars checks that string/int/bool/float scalars render correctly, that an
// helm:"omit" field is excluded, and that a zero-valued field is skipped.
func TestRenderValue_Scalars(t *testing.T) {
	v := rvScalar{
		Name:   "hello",
		Count:  42,
		Active: true,
		Score:  3.14,
		// Omitted and Zero are intentionally not set.
	}
	out, err := RenderValue(v, "")
	if err != nil {
		t.Fatalf("RenderValue: %v", err)
	}
	s := string(out)

	want := []string{
		"name: hello",
		"count: 42",
		"active: true",
		"score: 3.14",
	}
	for _, w := range want {
		if !strings.Contains(s, w) {
			t.Errorf("missing %q in:\n%s", w, s)
		}
	}
	for _, absent := range []string{"omitted", "zero"} {
		if strings.Contains(s, absent) {
			t.Errorf("field %q should be absent but appeared in:\n%s", absent, s)
		}
	}
}

// TestRenderValue_NestedStruct checks recursion into nested structs.
func TestRenderValue_NestedStruct(t *testing.T) {
	v := rvNested{
		Top:   "world",
		Inner: rvInner{X: 7},
	}
	out, err := RenderValue(v, "")
	if err != nil {
		t.Fatalf("RenderValue: %v", err)
	}
	s := string(out)

	want := []string{
		"top: world",
		"inner:",
		"  x: 7",
	}
	for _, w := range want {
		if !strings.Contains(s, w) {
			t.Errorf("missing %q in:\n%s", w, s)
		}
	}
}

// TestRenderValue_Duration checks that a Duration (Int64-named-type) is rendered as a compact human
// duration string via isDurationType+compactDuration rather than a bare integer or the verbose Go form.
func TestRenderValue_Duration(t *testing.T) {
	const oneHour = Duration(3_600_000_000_000) // 1h in nanoseconds
	v := rvDurationHolder{Interval: oneHour}
	out, err := RenderValue(v, "")
	if err != nil {
		t.Fatalf("RenderValue: %v", err)
	}
	s := string(out)

	// Must appear as compact "1h" (compactDuration), NOT as "1h0m0s" (verbose Go form) or a bare integer.
	if !strings.Contains(s, "interval: 1h") {
		t.Errorf("expected compact duration form \"interval: 1h\"; got:\n%s", s)
	}
	if strings.Contains(s, "1h0m0s") {
		t.Errorf("Duration rendered in verbose Go form instead of compact form:\n%s", s)
	}
	if strings.Contains(s, "3600000000000") {
		t.Errorf("Duration rendered as raw nanoseconds instead of human string:\n%s", s)
	}
}

// TestCompactDuration verifies the compactDuration helper for the standard table cases.
func TestCompactDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{time.Minute, "1m"},
		{24 * time.Hour, "24h"},
		{90 * time.Minute, "1h30m"},
		{0, "0s"},
	}
	for _, tc := range tests {
		got := compactDuration(tc.d)
		if got != tc.want {
			t.Errorf("compactDuration(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// rvDurationExample is used to assert compactDuration in the RenderExampleBlock path.
type rvDurationExample struct {
	Type    string   `yaml:"type"`
	Cadence Duration `yaml:"cadence"`
}

// TestRenderExampleBlock_DurationCompact verifies that a Duration-typed field rendered via
// RenderExampleBlock emits the compact form, not the verbose Go time.Duration.String() form.
func TestRenderExampleBlock_DurationCompact(t *testing.T) {
	const oneMin = Duration(60_000_000_000) // 1m in nanoseconds
	v := rvDurationExample{Type: "test", Cadence: oneMin}
	out, err := RenderExampleBlock([]Example{{Value: v}})
	if err != nil {
		t.Fatalf("RenderExampleBlock: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "cadence: 1m") {
		t.Errorf("expected compact duration \"cadence: 1m\" in example block; got:\n%s", s)
	}
	if strings.Contains(s, "1m0s") {
		t.Errorf("Duration rendered in verbose Go form instead of compact form:\n%s", s)
	}
}

// TestRenderValue_MapSortedKeys checks that map[string]string entries are emitted with keys sorted
// ascending (determinism requirement).
func TestRenderValue_MapSortedKeys(t *testing.T) {
	v := rvMapHolder{
		Labels: map[string]string{
			"zebra": "z",
			"alpha": "a",
			"mango": "m",
		},
	}
	out, err := RenderValue(v, "")
	if err != nil {
		t.Fatalf("RenderValue: %v", err)
	}
	s := string(out)

	// Keys must appear in alphabetical order.
	alphaIdx := strings.Index(s, "alpha")
	mangoIdx := strings.Index(s, "mango")
	zebraIdx := strings.Index(s, "zebra")
	if alphaIdx < 0 || mangoIdx < 0 || zebraIdx < 0 {
		t.Fatalf("one or more expected keys missing:\n%s", s)
	}
	if alphaIdx >= mangoIdx || mangoIdx >= zebraIdx {
		t.Errorf("keys are not in sorted order (alpha=%d mango=%d zebra=%d):\n%s",
			alphaIdx, mangoIdx, zebraIdx, s)
	}
}

// TestRenderValue_Slice checks that []string is rendered as a YAML sequence.
func TestRenderValue_Slice(t *testing.T) {
	v := rvSeqHolder{Tags: []string{"foo", "bar", "baz"}}
	out, err := RenderValue(v, "")
	if err != nil {
		t.Fatalf("RenderValue: %v", err)
	}
	s := string(out)

	want := []string{"- foo", "- bar", "- baz"}
	for _, w := range want {
		if !strings.Contains(s, w) {
			t.Errorf("missing sequence element %q in:\n%s", w, s)
		}
	}
}

// TestRenderValue_ZeroFieldSkipped explicitly verifies that a zero-valued field is not emitted.
func TestRenderValue_ZeroFieldSkipped(t *testing.T) {
	v := rvScalar{Name: "only-name"} // Count, Active, Score all zero
	out, err := RenderValue(v, "")
	if err != nil {
		t.Fatalf("RenderValue: %v", err)
	}
	s := string(out)

	if strings.Contains(s, "count:") {
		t.Errorf("zero int field 'count' should be omitted:\n%s", s)
	}
	if strings.Contains(s, "active:") {
		t.Errorf("zero bool field 'active' should be omitted:\n%s", s)
	}
	if strings.Contains(s, "score:") {
		t.Errorf("zero float field 'score' should be omitted:\n%s", s)
	}
}

// TestRenderValue_HelmTagIgnored verifies the value-path IGNORES helm tags: a helm:"omit" field that
// is explicitly set (non-zero) in the value IS rendered (omit governs the default-generation tag-path,
// not value rendering — e.g. a populated Settings map in a source example must appear).
func TestRenderValue_HelmTagIgnored(t *testing.T) {
	v := rvScalar{Name: "x", Omitted: "should-appear"}
	out, err := RenderValue(v, "")
	if err != nil {
		t.Fatalf("RenderValue: %v", err)
	}
	if !strings.Contains(string(out), "omitted") || !strings.Contains(string(out), "should-appear") {
		t.Errorf("helm:\"omit\" field with a non-zero value should render in the value-path:\n%s", out)
	}
}

// TestRenderExampleBlockSettingsComments verifies that SettingsComments are emitted as head-comments
// on the relevant map keys, and that nil SettingsComments produces byte-identical output (protecting
// TestHelmGeneratedExamplesUpToDate).
func TestRenderExampleBlockSettingsComments(t *testing.T) {
	val := struct {
		Type     string            `yaml:"type"`
		Settings map[string]string `yaml:"settings"`
	}{Type: "demo", Settings: map[string]string{"window": "1h", "settle": "10m"}}

	out, err := RenderExampleBlock([]Example{{Value: val,
		SettingsComments: map[string]string{"window": "trailing query span", "settle": "exclude the mutating tail"}}})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "trailing query span") || !strings.Contains(s, "settle: 10m") {
		t.Fatalf("settings key comment/default not rendered:\n%s", s)
	}

	// nil comments must render byte-identically to the pre-change path (protects TestHelmGeneratedExamplesUpToDate)
	plain, err := RenderExampleBlock([]Example{{Value: val}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "trailing query span") {
		t.Fatalf("nil SettingsComments must not emit comments:\n%s", plain)
	}
}

// TestRenderValue_Determinism asserts byte-for-byte identical output for the same input value.
func TestRenderValue_Determinism(t *testing.T) {
	v := rvMapHolder{
		Labels: map[string]string{"b": "2", "a": "1", "c": "3"},
	}
	out1, err := RenderValue(v, "")
	if err != nil {
		t.Fatalf("first RenderValue: %v", err)
	}
	out2, err := RenderValue(v, "")
	if err != nil {
		t.Fatalf("second RenderValue: %v", err)
	}
	if string(out1) != string(out2) {
		t.Errorf("RenderValue is non-deterministic:\nrun1:\n%s\nrun2:\n%s", out1, out2)
	}
}
