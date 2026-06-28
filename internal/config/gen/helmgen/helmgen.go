// SPDX-License-Identifier: AGPL-3.0-only

// Package helmgen renders the Helm chart's default config block straight from the Go config
// schema. It reflects a config struct type, reads each field's `helm:"..."` render tag, pulls the
// field's Go doc-comment from the source file, and emits a commented, ordered YAML map.
//
// It deliberately does NOT import internal/config: the caller passes the reflect.Type (and the path
// to config.go for comment extraction). That keeps the package importable from a `package config`
// gate test with no import cycle (config → helmgen → config).
//
// Tag grammar (on each struct field, in addition to the `yaml:"..."` name):
//
//	helm:"env=NAME"        scalar value rendered as the literal string "${NAME}"
//	helm:"default=VALUE"   literal default; for a []string field, VALUE is comma-split into a list
//	helm:"omit"            field excluded from the generated default
//	helm:"key=NAME"        for a map[string]T field: emit ONE entry keyed NAME, recursing into T
//	helm:"instance"        for a []T (T struct) field: emit EXACTLY ONE element, recursing into T
//
// A struct field with none of the above and a struct/ptr-to-struct type is recursed as a nested
// mapping. Any other untagged leaf is an error (every field must be covered — that is the whole
// point: a new field with no helm tag fails generation, which fails the gate test).
package helmgen

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Region markers delimit the generated `config:` block inside values.yaml. The generator replaces
// everything BETWEEN these lines; the gate test extracts the same region and byte-compares it.
const (
	BeginMarker = "# >>> BEGIN generated config — do not edit by hand; run `make generate` <<<"
	EndMarker   = "# >>> END generated config <<<"

	// The source-examples region: a SEPARATE, commented-out block of per-source-type example configs
	// (rendered from each source package's example value). It documents non-default source shapes
	// without touching the active generated `config:` block above.
	ExampleBeginMarker = "# >>> BEGIN generated source examples — do not edit by hand; run `make generate` <<<"
	ExampleEndMarker   = "# >>> END generated source examples <<<"
)

// RenderConfigBlock renders the full generated region: the BEGIN marker, a `config:` key, the
// reflected default config (indented two spaces under config:), and the END marker. This exact byte
// sequence is what both the generator splices into values.yaml and the gate test compares against.
func RenderConfigBlock(typ reflect.Type, srcPath string) ([]byte, error) {
	body, err := RenderType(typ, srcPath)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	b.WriteString(BeginMarker)
	b.WriteByte('\n')
	b.WriteString("config:\n")
	for _, line := range strings.Split(strings.TrimRight(string(body), "\n"), "\n") {
		if line == "" {
			b.WriteByte('\n')
			continue
		}
		b.WriteString("  ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteString(EndMarker)
	b.WriteByte('\n')
	return []byte(b.String()), nil
}

// ECSConfigHeader is the fixed banner prepended to the generated ECS config file. It carries NO
// `${WORD}` env-ref syntax (only `${...}` prose) so the file round-trips through config.LoadBytes
// without an unset-env fatal from a ref inside a comment (injectEnvPlaceholders scans raw text).
const ECSConfigHeader = `# genai-otel-bridge — ECS (DynamoDB-backed HA) default config — GENERATED, do not edit by hand.
#
# Generated from the Go config schema (internal/config/config.go) by ` + "`make generate`" + ` — the SAME
# source of truth as deploy/helm/values.yaml, so this example can never drift from the schema. The
# drift gate TestECSConfigExampleUpToDate (internal/config) fails CI if it is stale; re-run
# ` + "`make generate`" + ` and commit. Field comments below are the Go doc-comments.
#
# The ECS Terraform module (deploy/ecs/terraform) injects this verbatim as the GENAI_OTEL_BRIDGE_CONFIG
# container env var; the binary parses it in-memory via config.LoadBytes (no temp file — read-only
# rootfs safe). The ${...} placeholders in the body resolve at load time from the Secrets-Manager-
# injected env vars (the module's secret_arns map).
#
# This is a STARTING POINT, not a drop-in: copy it into your deployment, set ha.dynamodb.table to the
# module's table ("<var.name>-ha"), set ha.dynamodb.region (or rely on the task's AWS_REGION), and
# adjust the source/governance blocks per environment.
`

// ECSProfile is the render Profile for the ECS deployment target: it flips the HA backends to
// DynamoDB and force-includes the DynamoDB HA block (helm:"omit" in the chart) at its defaults. The
// dynamodb-default strings here are tied to config.Load's actual defaults by the package-config gate
// (TestECSProfileDefaultsMatchLoad), so they cannot silently diverge. The optional dynamodb fields
// (region/endpoint/key_prefix) are intentionally NOT force-included — they default to empty/env and
// are documented in the header, matching how values.yaml omits optional helm:"omit" blocks.
func ECSProfile() Profile {
	return Profile{
		Include: map[string]bool{
			"HAConfig.DynamoDB":              true,
			"DynamoDBHAConfig.Table":         true,
			"DynamoDBHAConfig.LockName":      true,
			"DynamoDBHAConfig.LeaseDuration": true,
			"DynamoDBHAConfig.RenewDeadline": true,
			"DynamoDBHAConfig.RetryPeriod":   true,
		},
		Defaults: map[string]string{
			"HAConfig.Coordinator":           "dynamodb",
			"HAConfig.Checkpoint":            "dynamodb",
			"DynamoDBHAConfig.Table":         "genai-otel-bridge-ha",
			"DynamoDBHAConfig.LockName":      "genai-otel-bridge-leader",
			"DynamoDBHAConfig.LeaseDuration": "15s",
			"DynamoDBHAConfig.RenewDeadline": "10s",
			"DynamoDBHAConfig.RetryPeriod":   "2s",
		},
	}
}

// RenderECSConfigFile renders the complete generated ECS config file: the ECSConfigHeader banner
// followed by the full default config rendered under ECSProfile (a bare config document — NO
// `config:` wrapper, since the ECS module injects it verbatim as GENAI_OTEL_BRIDGE_CONFIG). This is
// the exact byte sequence the generator writes and the gate test byte-compares.
func RenderECSConfigFile(typ reflect.Type, srcPath string) ([]byte, error) {
	body, err := RenderTypeProfile(typ, srcPath, ECSProfile())
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	b.WriteString(ECSConfigHeader)
	b.Write(body)
	return []byte(b.String()), nil
}

// findRegion locates the inclusive [begin..end] marker line indices in lines; (-1,-1) if either is absent.
func findRegion(lines []string, begin, end string) (int, int) {
	start, stop := -1, -1
	for i, ln := range lines {
		switch strings.TrimSpace(ln) {
		case begin:
			if start == -1 {
				start = i
			}
		case end:
			if start != -1 && stop == -1 {
				stop = i
			}
		}
	}
	return start, stop
}

// extractRegion returns the inclusive begin..end region (with a trailing newline). Errors if a marker
// is missing.
func extractRegion(valuesYAML []byte, begin, end string) ([]byte, error) {
	lines := strings.Split(string(valuesYAML), "\n")
	start, stop := findRegion(lines, begin, end)
	if start == -1 || stop == -1 {
		return nil, fmt.Errorf("helmgen: could not locate the %q..%q markers in values.yaml", begin, end)
	}
	return []byte(strings.Join(lines[start:stop+1], "\n") + "\n"), nil
}

// spliceRegion replaces the begin..end region of valuesYAML with block (itself a full begin..end region).
func spliceRegion(valuesYAML, block []byte, begin, end string) ([]byte, error) {
	lines := strings.Split(string(valuesYAML), "\n")
	start, stop := findRegion(lines, begin, end)
	if start == -1 || stop == -1 {
		return nil, fmt.Errorf("helmgen: could not locate the %q..%q markers in values.yaml", begin, end)
	}
	var b strings.Builder
	for _, ln := range lines[:start] {
		b.WriteString(ln)
		b.WriteByte('\n')
	}
	b.Write(block) // block already ends in a newline
	// lines[stop] is the END marker (included in block); resume after it.
	tail := lines[stop+1:]
	for i, ln := range tail {
		// avoid a trailing empty element from the final split producing a double newline
		if i == len(tail)-1 && ln == "" {
			continue
		}
		b.WriteString(ln)
		b.WriteByte('\n')
	}
	return []byte(b.String()), nil
}

// ExtractConfigBlock returns the inclusive generated `config:` region (the bytes the gate test compares
// to a freshly-rendered block). SpliceConfigBlock replaces it (block must be a full BeginMarker..EndMarker
// region from RenderConfigBlock).
func ExtractConfigBlock(valuesYAML []byte) ([]byte, error) {
	return extractRegion(valuesYAML, BeginMarker, EndMarker)
}
func SpliceConfigBlock(valuesYAML, block []byte) ([]byte, error) {
	return spliceRegion(valuesYAML, block, BeginMarker, EndMarker)
}

// ExtractExampleBlock / SpliceExampleBlock operate on the source-examples region (RenderExampleBlock).
func ExtractExampleBlock(valuesYAML []byte) ([]byte, error) {
	return extractRegion(valuesYAML, ExampleBeginMarker, ExampleEndMarker)
}
func SpliceExampleBlock(valuesYAML, block []byte) ([]byte, error) {
	return spliceRegion(valuesYAML, block, ExampleBeginMarker, ExampleEndMarker)
}

// Profile customises the type-driven render for a non-default deployment target (e.g. ECS, which
// needs the DynamoDB HA block the Helm chart omits). The zero Profile is the chart default — it
// renders byte-identically to the untagged path (RenderType). Both maps are keyed by
// "StructName.FieldName" (the Go type + field name, NOT the yaml name).
//
//   - Include force-renders a helm:"omit" field for the listed paths only (other omit fields stay
//     excluded). A force-included STRUCT recurses as a nested mapping; a force-included LEAF must have
//     a matching Defaults entry (else render errors — the same forcing function as an untagged leaf).
//   - Defaults overrides the rendered scalar default for the listed paths (e.g. flip
//     ha.coordinator's `lease` default to `dynamodb`). Ignored for struct-typed fields.
type Profile struct {
	Include  map[string]bool
	Defaults map[string]string
}

// RenderType reflects typ (a struct or pointer-to-struct) and returns the YAML for its default
// config map, with each key preceded by its Go doc-comment. srcPath, if non-empty, is the path to
// the Go source file whose field doc-comments are emitted as `#` head comments; pass "" to skip
// comment extraction (used in unit tests). This is the chart-default path (zero Profile).
func RenderType(typ reflect.Type, srcPath string) ([]byte, error) {
	return RenderTypeProfile(typ, srcPath, Profile{})
}

// RenderTypeProfile is RenderType under a Profile (see Profile). A zero Profile is byte-identical to
// RenderType.
func RenderTypeProfile(typ reflect.Type, srcPath string, profile Profile) ([]byte, error) {
	docs, err := parseFieldDocs(srcPath)
	if err != nil {
		return nil, err
	}
	node, err := renderStruct(typ, docs, profile)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2) // match the prior baked-config style (2-space nesting), not yaml.v3's default 4
	if err := enc.Encode(node); err != nil {
		return nil, fmt.Errorf("helmgen: marshal: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("helmgen: close encoder: %w", err)
	}
	return buf.Bytes(), nil
}

// fieldDocs maps "StructName.FieldName" → its doc comment (already cleaned, multi-line preserved).
type fieldDocs map[string]string

func parseFieldDocs(srcPath string) (fieldDocs, error) {
	docs := fieldDocs{}
	if srcPath == "" {
		return docs, nil
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, srcPath, nil, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("helmgen: parse %s: %w", srcPath, err)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return true
		}
		for _, field := range st.Fields.List {
			doc := strings.TrimSpace(field.Doc.Text())
			if doc == "" {
				continue
			}
			for _, name := range field.Names {
				docs[ts.Name.Name+"."+name.Name] = doc
			}
		}
		return true
	})
	return docs, nil
}

// renderStruct builds a yaml MappingNode for the struct type, in field-declaration order. profile
// (see Profile) is the zero value for the chart-default path.
func renderStruct(typ reflect.Type, docs fieldDocs, profile Profile) (*yaml.Node, error) {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct {
		return nil, fmt.Errorf("helmgen: expected struct, got %s", typ.Kind())
	}
	m := &yaml.Node{Kind: yaml.MappingNode}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		yamlName := strings.Split(f.Tag.Get("yaml"), ",")[0]
		if yamlName == "" || yamlName == "-" {
			continue
		}
		tag := f.Tag.Get("helm")
		path := typ.Name() + "." + f.Name
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if tag == "omit" {
			if !profile.Include[path] {
				continue
			}
			// Force-included omit field: a struct recurses as a nested mapping; a leaf needs an
			// explicit profile default (mirrors the untagged-leaf forcing function).
			if ft.Kind() == reflect.Struct {
				tag = ""
			} else if d, ok := profile.Defaults[path]; ok {
				tag = "default=" + d
			} else {
				return nil, fmt.Errorf("field %s.%s: profile force-includes a helm:%q leaf but supplies no Defaults[%q]", typ.Name(), f.Name, "omit", path)
			}
		} else if d, ok := profile.Defaults[path]; ok && ft.Kind() != reflect.Struct {
			tag = "default=" + d // override a tagged scalar default (e.g. coordinator lease→dynamodb)
		}
		valNode, err := renderField(f, tag, docs, profile)
		if err != nil {
			return nil, fmt.Errorf("field %s.%s: %w", typ.Name(), f.Name, err)
		}
		if valNode == nil { // omitted at field level (e.g. empty keyed map)
			continue
		}
		keyNode := &yaml.Node{Kind: yaml.ScalarNode, Value: yamlName}
		if doc := docs[typ.Name()+"."+f.Name]; doc != "" {
			keyNode.HeadComment = doc
		}
		m.Content = append(m.Content, keyNode, valNode)
	}
	return m, nil
}

func renderField(f reflect.StructField, tag string, docs fieldDocs, profile Profile) (*yaml.Node, error) {
	ft := f.Type
	for ft.Kind() == reflect.Pointer {
		ft = ft.Elem()
	}

	switch {
	case strings.HasPrefix(tag, "env="):
		name := strings.TrimPrefix(tag, "env=")
		return scalar("${" + name + "}"), nil

	case strings.HasPrefix(tag, "default="):
		val := strings.TrimPrefix(tag, "default=")
		if ft.Kind() == reflect.Slice && ft.Elem().Kind() == reflect.String {
			seq := &yaml.Node{Kind: yaml.SequenceNode}
			if val == "" {
				// An empty `default=` means an empty list. strings.Split("", ",") returns a one-element
				// slice [""], which would render a bogus `- ""` entry — so render `[]` explicitly instead.
				seq.Style = yaml.FlowStyle
				return seq, nil
			}
			for _, item := range strings.Split(val, ",") {
				seq.Content = append(seq.Content, typedScalar(ft.Elem(), item))
			}
			return seq, nil
		}
		return typedScalar(ft, val), nil

	case strings.HasPrefix(tag, "key="):
		if ft.Kind() != reflect.Map {
			return nil, fmt.Errorf(`helm:"key=" only valid on a map field`)
		}
		key := strings.TrimPrefix(tag, "key=")
		elem, err := renderStruct(ft.Elem(), docs, profile)
		if err != nil {
			return nil, err
		}
		m := &yaml.Node{Kind: yaml.MappingNode}
		m.Content = append(m.Content, scalar(key), elem)
		return m, nil

	case tag == "instance":
		if ft.Kind() != reflect.Slice {
			return nil, fmt.Errorf(`helm:"instance" only valid on a slice field`)
		}
		elem, err := renderStruct(ft.Elem(), docs, profile)
		if err != nil {
			return nil, err
		}
		return &yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{elem}}, nil

	case tag == "":
		if ft.Kind() == reflect.Struct {
			return renderStruct(ft, docs, profile)
		}
		return nil, fmt.Errorf("untagged leaf field of kind %s — add a helm:\"...\" tag (env/default/omit)", ft.Kind())

	default:
		return nil, fmt.Errorf("unrecognised helm tag %q", tag)
	}
}

// scalar makes a plain string scalar; yaml.Marshal quotes it if the value is YAML-significant.
func scalar(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Value: v}
}

// typedScalar tags the scalar so yaml renders ints/bools unquoted and strings quoted-as-needed. We
// set the resolved tag explicitly (!!int / !!bool / !!str) so a numeric-looking string default
// still round-trips with the right type and a value like ":6060" or "60s" is quoted when necessary.
//
// config.Duration is an Int64-kind named type but its YAML form is a human duration STRING ("60s",
// "10m") — so it must be rendered as !!str, never !!int. We detect it by the underlying type name.
func typedScalar(t reflect.Type, v string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.ScalarNode, Value: v}
	if isDurationType(t) {
		n.Tag = "!!str"
		return n
	}
	switch t.Kind() {
	case reflect.Bool:
		n.Tag = "!!bool"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n.Tag = "!!int"
	case reflect.Float32, reflect.Float64:
		// keep an integer-valued float (e.g. rps: 1) as a plain number, not 1.0
		if _, err := strconv.Atoi(v); err == nil {
			n.Tag = "!!int"
		} else {
			n.Tag = "!!float"
		}
	default:
		n.Tag = "!!str"
	}
	return n
}

// isDurationType reports whether t is the config.Duration named type (a time.Duration alias whose
// YAML representation is a duration string), so its defaults render as quoted strings not integers.
func isDurationType(t reflect.Type) bool {
	return t.Name() == "Duration" && t.PkgPath() != "" && t.Kind() == reflect.Int64
}

// RenderValue reflects a populated struct value v and returns YAML bytes with each field's
// Go doc-comment as a head comment. Only non-zero fields are emitted. srcPath is the path to the
// Go source file to extract comments from; pass "" to skip comment extraction.
//
// Rendering rules:
//   - Key is taken from the yaml:"name" tag (split on ","); empty or "-" → skip.
//   - helm tags are IGNORED here (they drive the default-generation tag-path, not value rendering).
//   - Zero-valued fields are skipped (reflect.Value.IsZero).
//   - config.Duration (Int64-kind named type) → formatted as time.Duration string ("1h", "10m").
//   - Other scalars → typedScalar(fieldType, fmt.Sprint(value)).
//   - Nested structs (and non-nil pointers to structs) → recurse.
//   - map[string]T → sorted keys, recurse for struct T, scalar for scalar T.
//   - []T slices → YAML sequence; element T may be scalar or struct.
//   - Nil pointers → treated as zero (skipped).
//
// Determinism: identical input value ⇒ identical bytes (map keys sorted ascending).
func RenderValue(v any, srcPath string) ([]byte, error) {
	docs, err := parseFieldDocs(srcPath)
	if err != nil {
		return nil, err
	}
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil, fmt.Errorf("helmgen: RenderValue: nil pointer")
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil, fmt.Errorf("helmgen: RenderValue: expected struct, got %s", rv.Kind())
	}
	node, err := renderValueStruct(rv, docs, nil)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(node); err != nil {
		return nil, fmt.Errorf("helmgen: RenderValue: marshal: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("helmgen: RenderValue: close encoder: %w", err)
	}
	return buf.Bytes(), nil
}

// renderValueStruct builds a yaml MappingNode for a struct reflect.Value, emitting only non-zero
// fields, in field-declaration order. comments, if non-nil, is passed down to renderValueField so
// that map[string]string fields (i.e. settings maps) can receive per-key head-comments; nil
// comments produces byte-identical output to the pre-unified path.
func renderValueStruct(rv reflect.Value, docs fieldDocs, comments map[string]string) (*yaml.Node, error) {
	rt := rv.Type()
	m := &yaml.Node{Kind: yaml.MappingNode}
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		yamlName := strings.Split(f.Tag.Get("yaml"), ",")[0]
		if yamlName == "" || yamlName == "-" {
			continue
		}
		// NOTE: the value-path deliberately does NOT honour helm:"omit". That tag governs the
		// default-generation TAG-path (RenderConfigBlock) — keeping a field out of the chart default.
		// An example is rendered from an explicit value: show exactly the fields it sets (zero-skip is
		// the only filter), so e.g. a helm:"omit" Settings map populated in an example DOES render.
		fv := rv.Field(i)
		// Dereference pointer; nil pointer → skip (zero).
		for fv.Kind() == reflect.Pointer {
			if fv.IsNil() {
				fv = reflect.Value{} // mark as zero
				break
			}
			fv = fv.Elem()
		}
		if !fv.IsValid() || fv.IsZero() {
			continue
		}
		valNode, err := renderValueField(f.Type, fv, docs, comments)
		if err != nil {
			return nil, fmt.Errorf("field %s.%s: %w", rt.Name(), f.Name, err)
		}
		if valNode == nil {
			continue
		}
		keyNode := &yaml.Node{Kind: yaml.ScalarNode, Value: yamlName}
		if doc := docs[rt.Name()+"."+f.Name]; doc != "" {
			keyNode.HeadComment = doc
		}
		m.Content = append(m.Content, keyNode, valNode)
	}
	return m, nil
}

// renderValueField converts a reflect.Value into a yaml.Node for use by RenderValue. comments, if
// non-nil, is threaded through so that map[string]string fields whose element kind is
// reflect.String receive per-key head-comments from comments[key]; nil comments is a no-op (output
// byte-identical to the pre-unified path).
func renderValueField(ft reflect.Type, fv reflect.Value, docs fieldDocs, comments map[string]string) (*yaml.Node, error) {
	// Dereference pointers in type/value simultaneously.
	for ft.Kind() == reflect.Pointer {
		if fv.IsNil() {
			return nil, nil
		}
		ft = ft.Elem()
		fv = fv.Elem()
	}

	switch ft.Kind() {
	case reflect.Struct:
		return renderValueStruct(fv, docs, comments)

	case reflect.Map:
		if fv.IsNil() || fv.Len() == 0 {
			return nil, nil
		}
		// Sort keys for determinism.
		keys := make([]string, 0, fv.Len())
		for _, k := range fv.MapKeys() {
			keys = append(keys, k.String())
		}
		sort.Strings(keys)
		m := &yaml.Node{Kind: yaml.MappingNode}
		elemType := ft.Elem()
		for _, k := range keys {
			mv := fv.MapIndex(reflect.ValueOf(k))
			var valNode *yaml.Node
			var err error
			if elemType.Kind() == reflect.Struct {
				valNode, err = renderValueStruct(mv, docs, comments)
			} else {
				valNode, err = renderValueField(elemType, mv, docs, comments)
			}
			if err != nil {
				return nil, fmt.Errorf("map key %q: %w", k, err)
			}
			if valNode == nil {
				continue
			}
			keyNode := scalar(k)
			// Apply head-comment only for string-valued maps (i.e. settings maps) when the caller
			// supplied a non-empty comment for this key. The double-hash ("# # comment") is
			// intentional: RenderExampleBlock prefixes every rendered line with "# ", so the
			// head-comment appears double-hashed and uncommenting the block leaves a real YAML comment.
			if elemType.Kind() == reflect.String && comments != nil {
				if c := comments[k]; c != "" {
					keyNode.HeadComment = "# " + c
				}
			}
			m.Content = append(m.Content, keyNode, valNode)
		}
		if len(m.Content) == 0 {
			return nil, nil
		}
		return m, nil

	case reflect.Slice:
		if fv.IsNil() || fv.Len() == 0 {
			return nil, nil
		}
		seq := &yaml.Node{Kind: yaml.SequenceNode}
		elemType := ft.Elem()
		for i := 0; i < fv.Len(); i++ {
			ev := fv.Index(i)
			var elemNode *yaml.Node
			var err error
			if elemType.Kind() == reflect.Struct {
				elemNode, err = renderValueStruct(ev, docs, comments)
			} else {
				elemNode, err = renderValueField(elemType, ev, docs, comments)
			}
			if err != nil {
				return nil, fmt.Errorf("slice index %d: %w", i, err)
			}
			if elemNode == nil {
				continue
			}
			seq.Content = append(seq.Content, elemNode)
		}
		if len(seq.Content) == 0 {
			return nil, nil
		}
		return seq, nil

	default:
		// Scalar (incl. named Int64 types like config.Duration).
		if isDurationType(ft) {
			dur := time.Duration(fv.Int())
			return typedScalar(ft, compactDuration(dur)), nil
		}
		return typedScalar(ft, fmt.Sprint(fv.Interface())), nil
	}
}

// compactDuration returns a compact human-readable form of d: whole hours, whole minutes, or
// hours+minutes when both are non-zero. Falls back to d.String() for any sub-minute remainder so
// that no information is lost; "0s" for the zero value.
//
// Examples: 1m→"1m", 24h→"24h", 90m→"1h30m", 0→"0s".
func compactDuration(d time.Duration) string {
	if d == 0 {
		return "0s"
	}
	h := d / time.Hour
	rem := d % time.Hour
	m := rem / time.Minute
	subMin := rem % time.Minute
	if subMin != 0 {
		// Sub-minute component present; fall back to standard form (e.g. "1m30s").
		return d.String()
	}
	switch {
	case h > 0 && m > 0:
		return fmt.Sprintf("%dh%dm", h, m)
	case h > 0:
		return fmt.Sprintf("%dh", h)
	default:
		return fmt.Sprintf("%dm", m)
	}
}

// Example pairs a SourceConfig-shaped value (Settings populated at defaults) with vendor-owned
// per-settings-key head-comments. SettingsComments is keyed by the settings KEY (e.g. "window");
// a key shared across loops for the same source uses one comment (same meaning per vendor).
// When SettingsComments is nil or empty the output is byte-identical to the pre-typed path.
type Example struct {
	Value            any
	SettingsComments map[string]string
}

// RenderExampleBlock renders example source values as a COMMENTED-OUT `sources:` snippet wrapped in the
// example-region markers. Each example is a populated SourceConfig-shaped value (a source package's
// ExampleSource()). The block documents non-default source shapes WITHOUT activating them — every line
// is prefixed "# ", so an operator copies it under `config.sources` and uncomments to use it. Rendered
// without field doc-comments (the active `config:` block above already documents every field).
//
// When an Example carries a non-empty SettingsComments map, each matching settings key node receives a
// head-comment. Because RenderExampleBlock prefixes every rendered line with "# ", the head-comment
// appears double-hashed ("# # comment") — which is correct: uncommenting the block strips the outer
// "# " and leaves a real YAML comment.
func RenderExampleBlock(examples []Example) ([]byte, error) {
	var body strings.Builder
	body.WriteString("sources:\n")
	for _, ex := range examples {
		// renderValueWithComments is the unified path: nil comments → byte-identical to RenderValue.
		y, err := renderValueWithComments(ex.Value, ex.SettingsComments)
		if err != nil {
			return nil, err
		}
		for i, ln := range strings.Split(strings.TrimRight(string(y), "\n"), "\n") {
			switch {
			case i == 0:
				body.WriteString("  - " + ln + "\n")
			case ln == "":
				body.WriteByte('\n')
			default:
				body.WriteString("    " + ln + "\n")
			}
		}
	}
	var b strings.Builder
	b.WriteString(ExampleBeginMarker + "\n")
	b.WriteString("# Example source(s) — NOT active. Copy under `config.sources`, set the secret(s), and\n")
	b.WriteString("# adjust per environment. Generated from each source package's ExampleSource().\n")
	for _, ln := range strings.Split(strings.TrimRight(body.String(), "\n"), "\n") {
		if ln == "" {
			b.WriteString("#\n")
		} else {
			b.WriteString("# " + ln + "\n")
		}
	}
	b.WriteString(ExampleEndMarker + "\n")
	return []byte(b.String()), nil
}

// renderValueWithComments is the internal entry point used by both RenderValue (comments=nil) and
// RenderExampleBlock (comments=ex.SettingsComments). nil comments → byte-identical output to the
// pre-unified path.
func renderValueWithComments(v any, comments map[string]string) ([]byte, error) {
	docs, err := parseFieldDocs("") // no doc comments on the value/example path
	if err != nil {
		return nil, err
	}
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil, fmt.Errorf("helmgen: renderValueWithComments: nil pointer")
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil, fmt.Errorf("helmgen: renderValueWithComments: expected struct, got %s", rv.Kind())
	}
	node, err := renderValueStruct(rv, docs, comments)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(node); err != nil {
		return nil, fmt.Errorf("helmgen: renderValueWithComments: marshal: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("helmgen: renderValueWithComments: close encoder: %w", err)
	}
	return buf.Bytes(), nil
}
