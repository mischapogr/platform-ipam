package migrate

import (
	"strings"
	"testing"

	"github.com/mischapogr/platform-ipam/internal/assess"
)

// renderBoth writes both encodings of r into strings.
func renderBoth(t *testing.T, r Report) (jsonText, textText string) {
	t.Helper()
	var jb, tb strings.Builder
	if err := WriteReportJSON(&jb, r); err != nil {
		t.Fatalf("WriteReportJSON: %v", err)
	}
	if err := WriteReportText(&tb, r); err != nil {
		t.Fatalf("WriteReportText: %v", err)
	}
	return jb.String(), tb.String()
}

// assertNoForbiddenPhrase scans this package's OWN generated prose --
// Report.Summary.Sentence and the two fixed Report.Notes -- plus the full
// text-format rendering, for every member of ForbiddenPhrases(), case
// insensitively.
//
// It deliberately does NOT scan the raw JSON rendering whole. ADR 0015
// requires the assessment's own Coverage and Summary to be carried into
// Report.Assessment "verbatim" -- unchanged from ADR 0014, whose OWN
// forbidden list does not include "complete", and whose own incomplete
// template legitimately contains the English word "complete" ("this report
// cannot make a complete statement for ..."). ADR 0015 separately extends
// ITS forbidden list to include "complete", which -- applied to the whole
// JSON blob -- would make both the record's own mandated `evidence.complete`
// field name and the verbatim-carried upstream ADR 0014 sentence violate the
// guard, for reasons that have nothing to do with THIS package's own prose.
// Rewriting internal/assess's already-accepted template is out of scope
// here. The text-format rendering is scanned in full because
// WriteReportText never re-prints the embedded assessment's own verbatim
// sentence or its coverage.complete field (see report_text.go) -- so a
// forbidden word appearing there can only be this package's own doing. See
// the report to the lead for this reading.
func assertNoForbiddenPhrase(t *testing.T, r Report) {
	t.Helper()
	ownProse := r.Summary.Sentence + " " + strings.Join(r.Notes, " ")
	_, textText := renderBoth(t, r)
	for _, phrase := range ForbiddenPhrases() {
		if strings.Contains(strings.ToLower(ownProse), phrase) {
			t.Errorf("this package's own prose (Summary.Sentence + Notes) contains forbidden phrase %q: %q", phrase, ownProse)
		}
		if strings.Contains(strings.ToLower(textText), phrase) {
			t.Errorf("text output contains forbidden phrase %q\n%s", phrase, textText)
		}
	}
}

func assertNoPercentSign(t *testing.T, r Report) {
	t.Helper()
	jsonText, textText := renderBoth(t, r)
	if strings.Contains(jsonText, "%") {
		t.Errorf("JSON output contains a %% character:\n%s", jsonText)
	}
	if strings.Contains(textText, "%") {
		t.Errorf("text output contains a %% character:\n%s", textText)
	}
}

func assertIncompleteTemplateChosen(t *testing.T, r Report) {
	t.Helper()
	if !strings.Contains(r.Summary.Sentence, "this progress report cannot make a full statement") {
		t.Errorf("sentence = %q, want the incomplete template's fixed substring", r.Summary.Sentence)
	}
	if strings.Contains(r.Summary.Sentence, "have subject, target and conflicts all affirmative at once") {
		t.Errorf("sentence = %q, must never contain the complete template's own clause", r.Summary.Sentence)
	}
}

func assertCompleteTemplateChosen(t *testing.T, r Report) {
	t.Helper()
	if !strings.Contains(r.Summary.Sentence, "have subject, target and conflicts all affirmative at once") {
		t.Errorf("sentence = %q, want the complete template's fixed substring", r.Summary.Sentence)
	}
	if strings.Contains(r.Summary.Sentence, "this progress report cannot make a full statement") {
		t.Errorf("sentence = %q, must never contain the incomplete template's own clause", r.Summary.Sentence)
	}
}

// --- head property 2 (part): the forbidden list and no % character, over
// every route to incompleteness, not just one hand-picked example. ---

func TestForbiddenPhraseAndPercentGuardAcrossIncompleteRoutes(t *testing.T) {
	target := baseTarget()
	notObserved := readWhole(vpcRec("111111111111", "us-east-1", "vpc-other", "10.9.0.0/16", "networks.csv", 1))
	notObservedReport := mustAssess(t, notObserved, assess.Options{})
	baseMove := Move{Subject: subject("111111111111", "us-east-1", "vpc-a"), Disposition: DispositionReplace, Target: target}
	basePlan := Plan{Waves: []Wave{{ID: "w1"}}, Moves: []Move{func() Move { m := baseMove; m.Wave = "w1"; return m }()}}

	incompleteCoverage := func() assess.Input {
		in := readWhole(vpcRec("111111111111", "us-east-1", "vpc-other", "10.9.0.0/16", "networks.csv", 1))
		in.Failures = []assess.FailureRow{{AccountID: "111111111111", Region: "us-east-1", Stage: assess.StageDescribe, Error: "Throttling"}}
		return in
	}()
	incompleteCoverageReport := mustAssess(t, incompleteCoverage, assess.Options{})

	tests := []struct {
		name     string
		report   assess.Report
		input    assess.Input
		evidence []DerivedEvidenceFile
	}{
		{"no evidence at all", notObservedReport, notObserved, nil},
		{"evidence scope excludes the target's tenant", notObservedReport, notObserved,
			[]DerivedEvidenceFile{{Scope: "tenant-other", Path: "e.json", Allocations: []DerivedEvidenceAllocation{{TenantID: target.TenantID, AllocationKey: target.AllocationKey, State: StateActive, VerifiedAt: strPtr("t")}}}}},
		{"evidence scope field itself missing", notObservedReport, notObserved,
			[]DerivedEvidenceFile{{Scope: "", Path: "e.json", Allocations: []DerivedEvidenceAllocation{{TenantID: target.TenantID, AllocationKey: target.AllocationKey, State: StateActive, VerifiedAt: strPtr("t")}}}}},
		{"underlying assessment coverage incomplete", incompleteCoverageReport, incompleteCoverage, []DerivedEvidenceFile{evActive(target)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := Derive(basePlan, tc.report, tc.input, tc.evidence, Options{})
			if r.Evidence.Complete {
				t.Fatalf("Evidence.Complete = true, want false")
			}
			assertIncompleteTemplateChosen(t, r)
			assertNoForbiddenPhrase(t, r)
			assertNoPercentSign(t, r)
		})
	}
}

// TestForbiddenPhraseGuardOnTheCompleteTemplateToo doubles as ADR 0015's
// third named end-to-end fixture -- "a third fixture, in which every move's
// three facts are affirmative, is the only one that exits 0" (for this
// package: the only one whose move reads FullyEvidenced true) -- built by
// hand rather than through estategen; see derive_estate_test.go's own
// closing comment for why. It is also stricter than the record's own
// literal test requirement (which scopes the forbidden-list check to
// evidence.complete == false), because this package's forbiddenPhrases doc
// comment commits to avoiding the whole list in BOTH branches -- this
// asserts that promise holds too.
func TestForbiddenPhraseGuardOnTheCompleteTemplateToo(t *testing.T) {
	target := baseTarget()
	notObserved := readWhole(vpcRec("111111111111", "us-east-1", "vpc-other", "10.9.0.0/16", "networks.csv", 1))
	notObservedReport := mustAssess(t, notObserved, assess.Options{})
	plan := Plan{Moves: []Move{{Subject: subject("111111111111", "us-east-1", "vpc-a"), Disposition: DispositionReplace, Target: target}}}
	r := Derive(plan, notObservedReport, notObserved, []DerivedEvidenceFile{evActive(target)}, Options{})
	if !r.Evidence.Complete {
		t.Fatalf("Evidence.Complete = false, want true")
	}
	if !r.Moves[0].FullyEvidenced {
		t.Fatalf("Moves[0].FullyEvidenced = false, want true: subject not-observed, target active, no claimed or unclaimed conflicts")
	}
	if r.Summary.Derived.FullyEvidenced != 1 || r.Summary.Derived.MovesTotal != 1 {
		t.Fatalf("Summary.Derived = %+v, want FullyEvidenced=1 of MovesTotal=1", r.Summary.Derived)
	}
	assertCompleteTemplateChosen(t, r)
	assertNoForbiddenPhrase(t, r)
	assertNoPercentSign(t, r)
}

// TestNoMoveEmptyPlanFollowsTheRules covers the degenerate case: a plan
// with zero moves and zero waves must still pick a template and never
// claim completeness by omission.
func TestNoMoveEmptyPlanFollowsTheRules(t *testing.T) {
	in := readWhole()
	report := mustAssess(t, in, assess.Options{})
	plan := Plan{}
	r := Derive(plan, report, in, nil, Options{})
	if r.Evidence.Complete {
		t.Fatal("zero evidence files must never read complete, even for an empty plan")
	}
	assertIncompleteTemplateChosen(t, r)
	assertNoForbiddenPhrase(t, r)
	assertNoPercentSign(t, r)
}

// TestRenderSentenceClausesAreIsolated is added after mutation testing found
// two survivors in renderSentence (report_templates.go): the missing-
// accounts/missing-region-pairs clause's OR turned into an AND, and the
// no-evidence-supplied branch inverted. Every prior test exercising the
// incomplete template happened to have both missingAccounts>0 AND
// missingRegionPairs>0 together (or neither), and to only ever check the
// template's common fixed substring rather than which specific clause was
// selected -- so a mutation that silently dropped or inverted one clause
// still produced text containing that common substring and passed.
func TestRenderSentenceClausesAreIsolated(t *testing.T) {
	t.Run("missing accounts without missing region pairs still names the account gap", func(t *testing.T) {
		// A whole-account failure (empty Region) with no rows anywhere
		// contributes to assess.Summary.IncompleteAccounts but NOT
		// IncompleteRegionPairs (internal/assess/coverage.go's own
		// incompleteAccountsAndRegionPairs only adds a region-pair when
		// Region != "").
		in := assess.Input{
			Accounts: []assess.AccountRecord{{AccountID: "111111111111", Status: "ACTIVE"}},
			Failures: []assess.FailureRow{{AccountID: "111111111111", Stage: assess.StageAssumeRole, Error: "AccessDenied"}},
			// A succeeded attempt in an unrelated region marks the account
			// attempted without adding a second, region-specific Failed entry
			// (see derive_test.go's identical fix for the same reason).
			Run: &assess.RunRecord{SourceFile: "run.json", Attempts: []assess.RunAttempt{
				{AccountID: "111111111111", Region: "eu-west-1", Outcome: assess.AttemptSucceeded},
			}},
		}
		report := mustAssess(t, in, assess.Options{})
		if report.Summary.IncompleteAccounts == 0 || report.Summary.IncompleteRegionPairs != 0 {
			t.Fatalf("test setup: IncompleteAccounts=%d IncompleteRegionPairs=%d, want >0 and exactly 0", report.Summary.IncompleteAccounts, report.Summary.IncompleteRegionPairs)
		}
		sentence := renderSentence(false, 0, 0, report.Summary.IncompleteAccounts, report.Summary.IncompleteRegionPairs, nil, true, "")
		if !strings.Contains(sentence, "the underlying assessment could not read") {
			t.Fatalf("sentence = %q, want the missing-accounts clause even though missingRegionPairs is 0", sentence)
		}
	})

	t.Run("no evidence supplied selects its own clause, not the uncovered-tenant clause", func(t *testing.T) {
		sentence := renderSentence(false, 1, 0, 0, 0, []string{"tenant-a"}, true, "")
		if !strings.Contains(sentence, "no allocation evidence was supplied at all") {
			t.Fatalf("sentence = %q, want the no-evidence-supplied clause", sentence)
		}
		if strings.Contains(sentence, "tenant-a") {
			t.Fatalf("sentence = %q, must not name a specific tenant when no evidence was supplied at all", sentence)
		}
	})

	t.Run("evidence supplied but scope-uncovered selects the tenant clause, not the no-evidence clause", func(t *testing.T) {
		sentence := renderSentence(false, 1, 0, 0, 0, []string{"tenant-a"}, false, "")
		if !strings.Contains(sentence, "no evidence could have seen 1 tenant(s)' allocations (tenant-a)") {
			t.Fatalf("sentence = %q, want the uncovered-tenant clause naming tenant-a", sentence)
		}
		if strings.Contains(sentence, "no allocation evidence was supplied at all") {
			t.Fatalf("sentence = %q, must not claim no evidence was supplied when some was", sentence)
		}
	})
}

// --- determinism ---

func TestDeterminismAcrossRuns(t *testing.T) {
	target := baseTarget()
	in := readWhole(vpcRec("111111111111", "us-east-1", "vpc-other", "10.9.0.0/16", "networks.csv", 1))
	report := mustAssess(t, in, assess.Options{})
	plan := Plan{Waves: []Wave{{ID: "w1"}}, Moves: []Move{{Subject: subject("111111111111", "us-east-1", "vpc-a"), Disposition: DispositionReplace, Wave: "w1", Target: target}}}

	r1 := Derive(plan, report, in, []DerivedEvidenceFile{evActive(target)}, Options{Stamp: "s"})
	r2 := Derive(plan, report, in, []DerivedEvidenceFile{evActive(target)}, Options{Stamp: "s"})
	j1, t1 := renderBoth(t, r1)
	j2, t2 := renderBoth(t, r2)
	if j1 != j2 {
		t.Fatalf("JSON differs across identical runs")
	}
	if t1 != t2 {
		t.Fatalf("text differs across identical runs")
	}
}

// TestDeterminismAcrossMoveAndWaveOrder is part of "determinism across the
// order of moves, files, evidence files and assessment inputs."
func TestDeterminismAcrossMoveAndWaveOrder(t *testing.T) {
	target := baseTarget()
	target2 := &Target{TenantID: "tenant-b", AllocationKey: "vpc-b", Scope: "vpc", Environment: "prod", Region: "us-east-1", AccountID: "222222222222", PrefixLength: intPtr(16)}
	in := readWhole(
		vpcRec("111111111111", "us-east-1", "vpc-other", "10.9.0.0/16", "networks.csv", 1),
		vpcRec("222222222222", "us-east-1", "vpc-other2", "10.8.0.0/16", "networks.csv", 2),
	)
	report := mustAssess(t, in, assess.Options{})

	m1 := Move{Subject: subject("111111111111", "us-east-1", "vpc-a"), Disposition: DispositionReplace, Wave: "w1", Target: target}
	m2 := Move{Subject: subject("222222222222", "us-east-1", "vpc-b"), Disposition: DispositionReplace, Wave: "w2", Target: target2}
	w1 := Wave{ID: "w1"}
	w2 := Wave{ID: "w2"}
	evidence := []DerivedEvidenceFile{evActive(target), evActive(target2)}

	planForward := Plan{Waves: []Wave{w1, w2}, Moves: []Move{m1, m2}}
	planReversed := Plan{Waves: []Wave{w2, w1}, Moves: []Move{m2, m1}}
	evidenceReversed := []DerivedEvidenceFile{evidence[1], evidence[0]}

	r1 := Derive(planForward, report, in, evidence, Options{})
	r2 := Derive(planReversed, report, in, evidenceReversed, Options{})
	j1, t1 := renderBoth(t, r1)
	j2, t2 := renderBoth(t, r2)
	if j1 != j2 {
		t.Fatalf("JSON differs across move/wave/evidence order:\n--- forward ---\n%s\n--- reversed ---\n%s", j1, j2)
	}
	if t1 != t2 {
		t.Fatalf("text differs across move/wave/evidence order")
	}
}

// --- the embedded assessment matches onboard assess's own output ---

func TestEmbeddedAssessmentEqualsWhatAssessProduces(t *testing.T) {
	in := readWhole(
		vpcRec("111111111111", "us-east-1", "vpc-a", "10.0.0.0/16", "networks.csv", 1),
		vpcRec("222222222222", "us-east-1", "vpc-b", "10.0.0.0/16", "networks.csv", 2),
	)
	report := mustAssess(t, in, assess.Options{})
	plan := Plan{Moves: []Move{{Subject: subject("111111111111", "us-east-1", "vpc-a")}}}
	r := Derive(plan, report, in, nil, Options{})

	if r.Assessment.Coverage.Complete != report.Coverage.Complete {
		t.Fatalf("embedded coverage.complete = %v, want %v", r.Assessment.Coverage.Complete, report.Coverage.Complete)
	}
	if r.Assessment.Summary.TotalRelationships != report.Summary.TotalRelationships {
		t.Fatalf("embedded summary.total_relationships = %d, want %d", r.Assessment.Summary.TotalRelationships, report.Summary.TotalRelationships)
	}
	if r.Assessment.Summary.Sentence != report.Summary.Sentence {
		t.Fatalf("embedded summary.sentence = %q, want %q", r.Assessment.Summary.Sentence, report.Summary.Sentence)
	}
}

// --- two encoders carry the same facts ---

func TestBothEncodersCarryTheSameFacts(t *testing.T) {
	target := baseTarget()
	in := readWhole(vpcRec("111111111111", "us-east-1", "vpc-other", "10.9.0.0/16", "networks.csv", 1))
	report := mustAssess(t, in, assess.Options{})
	plan := Plan{Moves: []Move{{Subject: subject("111111111111", "us-east-1", "vpc-a"), Disposition: DispositionReplace, Target: target}}}
	r := Derive(plan, report, in, []DerivedEvidenceFile{evActive(target)}, Options{})

	jsonText, textText := renderBoth(t, r)
	if !strings.Contains(jsonText, string(r.Moves[0].SubjectFact)) {
		t.Errorf("JSON does not carry the subject fact %q", r.Moves[0].SubjectFact)
	}
	if !strings.Contains(textText, string(r.Moves[0].SubjectFact)) {
		t.Errorf("text does not carry the subject fact %q", r.Moves[0].SubjectFact)
	}
	if !strings.Contains(jsonText, string(r.Moves[0].TargetFact)) || !strings.Contains(textText, string(r.Moves[0].TargetFact)) {
		t.Errorf("one encoder is missing the target fact %q", r.Moves[0].TargetFact)
	}
}
