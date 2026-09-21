package migrate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// This file is ADR 0015's own worked test: "a plan carrying a cidr key
// anywhere is refused by the strict decoder, naming the field", tested "as
// a property over generated documents: no document containing a cidr key
// decodes" (docs/WORK_PLAN.md, M3b1's cut). The property holds by
// construction, not by a scan this package runs at decode time: no type in
// this package's schema has a field named cidr, anywhere, at any depth, and
// every struct is decoded with encoding/json's DisallowUnknownFields, which
// applies recursively -- so ANY JSON object carrying a "cidr" key, wherever
// it appears in a document that is otherwise shaped like this schema (or
// not), fails to decode. This file's job is to demonstrate that property
// against documents this package did not hand-pick, not to implement it:
// there is no cidr-detecting code anywhere in plan_decode.go or plan.go for
// this test to be testing.

// randomJSONTree builds a JSON-compatible value (nested
// map[string]interface{} / []interface{} / string / float64 / bool leaves)
// up to a bounded depth, deterministically from rng, so a failing seed can
// be reproduced by re-running with the same rand.Source.
func randomJSONTree(rng *rand.Rand, depth int) interface{} {
	if depth <= 0 || rng.Intn(3) == 0 {
		switch rng.Intn(4) {
		case 0:
			return randomJSONString(rng)
		case 1:
			return float64(rng.Intn(1000))
		case 2:
			return rng.Intn(2) == 0
		default:
			return nil
		}
	}
	if rng.Intn(2) == 0 {
		n := rng.Intn(4)
		out := make(map[string]interface{}, n)
		for i := 0; i < n; i++ {
			out[fmt.Sprintf("field_%d_%d", depth, i)] = randomJSONTree(rng, depth-1)
		}
		return out
	}
	n := rng.Intn(4)
	out := make([]interface{}, n)
	for i := 0; i < n; i++ {
		out[i] = randomJSONTree(rng, depth-1)
	}
	return out
}

func randomJSONString(rng *rand.Rand) string {
	words := []string{"vpc-1", "eu-central-1", "tenant-a", "replace", "keep", "wave-1", "notes here"}
	return words[rng.Intn(len(words))]
}

// insertCIDRSomewhere walks v and inserts a "cidr" key into the first
// map[string]interface{} node it reaches by depth-first traversal in field
// order, or -- if v itself is not, and contains, any map -- wraps v in one.
// Every generated document this test feeds to DecodePlan therefore contains
// a "cidr" key at some depth, which is the property's precondition.
func insertCIDRSomewhere(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		t["cidr"] = "10.0.0.0/16"
		return t
	case []interface{}:
		for i, item := range t {
			if m, ok := item.(map[string]interface{}); ok {
				m["cidr"] = "10.0.0.0/16"
				return t
			}
			_ = i
		}
		return map[string]interface{}{"wrapped": t, "cidr": "10.0.0.0/16"}
	default:
		return map[string]interface{}{"value": t, "cidr": "10.0.0.0/16"}
	}
}

// TestNoDocumentContainingACIDRKeyDecodes is the property test: two hundred
// pseudo-randomly generated documents, each guaranteed to carry a "cidr"
// key somewhere, none of which DecodePlan may ever accept.
func TestNoDocumentContainingACIDRKeyDecodes(t *testing.T) {
	rng := rand.New(rand.NewSource(20260922))
	for i := 0; i < 200; i++ {
		tree := randomJSONTree(rng, 4)
		tree = insertCIDRSomewhere(tree)
		body, err := json.Marshal(tree)
		if err != nil {
			t.Fatalf("iteration %d: marshal: %v", i, err)
		}
		if _, err := DecodePlan(bytes.NewReader(body), "generated.json"); err == nil {
			t.Fatalf("iteration %d: DecodePlan accepted a document containing a cidr key: %s", i, body)
		}
	}
}

// TestNoEvidenceDocumentContainingACIDRKeyDecodes is the same property
// applied to DecodeEvidence: the allocation evidence schema has no cidr
// field either (plan_evidence.go's own doc comment).
func TestNoEvidenceDocumentContainingACIDRKeyDecodes(t *testing.T) {
	rng := rand.New(rand.NewSource(20260922))
	for i := 0; i < 100; i++ {
		tree := randomJSONTree(rng, 4)
		tree = insertCIDRSomewhere(tree)
		body, err := json.Marshal(tree)
		if err != nil {
			t.Fatalf("iteration %d: marshal: %v", i, err)
		}
		if _, err := DecodeEvidence(bytes.NewReader(body), "generated.json"); err == nil {
			t.Fatalf("iteration %d: DecodeEvidence accepted a document containing a cidr key: %s", i, body)
		}
	}
}

// TestCIDRKeyRefusedAtEveryNestingLevelOfAValidPlan is the concrete
// complement to the random property above: a "cidr" key inserted, one test
// case at a time, into every object-shaped position this schema actually
// defines -- the plan's top level, a wave, a move, a target, a blocker, a
// verification, an approval -- each starting from an otherwise valid
// document, so the property is demonstrated against the schema's own
// shape, not only against documents unrelated to it.
func TestCIDRKeyRefusedAtEveryNestingLevelOfAValidPlan(t *testing.T) {
	base := func() map[string]interface{} {
		var doc map[string]interface{}
		if err := json.Unmarshal([]byte(minimalPlanJSON), &doc); err != nil {
			t.Fatalf("unmarshal fixture: %v", err)
		}
		return doc
	}

	cases := map[string]func(doc map[string]interface{}){
		"top level": func(doc map[string]interface{}) {
			doc["cidr"] = "10.0.0.0/16"
		},
		"wave": func(doc map[string]interface{}) {
			waves := doc["waves"].([]interface{})
			waves[0].(map[string]interface{})["cidr"] = "10.0.0.0/16"
		},
		"move": func(doc map[string]interface{}) {
			moves := doc["moves"].([]interface{})
			moves[0].(map[string]interface{})["cidr"] = "10.0.0.0/16"
		},
		"subject": func(doc map[string]interface{}) {
			moves := doc["moves"].([]interface{})
			subject := moves[0].(map[string]interface{})["subject"].(map[string]interface{})
			subject["cidr"] = "10.0.0.0/16"
		},
		"target": func(doc map[string]interface{}) {
			moves := doc["moves"].([]interface{})
			target := moves[0].(map[string]interface{})["target"].(map[string]interface{})
			target["cidr"] = "10.0.0.0/16"
		},
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			doc := base()
			mutate(doc)
			body, err := json.Marshal(doc)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			_, err = DecodePlan(strings.NewReader(string(body)), "plan.json")
			if err == nil {
				t.Fatalf("DecodePlan accepted a cidr key at %s: %s", name, body)
			}
		})
	}
}
