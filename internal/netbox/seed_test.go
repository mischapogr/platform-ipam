package netbox

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
)

// --- a minimal in-memory NetBox fake for the four endpoints Seed uses ---

type seedFakeObject struct {
	id      int
	name    string // custom field's "name", choice set's "name", or tag's "slug"
	payload map[string]any
}

type seedFake struct {
	mu         sync.Mutex
	nextID     int
	choiceSets []seedFakeObject
	fields     []seedFakeObject
	tags       []seedFakeObject
	failPath   string // if non-empty, requests to this path return 500
	failAfter  int    // if > 0, the failAfter-th matching request onward fails; 0 means always
	seenOnPath int
}

func newSeedFake() *seedFake { return &seedFake{nextID: 1} }

func (f *seedFake) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.failPath != "" && strings.HasPrefix(r.URL.Path, f.failPath) {
			f.seenOnPath++
			if f.failAfter == 0 || f.seenOnPath >= f.failAfter {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		var store *[]seedFakeObject
		var key string
		switch r.URL.Path {
		case choiceSetsPath:
			store, key = &f.choiceSets, "name"
		case customFieldsPath:
			store, key = &f.fields, "name"
		case tagsPath:
			store, key = &f.tags, "slug"
		default:
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			var want string
			if key == "name" {
				want = r.URL.Query().Get("name")
			} else {
				want = r.URL.Query().Get("slug")
			}
			var results []map[string]any
			for _, obj := range *store {
				if obj.name == want {
					results = append(results, obj.payload)
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"count": len(results), "next": nil, "results": results})
		case http.MethodPost:
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			id := f.nextID
			f.nextID++
			body["id"] = id
			var name string
			if key == "name" {
				name, _ = body["name"].(string)
			} else {
				name, _ = body["slug"].(string)
			}
			*store = append(*store, seedFakeObject{id: id, name: name, payload: body})
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(body)
		default:
			http.NotFound(w, r)
		}
	})
}

func newSeedTestClient(t *testing.T, f *seedFake) *Client {
	t.Helper()
	s := localServer(f.handler())
	t.Cleanup(s.Close)
	c, err := New(Config{BaseURL: s.URL, Token: "test-token"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func resultByName(results []SeedResult, name string) (SeedResult, bool) {
	for _, r := range results {
		if r.Name == name {
			return r, true
		}
	}
	return SeedResult{}, false
}

func TestSeedCreatesEverythingOnEmptyNetBox(t *testing.T) {
	f := newSeedFake()
	c := newSeedTestClient(t, f)
	results, err := c.Seed(context.Background())
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	wantCount := len(seedChoiceSets) + len(seedFields) + 1
	if len(results) != wantCount {
		t.Fatalf("got %d results, want %d", len(results), wantCount)
	}
	for _, r := range results {
		if r.Action != SeedCreated {
			t.Errorf("%s %s: action = %s, want created", r.Kind, r.Name, r.Action)
		}
	}
	for _, want := range seedFields {
		res, ok := resultByName(results, want.Name)
		if !ok {
			t.Errorf("missing result for field %s", want.Name)
			continue
		}
		if res.Kind != SeedKindCustomField {
			t.Errorf("field %s: kind = %s", want.Name, res.Kind)
		}
	}
	tagRes, ok := resultByName(results, ImportedTag)
	if !ok || tagRes.Kind != SeedKindTag {
		t.Errorf("missing tag result for %s", ImportedTag)
	}
}

func TestSeedIsIdempotent(t *testing.T) {
	f := newSeedFake()
	c := newSeedTestClient(t, f)
	if _, err := c.Seed(context.Background()); err != nil {
		t.Fatalf("first Seed: %v", err)
	}
	results, err := c.Seed(context.Background())
	if err != nil {
		t.Fatalf("second Seed: %v", err)
	}
	for _, r := range results {
		if r.Action != SeedPresent {
			t.Errorf("%s %s: action = %s, want present on second run", r.Kind, r.Name, r.Action)
		}
	}
}

// TestSeedConflictOnWrongType is a mutation kill for customFieldMatches: an
// existing field of a different type must never be silently accepted or
// rewritten, and every other field must still be reported.
func TestSeedConflictOnWrongType(t *testing.T) {
	f := newSeedFake()
	f.fields = append(f.fields, seedFakeObject{id: 999, name: allocationIDCF, payload: map[string]any{
		"id": 999, "name": allocationIDCF, "type": "integer", "object_types": []any{"ipam.prefix"},
	}})
	c := newSeedTestClient(t, f)
	results, err := c.Seed(context.Background())
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	res, ok := resultByName(results, allocationIDCF)
	if !ok || res.Action != SeedConflict {
		t.Fatalf("allocationIDCF result = %+v, want conflict", res)
	}
	if res.Detail == "" {
		t.Error("conflict result carries no detail")
	}
	other, ok := resultByName(results, allocationKeyCF)
	if !ok || other.Action != SeedCreated {
		t.Errorf("allocationKeyCF result = %+v, want created", other)
	}
}

// TestSeedConflictOnWrongObjectTypes is a mutation kill for the object_types
// half of customFieldMatches: the field list, not just the type, decides a
// conflict.
func TestSeedConflictOnWrongObjectTypes(t *testing.T) {
	f := newSeedFake()
	f.fields = append(f.fields, seedFakeObject{id: 999, name: ImportBatchField, payload: map[string]any{
		"id": 999, "name": ImportBatchField, "type": "text", "object_types": []any{"ipam.prefix"},
	}})
	c := newSeedTestClient(t, f)
	results, err := c.Seed(context.Background())
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	res, ok := resultByName(results, ImportBatchField)
	if !ok || res.Action != SeedConflict {
		t.Fatalf("ImportBatchField result = %+v, want conflict (missing ipam.iprange)", res)
	}
}

// TestSeedNeverRewritesAConflict proves the "never changed" half of the
// contract: after a conflicting Seed run, the existing object's payload in
// the fake store is untouched (no create/replace ever reaches it).
func TestSeedNeverRewritesAConflict(t *testing.T) {
	f := newSeedFake()
	f.fields = append(f.fields, seedFakeObject{id: 999, name: allocationIDCF, payload: map[string]any{
		"id": 999, "name": allocationIDCF, "type": "integer", "object_types": []any{"ipam.prefix"},
	}})
	c := newSeedTestClient(t, f)
	if _, err := c.Seed(context.Background()); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, obj := range f.fields {
		if obj.name == allocationIDCF {
			if obj.payload["type"] != "integer" {
				t.Errorf("conflicting field was rewritten: type = %v", obj.payload["type"])
			}
			return
		}
	}
	t.Fatal("conflicting field vanished from the fake store")
}

// TestSeedChoiceSetConflictFailsDependentFieldWithoutCrashing: a wrong
// choice set must not be silently accepted, and must not crash the select
// field that depends on it by sending a nil/zero choice_set.
func TestSeedChoiceSetConflictFailsDependentFieldWithoutCrashing(t *testing.T) {
	f := newSeedFake()
	f.choiceSets = append(f.choiceSets, seedFakeObject{id: 999, name: "platform-allocation-state", payload: map[string]any{
		"id": 999, "name": "platform-allocation-state",
		"extra_choices": []any{[]any{"WRONG", "WRONG"}},
	}})
	c := newSeedTestClient(t, f)
	results, err := c.Seed(context.Background())
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	setRes, _ := resultByName(results, "platform-allocation-state")
	if setRes.Action != SeedConflict {
		t.Fatalf("choice set result = %+v, want conflict", setRes)
	}
	fieldRes, _ := resultByName(results, stateCF)
	if fieldRes.Action != SeedConflict {
		t.Fatalf("dependent field %s result = %+v, want conflict", stateCF, fieldRes)
	}
}

// TestSeedReportsNetBoxUnreachable is a mutation kill for error propagation:
// a transport/HTTP failure must come back as an error, not be swallowed into
// a false "present"/"created" report.
func TestSeedReportsNetBoxUnreachable(t *testing.T) {
	f := newSeedFake()
	f.failPath = choiceSetsPath
	c := newSeedTestClient(t, f)
	_, err := c.Seed(context.Background())
	if err == nil {
		t.Fatal("Seed: want an error when NetBox is unreachable, got nil")
	}
}

func TestSeedReportsNetBoxUnreachableMidRun(t *testing.T) {
	f := newSeedFake()
	f.failPath = customFieldsPath
	c := newSeedTestClient(t, f)
	_, err := c.Seed(context.Background())
	if err == nil {
		t.Fatal("Seed: want an error when a custom-field request fails, got nil")
	}
}

// TestSeedMultipleExistingDefinitionsIsAnError guards against a data
// integrity surprise: two custom fields already sharing the "unique" name
// filter must never be silently resolved to one of them.
func TestSeedMultipleExistingDefinitionsIsAnError(t *testing.T) {
	f := newSeedFake()
	f.fields = append(f.fields,
		seedFakeObject{id: 1, name: allocationIDCF, payload: map[string]any{"id": 1, "name": allocationIDCF, "type": "text", "object_types": []any{"ipam.prefix"}}},
		seedFakeObject{id: 2, name: allocationIDCF, payload: map[string]any{"id": 2, "name": allocationIDCF, "type": "text", "object_types": []any{"ipam.prefix"}}},
	)
	c := newSeedTestClient(t, f)
	_, err := c.Seed(context.Background())
	if err == nil {
		t.Fatal("Seed: want an error for two existing definitions of the same field, got nil")
	}
}

// --- customFieldMatches / choiceSetMatches / sameStringSet unit coverage ---

func TestCustomFieldMatches(t *testing.T) {
	f := seedFieldSpec{Name: "x", Type: "text", ObjectTypes: []string{"ipam.prefix", "ipam.iprange"}}
	cases := []struct {
		name     string
		existing map[string]any
		want     bool
	}{
		{"exact match, order-independent", map[string]any{"type": "text", "object_types": []any{"ipam.iprange", "ipam.prefix"}}, true},
		{"choice-rendered type matches", map[string]any{"type": map[string]any{"value": "text", "label": "Text"}, "object_types": []any{"ipam.prefix", "ipam.iprange"}}, true},
		{"different type", map[string]any{"type": "integer", "object_types": []any{"ipam.prefix", "ipam.iprange"}}, false},
		{"missing object type", map[string]any{"type": "text", "object_types": []any{"ipam.prefix"}}, false},
		{"extra object type", map[string]any{"type": "text", "object_types": []any{"ipam.prefix", "ipam.iprange", "dcim.device"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := customFieldMatches(tc.existing, f); got != tc.want {
				t.Errorf("customFieldMatches(%v, %v) = %v, want %v", tc.existing, f, got, tc.want)
			}
		})
	}
}

func TestChoiceSetMatches(t *testing.T) {
	wanted := []string{"A", "B", "C"}
	cases := []struct {
		name     string
		existing map[string]any
		want     bool
	}{
		{"exact, reordered", map[string]any{"extra_choices": []any{[]any{"C", "C"}, []any{"A", "A"}, []any{"B", "B"}}}, true},
		{"missing a value", map[string]any{"extra_choices": []any{[]any{"A", "A"}, []any{"B", "B"}}}, false},
		{"extra value", map[string]any{"extra_choices": []any{[]any{"A", "A"}, []any{"B", "B"}, []any{"C", "C"}, []any{"D", "D"}}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := choiceSetMatches(tc.existing, wanted); got != tc.want {
				t.Errorf("choiceSetMatches(%v, %v) = %v, want %v", tc.existing, wanted, got, tc.want)
			}
		})
	}
}

// --- parity with deploy/compose/seed-netbox.py: the single source of truth ---

// The regexps below parse deploy/compose/seed-netbox.py's own FIELDS,
// CHOICE_SETS, SELECT_CHOICE_SETS, FIELD_OBJECT_TYPES and TAGS declarations.
// The script's tuple/dict literals are simple and deliberately kept that way
// so this parser does not need a Python grammar. This is the drift guard
// docs/WORK_PLAN.md package N4 asks for: the Go catalogue in seed.go is the
// single source of truth, and this test is what makes a silent divergence
// from deploy/compose/seed-netbox.py impossible to ship.

func seedNetboxPyPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// this file: <repo>/internal/netbox/seed_test.go
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
	path := filepath.Join(repoRoot, "deploy", "compose", "seed-netbox.py")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("deploy/compose/seed-netbox.py not found at %s: %v", path, err)
	}
	return path
}

var (
	pyFieldRe  = regexp.MustCompile(`\(\s*"([a-z_]+)"\s*,\s*"([a-z]+)"\s*\)`)
	pyChoiceRe = regexp.MustCompile(`(?s)"([a-z-]+)"\s*,\s*\[\s*([^\]]*)\]`)
	pyQuotedRe = regexp.MustCompile(`"([^"]*)"`)
	pyDictPair = regexp.MustCompile(`"([a-z_]+)"\s*:\s*"([a-z-]+)"`)
	pyImportRe = regexp.MustCompile(`"([a-z_]+)"\s*:\s*IMPORT_OBJECT_TYPES`)
	pyTagBlock = regexp.MustCompile(`(?s)TAGS\s*=\s*\(\s*\(\s*(.*?)\)\s*,?\s*\)`)
)

// pyBlock extracts the text between "<name> = (" or "<name> = {" and the
// matching top-level closing bracket, by counting nested brackets rather
// than a single non-greedy regex -- CHOICE_SETS' second entry spans several
// lines and itself contains a "]" that a naive "up to the first )" match
// would stop at too early.
func pyBlock(t *testing.T, src, name string) string {
	t.Helper()
	start := strings.Index(src, name+" = ")
	if start < 0 {
		t.Fatalf("could not find %q in seed-netbox.py", name)
	}
	rest := src[start+len(name+" = "):]
	if len(rest) == 0 {
		t.Fatalf("%s has no body", name)
	}
	open := rest[0]
	var close byte
	switch open {
	case '(':
		close = ')'
	case '{':
		close = '}'
	default:
		t.Fatalf("%s does not open with ( or {: %q", name, rest[:20])
	}
	depth := 0
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return rest[1:i]
			}
		}
	}
	t.Fatalf("unbalanced brackets for %s", name)
	return ""
}

func TestSeedFieldsMatchPythonScript(t *testing.T) {
	path := seedNetboxPyPath(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	src := string(raw)

	fieldsBlock := pyBlock(t, src, "FIELDS")
	var pyFields []seedFieldSpec
	for _, m := range pyFieldRe.FindAllStringSubmatch(fieldsBlock, -1) {
		pyFields = append(pyFields, seedFieldSpec{Name: m[1], Type: m[2]})
	}
	if len(pyFields) != len(seedFields) {
		t.Fatalf("seed-netbox.py FIELDS has %d entries, seedFields has %d", len(pyFields), len(seedFields))
	}
	for i, py := range pyFields {
		g := seedFields[i]
		if py.Name != g.Name || py.Type != g.Type {
			t.Errorf("field %d: python (%s, %s) != go (%s, %s)", i, py.Name, py.Type, g.Name, g.Type)
		}
	}

	choicesBlock := pyBlock(t, src, "CHOICE_SETS")
	pyChoiceMatches := pyChoiceRe.FindAllStringSubmatch(choicesBlock, -1)
	if len(pyChoiceMatches) != len(seedChoiceSets) {
		t.Fatalf("seed-netbox.py CHOICE_SETS has %d entries, seedChoiceSets has %d", len(pyChoiceMatches), len(seedChoiceSets))
	}
	for i, m := range pyChoiceMatches {
		wantName := m[1]
		var wantChoices []string
		for _, q := range pyQuotedRe.FindAllStringSubmatch(m[2], -1) {
			wantChoices = append(wantChoices, q[1])
		}
		got := seedChoiceSets[i]
		if got.Name != wantName {
			t.Errorf("choice set %d: python name %s != go name %s", i, wantName, got.Name)
		}
		if len(got.Choices) != len(wantChoices) || !sameStringSet(got.Choices, wantChoices) {
			t.Errorf("choice set %s: python choices %v != go choices %v", wantName, wantChoices, got.Choices)
		}
	}

	selectBlock := pyBlock(t, src, "SELECT_CHOICE_SETS")
	pySelect := map[string]string{}
	for _, m := range pyDictPair.FindAllStringSubmatch(selectBlock, -1) {
		pySelect[m[1]] = m[2]
	}
	for _, f := range seedFields {
		if f.Type == "select" {
			if pySelect[f.Name] != f.ChoiceSet {
				t.Errorf("field %s: python choice set %q != go choice set %q", f.Name, pySelect[f.Name], f.ChoiceSet)
			}
		} else if _, ok := pySelect[f.Name]; ok {
			t.Errorf("field %s: python marks it select via SELECT_CHOICE_SETS but go type is %q", f.Name, f.Type)
		}
	}

	objTypesBlock := pyBlock(t, src, "FIELD_OBJECT_TYPES")
	pyImportFields := map[string]bool{}
	for _, m := range pyImportRe.FindAllStringSubmatch(objTypesBlock, -1) {
		pyImportFields[m[1]] = true
	}
	for _, f := range seedFields {
		isImportField := len(f.ObjectTypes) == len(importObjectTypes) && sameStringSet(f.ObjectTypes, importObjectTypes)
		if isImportField != pyImportFields[f.Name] {
			t.Errorf("field %s: import-object-types mismatch (go=%v, python=%v)", f.Name, isImportField, pyImportFields[f.Name])
		}
	}

	tagMatch := pyTagBlock.FindStringSubmatch(src)
	if tagMatch == nil {
		t.Fatal("could not find TAGS block in seed-netbox.py")
	}
	tagStrings := pyQuotedRe.FindAllStringSubmatch(tagMatch[1], -1)
	if len(tagStrings) != 3 {
		t.Fatalf("TAGS entry has %d quoted strings, want 3 (name, slug, description)", len(tagStrings))
	}
	pyTagName, pyTagSlug, pyTagDesc := tagStrings[0][1], tagStrings[1][1], tagStrings[2][1]
	if pyTagName != ImportedTag || pyTagSlug != ImportedTag {
		t.Errorf("python tag (%s, %s) != go ImportedTag %s", pyTagName, pyTagSlug, ImportedTag)
	}
	if pyTagDesc != seedTagDescription {
		t.Errorf("python tag description %q != go seedTagDescription %q", pyTagDesc, seedTagDescription)
	}
}

func TestSeedHelpersAreOrderAndDuplicateSensitiveWhereItMatters(t *testing.T) {
	if !sameStringSet([]string{"a", "b"}, []string{"b", "a"}) {
		t.Error("sameStringSet should ignore order")
	}
	if sameStringSet([]string{"a", "a"}, []string{"a", "b"}) {
		t.Error("sameStringSet should not treat a duplicate as covering a missing value")
	}
	got := append([]string{}, importObjectTypes...)
	sort.Strings(got)
	if got[0] != "ipam.iprange" || got[1] != "ipam.prefix" {
		t.Errorf("importObjectTypes = %v, want sorted [ipam.iprange ipam.prefix]", importObjectTypes)
	}
}
