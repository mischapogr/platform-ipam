package migrate

import (
	"fmt"
	"strings"

	"github.com/mischapogr/platform-ipam/internal/assess"
)

// forbiddenPhrases is ADR 0015's extended deny list: "the forbidden list
// extends ADR 0014's with the words a migration report would reach for:
// conflict-free, no conflicts, ready, clean, complete, done, migrated, cut
// over, reachable, unreachable." Every word here is checked, case
// insensitively, against this package's OWN generated prose -- Summary.
// Sentence and the two fixed Notes -- and the full text-format rendering
// (report_test.go's assertNoForbiddenPhrase). It is deliberately NOT
// checked against the raw JSON's embedded, verbatim-carried
// Assessment.Coverage/Assessment.Summary (ADR 0014's own template
// legitimately contains the word "complete", and the mandated
// `evidence.complete` field name is a schema element, not prose) -- see
// assertNoForbiddenPhrase's own doc comment and this package's report to
// the lead for the full reasoning.
var forbiddenPhrases = []string{
	"conflict-free",
	"no conflicts",
	"ready",
	"clean",
	"complete",
	"done",
	"migrated",
	"cut over",
	"reachable",
	"unreachable",
}

// ForbiddenPhrases returns a copy of the deny list this package's prose
// must never contain.
func ForbiddenPhrases() []string {
	out := make([]string, len(forbiddenPhrases))
	copy(out, forbiddenPhrases)
	return out
}

// renderSentence selects one of exactly two templates by evComplete (ADR
// 0015, "Coverage, the two templates, the forbidden list and no
// percentage"): "Every rendering of the summary is selected by that field
// from exactly two templates. There is no third template and no free-text
// summary anywhere in the tool." Neither template uses any word in
// forbiddenPhrases, in either branch -- not only the incomplete one, which
// is all the record's own test literally requires -- so this package's own
// prose never has to be audited for which branch a forbidden word happens
// to be safe in.
//
// stamp is accepted for symmetry with internal/assess's own renderSentence,
// which appends it to the complete-case sentence when the total is zero;
// this package's complete-case sentence always has something to say (the
// fully-evidenced count and its denominator), so stamp is not interpolated
// here -- it still reaches the report through Report.Stamp itself.
func renderSentence(evComplete bool, movesTotal, fullyEvidenced, missingAccounts, missingRegionPairs int, uncoveredTenants []string, noEvidenceSupplied bool, stamp string) string {
	if !evComplete {
		var clauses []string
		if missingAccounts > 0 || missingRegionPairs > 0 {
			clauses = append(clauses, fmt.Sprintf("the underlying assessment could not read %d account(s) and %d account-region pair(s)", missingAccounts, missingRegionPairs))
		}
		if noEvidenceSupplied {
			clauses = append(clauses, "no allocation evidence was supplied at all")
		} else if len(uncoveredTenants) > 0 {
			clauses = append(clauses, fmt.Sprintf("no evidence could have seen %d tenant(s)' allocations (%s)", len(uncoveredTenants), strings.Join(uncoveredTenants, ", ")))
		}
		if len(clauses) == 0 {
			// Defensive: evComplete is false but neither cause above fired.
			// This should not be reachable given evidenceIndex.complete's
			// own rule, but the incomplete template must always name
			// something rather than assert a full statement by omission.
			clauses = append(clauses, "the evidence behind this report's facts is not fully accounted for")
		}
		return "this progress report cannot make a full statement: " + strings.Join(clauses, "; ")
	}
	return fmt.Sprintf("every account and account-region pair this plan's subjects touch was read, and evidence was supplied for every tenant this plan's targets name; %d of %d move(s) have subject, target and conflicts all affirmative at once", fullyEvidenced, movesTotal)
}

// disclaimerNotes returns the two fixed, once-only disclaimers ADR 0015
// requires, always in this order: the assessment's own reachability
// disclaimer carried in verbatim, and this report's own scope disclaimer.
// "The assessment's reachability disclaimer is carried into this report
// verbatim and printed once ... Beside it the report prints, once, that it
// describes address compatibility for the target design the plan names,
// and nothing about routes, Transit Gateway attachments, propagations,
// security controls, DNS or application connectivity."
func disclaimerNotes(report assess.Report) []string {
	reach := ""
	for _, n := range report.Notes {
		if n.Kind == assess.NoteReachability {
			reach = n.Message
			break
		}
	}
	if reach == "" {
		// Defensive fallback matching assess.reachabilityNote()'s own fixed
		// text; assess.Assess always includes this note first, so this
		// branch is not expected to run against a real assess.Report.
		reach = "no route table, Transit Gateway attachment, propagation or traffic test was read; nothing in this report asserts that any two workloads can or cannot reach each other"
	}
	scope := "this report describes address compatibility for the target design the plan names, and nothing about routes, Transit Gateway attachments, propagations, security controls, DNS or application connectivity"
	return []string{reach, scope}
}
