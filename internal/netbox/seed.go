// Package netbox: seed.go (docs/WORK_PLAN.md package N4, found by M9b1's
// review note). Every custom field, choice set and tag the rest of this
// package relies on today is created by hand or by
// deploy/compose/seed-netbox.py, which refuses any non-development endpoint
// -- so an import against stage or production NetBox fails with a bare 400
// until an operator creates the fields there by hand. Seed is the mode the
// documentation (docs/DEPLOYMENT.md's process-mode table, line 66;
// docs/NETBOX_INTEGRATION.md section 6) already promises: it creates or
// verifies the catalogue below, idempotently, and never mutates an existing
// definition -- an existing custom field of another `type`, or whose
// `object_types` differ from what this file expects, is reported as a
// conflict and left exactly as it is (the same rule seed-netbox.py's own
// ensure() applies to the wider development contract it seeds).
//
// This file is deliberately the ONLY place in this package that lists the
// catalogue: allocationIDCF, ImportBatchField and the rest are declared once
// each, in client.go, occupancy.go and refresh.go, and reused here rather
// than duplicated as string literals -- but several fields (platform_tenant_
// id, platform_environment, ...) exist today only as literals inline in
// client.go's ownedFields, so they are repeated here as literals too, next to
// a comment pointing at their one call site. seed_test.go is what keeps this
// list complete: it parses deploy/compose/seed-netbox.py's own FIELDS,
// SELECT_CHOICE_SETS, CHOICE_SETS, FIELD_OBJECT_TYPES and TAGS declarations
// and asserts they are byte-for-byte the same catalogue as seedFields,
// seedChoiceSets and the import tag below -- so the two can never drift
// silently apart. Go is the source of truth precisely because it is the one
// of the two that a compile error, not just a test, can catch drifting from
// the rest of this package's own field constants.
//
// Seed never runs as part of an onboarding import (occupancy.go, adopt.go,
// abandon.go, cancel.go, refresh.go, remove.go are untouched by this file):
// it is a boot-time correctness check an operator runs once, before the
// first import against a new NetBox, and again after an upgrade that adds a
// field.
package netbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

// Seed actions, reported per object.
const (
	SeedCreated  = "created"
	SeedPresent  = "present"
	SeedConflict = "conflict"
)

// Seed object kinds.
const (
	SeedKindChoiceSet   = "choice_set"
	SeedKindCustomField = "custom_field"
	SeedKindTag         = "tag"
)

// SeedResult reports what Seed did, or found, for one object.
type SeedResult struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Action string `json:"action"`
	// Detail is set only for SeedConflict: what disagreed with what this
	// file expects, so an operator can decide whether to fix the field by
	// hand or fix this catalogue.
	Detail string `json:"detail,omitempty"`
}

// seedFieldSpec is one entry of the custom-field catalogue.
type seedFieldSpec struct {
	Name string
	// Type is NetBox's own custom-field "type" value: text, select,
	// datetime, json or boolean. Every value used below is one
	// deploy/compose/seed-netbox.py has already created against the pinned
	// image (ADR 0016's dated paragraphs measured json and boolean; the
	// rest predate this package).
	Type string
	// ObjectTypes are the NetBox content-type slugs (app_label.model, e.g.
	// "ipam.prefix") this field applies to.
	ObjectTypes []string
	// ChoiceSet names a seedChoiceSets entry; non-empty only when Type ==
	// "select".
	ChoiceSet string
}

// seedChoiceSet is one custom-field-choice-set the catalogue's select fields
// reference. NetBox requires every selection custom field to name a choice
// set rather than carrying its own inline choices.
type seedChoiceSet struct {
	Name    string
	Choices []string
}

// importObjectTypes: the onboarding import writes platform_import_batch and
// platform_import_source on a range as well as a prefix (occupancy.go's
// EnsureOccupancy is the only caller, for both OccupancyPrefix and
// OccupancyRange); every other field in this catalogue is prefix-only.
var importObjectTypes = []string{"ipam.iprange", "ipam.prefix"}

var prefixOnly = []string{"ipam.prefix"}

// seedChoiceSets: values taken from the implementation, not invented here --
// platform-allocation-state mirrors internal/domain's lifecycle states, and
// platform-drift-status mirrors the finding codes the worker actually emits
// (internal/service/worker.go). The NetBox adapter does not populate
// platform_drift_status yet; the field and its choice set exist so operator
// filters have a stable vocabulary once it does -- exactly
// deploy/compose/seed-netbox.py's own CHOICE_SETS, reproduced here so the two
// never drift silently (seed_test.go).
var seedChoiceSets = []seedChoiceSet{
	{Name: "platform-allocation-state", Choices: []string{
		domain.Reserved, domain.Active, domain.Quarantined, domain.Released,
	}},
	{Name: "platform-drift-status", Choices: []string{
		"none", "coverage_incomplete", "unmanaged_occupancy", "resource_missing", "binding_mismatch",
	}},
}

// seedFields is the complete custom-field catalogue this adapter relies on,
// in the same order as deploy/compose/seed-netbox.py's FIELDS so a diff
// between the two reads cleanly. Named constants are reused wherever this
// package already declares one (client.go, occupancy.go, refresh.go); the
// rest are client.go's ownedFields literals, named here as plain strings
// next to that call site.
var seedFields = []seedFieldSpec{
	{Name: allocationIDCF, Type: "text", ObjectTypes: prefixOnly},
	{Name: allocationKeyCF, Type: "text", ObjectTypes: prefixOnly},
	{Name: operationIDCF, Type: "text", ObjectTypes: prefixOnly},
	{Name: parentAllocationIDCF, Type: "text", ObjectTypes: prefixOnly},
	// client.go's ownedFields writes these four under literal keys; there is
	// no named constant for them today (see this file's package comment).
	{Name: "platform_tenant_id", Type: "text", ObjectTypes: prefixOnly},
	{Name: "platform_environment", Type: "text", ObjectTypes: prefixOnly},
	{Name: "platform_pool_id", Type: "text", ObjectTypes: prefixOnly},
	{Name: "platform_policy_version", Type: "text", ObjectTypes: prefixOnly},
	{Name: stateCF, Type: "select", ObjectTypes: prefixOnly, ChoiceSet: "platform-allocation-state"},
	{Name: awsAccountCF, Type: "text", ObjectTypes: prefixOnly},
	{Name: awsRegionCF, Type: "text", ObjectTypes: prefixOnly},
	{Name: awsResourceCF, Type: "text", ObjectTypes: prefixOnly},
	{Name: "platform_aws_az_id", Type: "text", ObjectTypes: prefixOnly},
	{Name: "platform_quarantine_until", Type: "datetime", ObjectTypes: prefixOnly},
	{Name: "platform_last_observed_at", Type: "datetime", ObjectTypes: prefixOnly},
	{Name: "platform_drift_status", Type: "select", ObjectTypes: prefixOnly, ChoiceSet: "platform-drift-status"},
	{Name: ImportBatchField, Type: "text", ObjectTypes: importObjectTypes},
	{Name: ImportSourceField, Type: "text", ObjectTypes: importObjectTypes},
	{Name: ImportContributorsField, Type: "json", ObjectTypes: prefixOnly},
	{Name: ImportContributorsReconstructedField, Type: "boolean", ObjectTypes: prefixOnly},
}

// seedTagSlug and seedTagDescription mirror deploy/compose/seed-netbox.py's
// one TAGS entry: tag assignment on create resolves an existing tag by slug
// and fails otherwise, so the import adapter cannot create this itself.
const seedTagDescription = "occupancy imported by platform-ipam onboarding; not a platform allocation"

// Seed creates or verifies every choice set, custom field and tag this
// package relies on. It processes every object rather than stopping at the
// first conflict, so one run gives a complete picture; nothing already
// present is ever modified. The returned slice is always in catalogue order
// (choice sets, then fields, then the tag), truncated at the point a NetBox
// request itself failed (network error or non-2xx) -- that failure is
// returned as an error, distinct from a conflict.
func (c *Client) Seed(ctx context.Context) ([]SeedResult, error) {
	var results []SeedResult
	choiceSetIDs := make(map[string]int, len(seedChoiceSets))
	conflictedChoiceSets := make(map[string]bool, len(seedChoiceSets))
	for _, set := range seedChoiceSets {
		res, id, err := c.ensureChoiceSet(ctx, set)
		if err != nil {
			return results, err
		}
		results = append(results, res)
		if res.Action == SeedConflict {
			conflictedChoiceSets[set.Name] = true
		} else {
			choiceSetIDs[set.Name] = id
		}
	}
	for _, f := range seedFields {
		if f.ChoiceSet != "" && conflictedChoiceSets[f.ChoiceSet] {
			results = append(results, SeedResult{Kind: SeedKindCustomField, Name: f.Name, Action: SeedConflict,
				Detail: fmt.Sprintf("choice set %q is itself in conflict", f.ChoiceSet)})
			continue
		}
		res, err := c.ensureCustomField(ctx, f, choiceSetIDs)
		if err != nil {
			return results, err
		}
		results = append(results, res)
	}
	res, err := c.ensureTag(ctx, ImportedTag, seedTagDescription)
	if err != nil {
		return results, err
	}
	results = append(results, res)
	return results, nil
}

// seedFindOne looks a NetBox object up by an exact filter query (e.g.
// "name=platform_state") and returns it, or nil if none exists. More than
// one match is reported as an error by the caller -- defensive, since every
// filter used here is on a field NetBox itself treats as unique.
func (c *Client) seedFindOne(ctx context.Context, path, query string) (map[string]any, error) {
	resp, err := c.request(ctx, http.MethodGet, path+"?"+query, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var page struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return nil, fmt.Errorf("decode netbox response for %s: %w", path, err)
	}
	if len(page.Results) > 1 {
		return nil, fmt.Errorf("multiple existing definitions matched %s?%s", path, query)
	}
	if len(page.Results) == 1 {
		return page.Results[0], nil
	}
	return nil, nil
}

// choiceSetsPath, customFieldsPath and tagsPath are the NetBox endpoints
// this file reads and writes, mirroring deploy/compose/seed-netbox.py's own
// paths.
const (
	choiceSetsPath   = "/api/extras/custom-field-choice-sets/"
	customFieldsPath = "/api/extras/custom-fields/"
	tagsPath         = "/api/extras/tags/"
)

func (c *Client) ensureChoiceSet(ctx context.Context, set seedChoiceSet) (SeedResult, int, error) {
	existing, err := c.seedFindOne(ctx, choiceSetsPath, "name="+set.Name)
	if err != nil {
		return SeedResult{}, 0, err
	}
	if existing != nil {
		id, _ := existing["id"].(float64)
		if !choiceSetMatches(existing, set.Choices) {
			return SeedResult{Kind: SeedKindChoiceSet, Name: set.Name, Action: SeedConflict,
				Detail: "existing choice set's choices differ from " + set.Name}, 0, nil
		}
		return SeedResult{Kind: SeedKindChoiceSet, Name: set.Name, Action: SeedPresent}, int(id), nil
	}
	extraChoices := make([][2]string, len(set.Choices))
	for i, v := range set.Choices {
		extraChoices[i] = [2]string{v, v}
	}
	payload := map[string]any{"name": set.Name, "extra_choices": extraChoices}
	resp, err := c.request(ctx, http.MethodPost, choiceSetsPath, payload)
	if err != nil {
		return SeedResult{}, 0, err
	}
	defer resp.Body.Close()
	var created struct {
		ID int `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		return SeedResult{}, 0, fmt.Errorf("decode created choice set %s: %w", set.Name, err)
	}
	return SeedResult{Kind: SeedKindChoiceSet, Name: set.Name, Action: SeedCreated}, created.ID, nil
}

// choiceSetMatches compares only the set of values NetBox echoes in
// extra_choices (each entry a [value, label] pair) against wanted, ignoring
// order -- NetBox does not promise to return them in the order they were
// sent.
func choiceSetMatches(existing map[string]any, wanted []string) bool {
	raw, _ := existing["extra_choices"].([]any)
	got := make([]string, 0, len(raw))
	for _, entry := range raw {
		pair, ok := entry.([]any)
		if !ok || len(pair) == 0 {
			return false
		}
		v, ok := pair[0].(string)
		if !ok {
			return false
		}
		got = append(got, v)
	}
	return sameStringSet(got, wanted)
}

func (c *Client) ensureCustomField(ctx context.Context, f seedFieldSpec, choiceSetIDs map[string]int) (SeedResult, error) {
	existing, err := c.seedFindOne(ctx, customFieldsPath, "name="+f.Name)
	if err != nil {
		return SeedResult{}, err
	}
	if existing != nil {
		if !customFieldMatches(existing, f) {
			return SeedResult{Kind: SeedKindCustomField, Name: f.Name, Action: SeedConflict,
				Detail: fmt.Sprintf("existing custom field %s has a different type or object_types", f.Name)}, nil
		}
		return SeedResult{Kind: SeedKindCustomField, Name: f.Name, Action: SeedPresent}, nil
	}
	payload := map[string]any{"name": f.Name, "type": f.Type, "object_types": f.ObjectTypes}
	if f.ChoiceSet != "" {
		id, ok := choiceSetIDs[f.ChoiceSet]
		if !ok {
			return SeedResult{Kind: SeedKindCustomField, Name: f.Name, Action: SeedConflict,
				Detail: fmt.Sprintf("choice set %q was not created", f.ChoiceSet)}, nil
		}
		payload["choice_set"] = id
	}
	resp, err := c.request(ctx, http.MethodPost, customFieldsPath, payload)
	if err != nil {
		return SeedResult{}, err
	}
	resp.Body.Close()
	return SeedResult{Kind: SeedKindCustomField, Name: f.Name, Action: SeedCreated}, nil
}

// customFieldMatches checks only what docs/WORK_PLAN.md package N4 calls a
// conflict: an existing field of another type, or a different object_types
// set. It deliberately does not compare choice_set identity (a select
// field's choice set can be re-pointed by an operator without becoming a
// different field) or any other attribute an operator is free to edit
// (label, description, required, filter logic, ...).
func customFieldMatches(existing map[string]any, f seedFieldSpec) bool {
	if fieldTypeValue(existing["type"]) != f.Type {
		return false
	}
	raw, _ := existing["object_types"].([]any)
	got := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			return false
		}
		got = append(got, s)
	}
	return sameStringSet(got, f.ObjectTypes)
}

// fieldTypeValue extracts a custom field's "type" whether NetBox rendered it
// as a bare string or, as its other enumerated fields do, as a
// {"value","label"} object.
func fieldTypeValue(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case map[string]any:
		if s, ok := t["value"].(string); ok {
			return s
		}
	}
	return ""
}

func (c *Client) ensureTag(ctx context.Context, slug, description string) (SeedResult, error) {
	existing, err := c.seedFindOne(ctx, tagsPath, "slug="+slug)
	if err != nil {
		return SeedResult{}, err
	}
	if existing != nil {
		// A tag has no "type" to conflict on; matching by slug (its unique
		// key) is the only definition of "the same tag" NetBox offers.
		return SeedResult{Kind: SeedKindTag, Name: slug, Action: SeedPresent}, nil
	}
	payload := map[string]any{"name": slug, "slug": slug, "description": description}
	resp, err := c.request(ctx, http.MethodPost, tagsPath, payload)
	if err != nil {
		return SeedResult{}, err
	}
	resp.Body.Close()
	return SeedResult{Kind: SeedKindTag, Name: slug, Action: SeedCreated}, nil
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as := append([]string(nil), a...)
	bs := append([]string(nil), b...)
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}
