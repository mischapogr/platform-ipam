package migrate

import (
	"testing"

	"github.com/mischapogr/platform-ipam/internal/assess"
)

func baseTarget() *Target {
	return &Target{
		TenantID: "tenant-a", AllocationKey: "vpc-a", Scope: "vpc", Environment: "prod",
		Region: "us-east-1", AccountID: "111111111111", PrefixLength: intPtr(16),
	}
}

func evActive(target *Target) DerivedEvidenceFile {
	return DerivedEvidenceFile{Scope: target.TenantID, ReadAt: "2026-09-20T12:00:00Z", Path: "evidence.json", Allocations: []DerivedEvidenceAllocation{
		{TenantID: target.TenantID, AllocationKey: target.AllocationKey, Scope: target.Scope, Environment: target.Environment,
			Region: target.Region, AccountID: target.AccountID, PrefixLength: *target.PrefixLength,
			State: StateActive, VerifiedAt: strPtr("2026-09-20T11:00:00Z")},
	}}
}

// --- SubjectFact ---

// TestSubjectFactNotObservedUnderCompleteCoverageAgainstUnknownUnderIncomplete
// is ADR 0015's own headline pair: "not-observed under complete coverage
// against unknown under incomplete coverage for the same missing subject --
// the pair that matters most."
func TestSubjectFactNotObservedUnderCompleteCoverageAgainstUnknownUnderIncomplete(t *testing.T) {
	// A different VPC in the same account/region is observed, so coverage is
	// otherwise complete for that pair, but the subject we ask about never
	// appears -- complete coverage, not-observed.
	in := readWhole(vpcRec("111111111111", "us-east-1", "vpc-other", "10.9.0.0/16", "networks.csv", 1))
	report := mustAssess(t, in, assess.Options{})
	sFact, _, unmatched := subjectFact(subject("111111111111", "us-east-1", "vpc-a"), in, report.Coverage, knownSubjectSet(in), knownAccountSet(in))
	if sFact != SubjectNotObserved || unmatched {
		t.Fatalf("subject = %s, unmatched = %v; want not-observed, false", sFact, unmatched)
	}

	// Now the same account/region has an incomplete read (a failures.csv
	// row), and the identical missing subject must read unknown instead.
	in2 := readWhole(vpcRec("111111111111", "us-east-1", "vpc-other", "10.9.0.0/16", "networks.csv", 1))
	in2.Failures = []assess.FailureRow{{AccountID: "111111111111", Region: "us-east-1", Stage: assess.StageDescribe, Error: "Throttling"}}
	report2 := mustAssess(t, in2, assess.Options{})
	sFact2, reason2, unmatched2 := subjectFact(subject("111111111111", "us-east-1", "vpc-a"), in2, report2.Coverage, knownSubjectSet(in2), knownAccountSet(in2))
	if sFact2 != SubjectUnknown || unmatched2 {
		t.Fatalf("subject = %s, unmatched = %v; want unknown, false", sFact2, unmatched2)
	}
	if reason2 == "" {
		t.Fatal("subject fact unknown but no coverage entry named")
	}
}

func TestSubjectFactObservedRegardlessOfCoverage(t *testing.T) {
	in := readWhole(vpcRec("111111111111", "us-east-1", "vpc-a", "10.0.0.0/16", "networks.csv", 1))
	in.Run = nil // coverage cannot be complete at all
	report := mustAssess(t, in, assess.Options{})
	sFact, _, unmatched := subjectFact(subject("111111111111", "us-east-1", "vpc-a"), in, report.Coverage, knownSubjectSet(in), knownAccountSet(in))
	if sFact != SubjectObserved || unmatched {
		t.Fatalf("a positively observed subject must read observed regardless of coverage elsewhere: got %s, unmatched=%v", sFact, unmatched)
	}
}

// TestSubjectFactUnmatchedWhenAccountUnknown is the fourth answer ADR 0015
// names: "A subject that appears in no assessment at all ... is unmatched."
// An account the collector never even listed cannot honestly be
// `not-observed` (which claims the VPC used to exist and is now gone), so
// this package reads it as Unknown+Unmatched rather than guessing.
func TestSubjectFactUnmatchedWhenAccountUnknown(t *testing.T) {
	in := readWhole(vpcRec("111111111111", "us-east-1", "vpc-other", "10.9.0.0/16", "networks.csv", 1))
	report := mustAssess(t, in, assess.Options{})
	sFact, reason, unmatched := subjectFact(subject("222222222222", "us-east-1", "vpc-typo"), in, report.Coverage, knownSubjectSet(in), knownAccountSet(in))
	if sFact != SubjectUnknown || !unmatched {
		t.Fatalf("subject = %s, unmatched = %v; want unknown, true", sFact, unmatched)
	}
	if reason == "" {
		t.Fatal("unmatched subject carries no reason")
	}
}

// TestCoverageIncompleteForRoutesIsolated exercises each of
// coverageIncompleteFor's five branches (derive_facts.go) in isolation from
// the others, so a mutation collapsing one branch into another (found by
// this package's own mutation pass) cannot hide behind a second branch that
// happens to also fire. Added after mutation testing found four survivors:
// dropping the RunMissing branch, dropping the AccountsMissing branch, the
// Failed-entry region match turning OR into AND, and the RowCountShort
// match turning AND into OR.
func TestCoverageIncompleteForRoutesIsolated(t *testing.T) {
	t.Run("RunMissing alone (no other coverage signal)", func(t *testing.T) {
		in := assess.Input{
			Records:  []assess.ResourceRecord{vpcRec("111111111111", "us-east-1", "vpc-other", "10.9.0.0/16", "networks.csv", 1)},
			Accounts: []assess.AccountRecord{{AccountID: "111111111111", Status: "ACTIVE"}},
			// Run is nil: no other branch (Failed/Partial/NotAttempted/RowCountShort)
			// can fire, since they all read from coverage lists computeCoverage
			// only populates when interpreting run.Attempts or failures.csv, and
			// neither is supplied here.
		}
		report := mustAssess(t, in, assess.Options{})
		if !report.Coverage.RunMissing {
			t.Fatalf("test setup: coverage.RunMissing = false, want true")
		}
		incomplete, reason := coverageIncompleteFor(report.Coverage, "111111111111", "us-east-1")
		if !incomplete || reason == "" {
			t.Fatalf("coverageIncompleteFor = (%v, %q), want (true, non-empty) on RunMissing alone", incomplete, reason)
		}
	})

	t.Run("AccountsMissing alone (no other coverage signal)", func(t *testing.T) {
		in := assess.Input{
			Records: []assess.ResourceRecord{vpcRec("111111111111", "us-east-1", "vpc-other", "10.9.0.0/16", "networks.csv", 1)},
			Run: &assess.RunRecord{SourceFile: "run.json", Attempts: []assess.RunAttempt{
				{AccountID: "111111111111", Region: "us-east-1", Outcome: assess.AttemptSucceeded},
			}},
			// Accounts is nil: AccountsMissing alone must trigger incompleteness.
		}
		report := mustAssess(t, in, assess.Options{})
		if !report.Coverage.AccountsMissing {
			t.Fatalf("test setup: coverage.AccountsMissing = false, want true")
		}
		incomplete, reason := coverageIncompleteFor(report.Coverage, "111111111111", "us-east-1")
		if !incomplete || reason == "" {
			t.Fatalf("coverageIncompleteFor = (%v, %q), want (true, non-empty) on AccountsMissing alone", incomplete, reason)
		}
	})

	// A whole-account failure (empty Region, e.g. assume-role) with NO rows
	// anywhere for that account is caught ONLY by the Failed branch's
	// f.Region == "" arm: accountRegionHasRows is empty, so the Partial
	// branch never fires, and the account is marked attempted (it has a
	// failed outcome), so NotAttempted never fires either.
	t.Run("whole-account Failed entry alone, isolated from Partial and NotAttempted", func(t *testing.T) {
		in := assess.Input{
			Accounts: []assess.AccountRecord{{AccountID: "111111111111", Status: "ACTIVE"}},
			Failures: []assess.FailureRow{{AccountID: "111111111111", Stage: assess.StageAssumeRole, Error: "AccessDenied", SourceFile: "failures.csv", SourceRow: 1}},
			// A succeeded attempt in an UNRELATED region (eu-west-1, not the
			// us-east-1 this test queries) marks the account "attempted" so
			// NotAttempted stays empty, without adding a second, region-specific
			// Failed entry the way an AttemptFailed run.Attempts row would
			// (computeCoverage's default branch synthesizes one whenever an
			// attempt's own outcome is not already recorded in failures.csv by
			// exactly that account+region pair).
			Run: &assess.RunRecord{SourceFile: "run.json", Attempts: []assess.RunAttempt{
				{AccountID: "111111111111", Region: "eu-west-1", Outcome: assess.AttemptSucceeded},
			}},
		}
		report := mustAssess(t, in, assess.Options{})
		if len(report.Coverage.Partial) != 0 || len(report.Coverage.NotAttempted) != 0 {
			t.Fatalf("test setup: Partial = %v, NotAttempted = %v; want both empty so only Failed is exercised", report.Coverage.Partial, report.Coverage.NotAttempted)
		}
		if len(report.Coverage.Failed) != 1 || report.Coverage.Failed[0].Region != "" {
			t.Fatalf("test setup: Failed = %+v, want exactly one whole-account (empty-region) entry", report.Coverage.Failed)
		}
		incomplete, reason := coverageIncompleteFor(report.Coverage, "111111111111", "us-east-1")
		if !incomplete || reason == "" {
			t.Fatalf("coverageIncompleteFor = (%v, %q), want (true, non-empty) on a whole-account Failed entry alone", incomplete, reason)
		}
	})

	// A RowCountShort entry for one region of an account must not make an
	// UNRELATED, fully-read region of the SAME account read incomplete.
	t.Run("RowCountShort is scoped to its own account AND region, not account alone", func(t *testing.T) {
		recorded := 5
		in := assess.Input{
			Records:  []assess.ResourceRecord{vpcRec("111111111111", "us-east-1", "vpc-x", "10.0.0.0/16", "networks.csv", 1)},
			Accounts: []assess.AccountRecord{{AccountID: "111111111111", Status: "ACTIVE"}},
			Run: &assess.RunRecord{SourceFile: "run.json", Attempts: []assess.RunAttempt{
				{AccountID: "111111111111", Region: "us-east-1", Outcome: assess.AttemptSucceeded, RowCount: intPtr(1)},
				// eu-west-1 has zero rows present but claims 5 were recorded: a
				// RowCountShort entry for (111111111111, eu-west-1) only.
				{AccountID: "111111111111", Region: "eu-west-1", Outcome: assess.AttemptSucceeded, RowCount: &recorded},
			}},
		}
		report := mustAssess(t, in, assess.Options{})
		if len(report.Coverage.RowCountShort) != 1 || report.Coverage.RowCountShort[0].Region != "eu-west-1" {
			t.Fatalf("test setup: RowCountShort = %+v, want exactly one entry for eu-west-1", report.Coverage.RowCountShort)
		}
		// The query is for us-east-1 -- a different, fully-read region of the
		// SAME account -- and must read complete.
		incomplete, _ := coverageIncompleteFor(report.Coverage, "111111111111", "us-east-1")
		if incomplete {
			t.Fatal("coverageIncompleteFor = true for us-east-1, want false: the RowCountShort gap is in eu-west-1, a different region of the same account")
		}
		incompleteEU, _ := coverageIncompleteFor(report.Coverage, "111111111111", "eu-west-1")
		if !incompleteEU {
			t.Fatal("coverageIncompleteFor = false for eu-west-1, want true: that is the region with the RowCountShort gap")
		}
	})
}

func intPtr(n int) *int { return &n }

// TestFullyEvidencedFalseWhenAClaimedIDIsPresentEvenIfSubjectHasNoTouchingConflict
// isolates fullyEvidenced's per-resolve loop from its subject/target/
// unclaimed guard clauses (mutation testing found that inverting
// `r.Status != ResolveStale` survived every other test, because every
// existing "claimed conflict still present" fixture also had subject
// SubjectObserved, which already short-circuits fullyEvidenced to false
// before the resolves loop runs at all). A move can claim a conflict id
// that is CURRENTLY present between two entirely different VPCs -- a
// reviewer's copy-paste mistake -- while its own subject is genuinely
// not-observed; that id must still read Present, not Stale, and must still
// block FullyEvidenced.
func TestFullyEvidencedFalseWhenAClaimedIDIsPresentEvenIfSubjectHasNoTouchingConflict(t *testing.T) {
	x := vpcRec("222222222222", "us-east-1", "vpc-x", "10.5.0.0/16", "networks.csv", 1)
	y := vpcRec("333333333333", "us-east-1", "vpc-y", "10.5.0.0/16", "networks.csv", 2)
	// vpc-other keeps account 111111111111/us-east-1 coverage complete so
	// the missing subject reads not-observed rather than unknown.
	other := vpcRec("111111111111", "us-east-1", "vpc-other", "10.9.0.0/16", "networks.csv", 3)
	in := readWhole(x, y, other)
	report := mustAssess(t, in, assess.Options{})
	if len(report.Conflicts) != 1 {
		t.Fatalf("test setup: conflicts = %+v, want exactly one (between vpc-x and vpc-y)", report.Conflicts)
	}
	unrelatedID := report.Conflicts[0].ID

	target := baseTarget()
	m := Move{
		Subject:     subject("111111111111", "us-east-1", "vpc-a"), // not present anywhere in in.Records
		Disposition: DispositionReplace, Target: target,
		Resolves: []string{unrelatedID}, // mistakenly names an unrelated, currently-present conflict
	}
	plan := Plan{Moves: []Move{m}}
	rep := Derive(plan, report, in, []DerivedEvidenceFile{evActive(target)}, Options{})

	got := rep.Moves[0]
	if got.SubjectFact != SubjectNotObserved {
		t.Fatalf("test setup: subject fact = %s, want not-observed", got.SubjectFact)
	}
	if len(got.Resolves) != 1 || got.Resolves[0].Status != ResolvePresent {
		t.Fatalf("resolves = %+v, want [%s present]", got.Resolves, unrelatedID)
	}
	if got.FullyEvidenced {
		t.Fatal("FullyEvidenced = true, want false: the move's own claimed conflict id is currently present, not stale")
	}
}

// --- TargetFact ---

func TestTargetFactRoutes(t *testing.T) {
	target := baseTarget()

	t.Run("unknown: no evidence at all", func(t *testing.T) {
		idx := buildEvidenceIndex(nil)
		fact, _ := targetFact(Move{Disposition: DispositionReplace, Target: target}, idx)
		if fact != TargetUnknown {
			t.Fatalf("got %s, want unknown", fact)
		}
	})

	t.Run("unknown: evidence scope excludes the target's tenant", func(t *testing.T) {
		idx := buildEvidenceIndex([]DerivedEvidenceFile{{Scope: "tenant-other", Path: "e.json", Allocations: []DerivedEvidenceAllocation{
			{TenantID: target.TenantID, AllocationKey: target.AllocationKey, State: StateActive},
		}}})
		fact, _ := targetFact(Move{Disposition: DispositionReplace, Target: target}, idx)
		if fact != TargetUnknown {
			t.Fatalf("got %s, want unknown (scope excludes tenant, NEVER none)", fact)
		}
	})

	t.Run("none: tenant covered, key absent", func(t *testing.T) {
		idx := buildEvidenceIndex([]DerivedEvidenceFile{{Scope: target.TenantID, Path: "e.json"}})
		fact, _ := targetFact(Move{Disposition: DispositionReplace, Target: target}, idx)
		if fact != TargetNone {
			t.Fatalf("got %s, want none", fact)
		}
	})

	t.Run("reserved: allocation exists, fields match, not verified", func(t *testing.T) {
		f := evActive(target)
		f.Allocations[0].State = StateReserved
		f.Allocations[0].VerifiedAt = nil
		idx := buildEvidenceIndex([]DerivedEvidenceFile{f})
		fact, _ := targetFact(Move{Disposition: DispositionReplace, Target: target}, idx)
		if fact != TargetReserved {
			t.Fatalf("got %s, want reserved", fact)
		}
	})

	t.Run("active: state ACTIVE with verified_at", func(t *testing.T) {
		idx := buildEvidenceIndex([]DerivedEvidenceFile{evActive(target)})
		fact, _ := targetFact(Move{Disposition: DispositionReplace, Target: target}, idx)
		if fact != TargetActive {
			t.Fatalf("got %s, want active", fact)
		}
	})

	t.Run("retired: QUARANTINED", func(t *testing.T) {
		f := evActive(target)
		f.Allocations[0].State = StateQuarantined
		idx := buildEvidenceIndex([]DerivedEvidenceFile{f})
		fact, _ := targetFact(Move{Disposition: DispositionReplace, Target: target}, idx)
		if fact != TargetRetired {
			t.Fatalf("got %s, want retired (QUARANTINED)", fact)
		}
	})

	t.Run("retired: RELEASED", func(t *testing.T) {
		f := evActive(target)
		f.Allocations[0].State = StateReleased
		idx := buildEvidenceIndex([]DerivedEvidenceFile{f})
		fact, _ := targetFact(Move{Disposition: DispositionReplace, Target: target}, idx)
		if fact != TargetRetired {
			t.Fatalf("got %s, want retired (RELEASED)", fact)
		}
	})

	t.Run("mismatched: prefix length differs", func(t *testing.T) {
		f := evActive(target)
		f.Allocations[0].PrefixLength = 24
		idx := buildEvidenceIndex([]DerivedEvidenceFile{f})
		fact, fields := targetFact(Move{Disposition: DispositionReplace, Target: target}, idx)
		if fact != TargetMismatched || len(fields) != 1 || fields[0] != "prefix_length" {
			t.Fatalf("got %s %v, want mismatched [prefix_length]", fact, fields)
		}
	})

	t.Run("mismatched: environment differs", func(t *testing.T) {
		f := evActive(target)
		f.Allocations[0].Environment = "staging"
		idx := buildEvidenceIndex([]DerivedEvidenceFile{f})
		fact, fields := targetFact(Move{Disposition: DispositionReplace, Target: target}, idx)
		if fact != TargetMismatched || len(fields) != 1 || fields[0] != "environment" {
			t.Fatalf("got %s %v, want mismatched [environment]", fact, fields)
		}
	})

	t.Run("keep move never mismatched: only tenant_id and allocation_key are typed", func(t *testing.T) {
		keepTarget := &Target{TenantID: target.TenantID, AllocationKey: target.AllocationKey}
		f := evActive(target) // evidence carries fields keepTarget never typed
		idx := buildEvidenceIndex([]DerivedEvidenceFile{f})
		fact, fields := targetFact(Move{Disposition: DispositionKeep, Target: keepTarget}, idx)
		if fact != TargetActive || len(fields) != 0 {
			t.Fatalf("got %s %v, want active with no mismatch fields", fact, fields)
		}
	})

	t.Run("operator scope covers every tenant", func(t *testing.T) {
		f := evActive(target)
		f.Scope = "operator"
		idx := buildEvidenceIndex([]DerivedEvidenceFile{f})
		fact, _ := targetFact(Move{Disposition: DispositionReplace, Target: target}, idx)
		if fact != TargetActive {
			t.Fatalf("got %s, want active under an operator-scoped export", fact)
		}
	})

	t.Run("none: retire and undecided moves carry no target", func(t *testing.T) {
		idx := buildEvidenceIndex(nil)
		for _, d := range []Disposition{DispositionRetire, DispositionUndecided} {
			fact, _ := targetFact(Move{Disposition: d, Target: nil}, idx)
			if fact != TargetNone {
				t.Fatalf("disposition %s: got %s, want none", d, fact)
			}
		}
	})
}

// --- Conflicts: resolved present/stale, and unclaimed ---

func TestConflictsPresentAgainstStale(t *testing.T) {
	a := vpcRec("111111111111", "us-east-1", "vpc-a", "10.0.0.0/16", "networks.csv", 1)
	b := vpcRec("222222222222", "us-east-1", "vpc-b", "10.0.0.0/16", "networks.csv", 2)
	report := mustAssess(t, readWhole(a, b), assess.Options{})
	if len(report.Conflicts) != 1 {
		t.Fatalf("baseline conflicts = %+v, want 1", report.Conflicts)
	}
	realID := report.Conflicts[0].ID
	staleID := "c-0000000000000000"

	m := Move{Subject: subject("111111111111", "us-east-1", "vpc-a"), Resolves: []string{realID, staleID}}
	touching := touchingConflicts(report.Conflicts)
	resolves, unclaimed := moveConflictResults(m, touching, conflictIDSet(report.Conflicts))
	if len(unclaimed) != 0 {
		t.Fatalf("unclaimed = %v, want none (both ids were claimed)", unclaimed)
	}
	byID := map[string]ResolveStatus{}
	for _, r := range resolves {
		byID[r.ConflictID] = r.Status
	}
	if byID[realID] != ResolvePresent {
		t.Errorf("real id status = %s, want present", byID[realID])
	}
	if byID[staleID] != ResolveStale {
		t.Errorf("removed id status = %s, want stale", byID[staleID])
	}
}

func TestUnclaimedConflictsOnASubjectNoMoveResolves(t *testing.T) {
	a := vpcRec("111111111111", "us-east-1", "vpc-a", "10.0.0.0/16", "networks.csv", 1)
	b := vpcRec("222222222222", "us-east-1", "vpc-b", "10.0.0.0/16", "networks.csv", 2)
	report := mustAssess(t, readWhole(a, b), assess.Options{})
	touching := touchingConflicts(report.Conflicts)
	m := Move{Subject: subject("111111111111", "us-east-1", "vpc-a")} // resolves nothing
	resolves, unclaimed := moveConflictResults(m, touching, conflictIDSet(report.Conflicts))
	if len(resolves) != 0 {
		t.Fatalf("resolves = %v, want none", resolves)
	}
	if len(unclaimed) != 1 || unclaimed[0] != report.Conflicts[0].ID {
		t.Fatalf("unclaimed = %v, want [%s]", unclaimed, report.Conflicts[0].ID)
	}
}

// --- unplanned ---

func TestUnplannedConflictOnASubjectNoMoveMentions(t *testing.T) {
	a := vpcRec("111111111111", "us-east-1", "vpc-a", "10.0.0.0/16", "networks.csv", 1)
	b := vpcRec("222222222222", "us-east-1", "vpc-b", "10.0.0.0/16", "networks.csv", 2)
	report := mustAssess(t, readWhole(a, b), assess.Options{})
	// Neither vpc-a nor vpc-b is named by any move.
	unplanned := unplannedConflictIDs(report.Conflicts, nil)
	if len(unplanned) != 1 || unplanned[0] != report.Conflicts[0].ID {
		t.Fatalf("unplanned = %v, want [%s]", unplanned, report.Conflicts[0].ID)
	}

	// Once one side is named by a move, this package's reading is that the
	// conflict is no longer globally unplanned (it is `unclaimed` for that
	// move instead -- TestUnclaimedConflictsOnASubjectNoMoveResolves above).
	planned := []Move{{Subject: subject("111111111111", "us-east-1", "vpc-a")}}
	unplanned2 := unplannedConflictIDs(report.Conflicts, planned)
	if len(unplanned2) != 0 {
		t.Fatalf("unplanned = %v, want none once one side has a move", unplanned2)
	}
}

// --- keep-move-not-adoptable ---

func TestKeepMoveBlockedWhileAConflictIsStillReported(t *testing.T) {
	a := vpcRec("111111111111", "us-east-1", "vpc-a", "10.0.0.0/16", "networks.csv", 1)
	b := vpcRec("222222222222", "us-east-1", "vpc-b", "10.0.0.0/16", "networks.csv", 2)
	report := mustAssess(t, readWhole(a, b), assess.Options{})
	input := readWhole(a, b)

	keepTarget := &Target{TenantID: "tenant-a", AllocationKey: "vpc-a"}
	plan := Plan{Moves: []Move{{Subject: subject("111111111111", "us-east-1", "vpc-a"), Disposition: DispositionKeep, Target: keepTarget}}}
	rep := Derive(plan, report, input, nil, Options{})
	if !rep.Moves[0].KeepBlockedByConflict {
		t.Fatal("keep move with a still-reported conflict must be blocked")
	}

	// Once the conflicting side is gone the same move is not blocked.
	aloneReport := mustAssess(t, readWhole(a), assess.Options{})
	aloneInput := readWhole(a)
	rep2 := Derive(plan, aloneReport, aloneInput, nil, Options{})
	if rep2.Moves[0].KeepBlockedByConflict {
		t.Fatal("keep move with no conflict left must not be blocked")
	}
}

// --- head property 1: no move fully evidenced on typed input alone ---

// TestNoMoveIsFullyEvidencedOnTypedInputAlone is ADR 0015's headline
// property: "A table-driven test over every route to 'not fully evidenced'
// -- no allocation evidence at all; evidence whose scope excludes the
// target's tenant; an allocation that exists but is RESERVED; an ACTIVE
// allocation whose binding carries no verified_at; a subject whose account
// and region lie in a coverage gap; a claimed conflict that is still
// present; an unclaimed conflict on the subject -- asserts that the move
// is not counted as fully evidenced."
func TestNoMoveIsFullyEvidencedOnTypedInputAlone(t *testing.T) {
	target := baseTarget()
	notObservedInput := readWhole(vpcRec("111111111111", "us-east-1", "vpc-other", "10.9.0.0/16", "networks.csv", 1))
	notObservedReport := mustAssess(t, notObservedInput, assess.Options{})

	baseMove := func() Move {
		return Move{Subject: subject("111111111111", "us-east-1", "vpc-a"), Disposition: DispositionReplace, Target: target}
	}

	tests := []struct {
		name     string
		evidence []DerivedEvidenceFile
		input    assess.Input
		report   assess.Report
		move     func() Move
	}{
		{name: "no allocation evidence at all", evidence: nil, input: notObservedInput, report: notObservedReport, move: baseMove},
		{name: "evidence scope excludes the target's tenant",
			evidence: []DerivedEvidenceFile{{Scope: "tenant-other", Path: "e.json", Allocations: []DerivedEvidenceAllocation{{TenantID: target.TenantID, AllocationKey: target.AllocationKey, State: StateActive, VerifiedAt: strPtr("t")}}}},
			input:    notObservedInput, report: notObservedReport, move: baseMove},
		{name: "allocation exists but is RESERVED",
			evidence: func() []DerivedEvidenceFile {
				f := evActive(target)
				f.Allocations[0].State = StateReserved
				f.Allocations[0].VerifiedAt = nil
				return []DerivedEvidenceFile{f}
			}(),
			input: notObservedInput, report: notObservedReport, move: baseMove},
		{name: "ACTIVE allocation with no verified_at",
			evidence: func() []DerivedEvidenceFile {
				f := evActive(target)
				f.Allocations[0].VerifiedAt = nil
				return []DerivedEvidenceFile{f}
			}(),
			input: notObservedInput, report: notObservedReport, move: baseMove},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan := Plan{Moves: []Move{tc.move()}}
			rep := Derive(plan, tc.report, tc.input, tc.evidence, Options{})
			if rep.Moves[0].FullyEvidenced {
				t.Fatalf("move reported fully evidenced: %+v", rep.Moves[0])
			}
		})
	}

	// A subject whose account and region lie in a coverage gap.
	t.Run("subject in a coverage gap", func(t *testing.T) {
		in := readWhole(vpcRec("111111111111", "us-east-1", "vpc-other", "10.9.0.0/16", "networks.csv", 1))
		in.Failures = []assess.FailureRow{{AccountID: "111111111111", Region: "us-east-1", Stage: assess.StageDescribe, Error: "Throttling"}}
		report := mustAssess(t, in, assess.Options{})
		plan := Plan{Moves: []Move{baseMove()}}
		rep := Derive(plan, report, in, []DerivedEvidenceFile{evActive(target)}, Options{})
		if rep.Moves[0].FullyEvidenced {
			t.Fatal("a subject in a coverage gap must not be fully evidenced")
		}
	})

	// A claimed conflict still present, and an unclaimed conflict.
	t.Run("claimed conflict still present", func(t *testing.T) {
		b := vpcRec("222222222222", "us-east-1", "vpc-b", "10.0.0.0/16", "networks.csv", 2)
		conflictInput := readWhole(vpcRec("111111111111", "us-east-1", "vpc-a", "10.0.0.0/16", "networks.csv", 1), b)
		report := mustAssess(t, conflictInput, assess.Options{})
		m := baseMove()
		m.Subject = subject("111111111111", "us-east-1", "vpc-a")
		m.Resolves = []string{report.Conflicts[0].ID}
		plan := Plan{Moves: []Move{m}}
		rep := Derive(plan, report, conflictInput, []DerivedEvidenceFile{evActive(target)}, Options{})
		if rep.Moves[0].FullyEvidenced {
			t.Fatal("a claimed conflict still present must not be fully evidenced")
		}
	})
	t.Run("unclaimed conflict", func(t *testing.T) {
		b := vpcRec("222222222222", "us-east-1", "vpc-b", "10.0.0.0/16", "networks.csv", 2)
		conflictInput := readWhole(vpcRec("111111111111", "us-east-1", "vpc-a", "10.0.0.0/16", "networks.csv", 1), b)
		report := mustAssess(t, conflictInput, assess.Options{})
		m := baseMove()
		m.Subject = subject("111111111111", "us-east-1", "vpc-a")
		// Resolves nothing, so the reported conflict is unclaimed.
		plan := Plan{Moves: []Move{m}}
		rep := Derive(plan, report, conflictInput, []DerivedEvidenceFile{evActive(target)}, Options{})
		if rep.Moves[0].FullyEvidenced {
			t.Fatal("an unclaimed conflict must not be fully evidenced")
		}
	})
}

// TestMutatingTypedFieldsNeverChangesADerivedFact is the property beside the
// head property: "a mutation that flips any typed field -- an approval, a
// verification outcome, a blocker's text -- must change no derived fact,
// which is the assertion that typed input cannot lie."
func TestMutatingTypedFieldsNeverChangesADerivedFact(t *testing.T) {
	b := vpcRec("222222222222", "us-east-1", "vpc-b", "10.0.0.0/16", "networks.csv", 2)
	in := readWhole(vpcRec("111111111111", "us-east-1", "vpc-a", "10.0.0.0/16", "networks.csv", 1), b)
	report := mustAssess(t, in, assess.Options{})

	base := Move{
		Subject: subject("111111111111", "us-east-1", "vpc-a"), Disposition: DispositionReplace, Target: baseTarget(),
		Owner: "team-a", Approval: &Approval{ApprovedBy: "alice", ApprovedAt: "2026-09-20", Approves: "go"},
		Blockers:     []Blocker{{ID: "b1", Description: "waiting on DNS"}},
		Verification: []Verification{{ID: "v1", Description: "ping test", Outcome: "passed"}},
		Rollback:     "revert DNS within 1h", Notes: "customer facing",
		Resolves: []string{report.Conflicts[0].ID},
	}
	basePlan := Plan{Moves: []Move{base}}
	baseReport := Derive(basePlan, report, in, []DerivedEvidenceFile{evActive(baseTarget())}, Options{})

	mutations := []func(Move) Move{
		func(m Move) Move { m.Owner = "someone-else"; return m },
		func(m Move) Move {
			m.Approval = &Approval{ApprovedBy: "bob", ApprovedAt: "2099-01-01", Approves: "no"}
			return m
		},
		func(m Move) Move { m.Approval = nil; return m },
		func(m Move) Move {
			m.Verification = []Verification{{ID: "v2", Outcome: "failed"}}
			return m
		},
		func(m Move) Move {
			m.Blockers = []Blocker{{ID: "b2", Description: "different text entirely"}}
			return m
		},
		func(m Move) Move { m.Rollback = "a completely different plan"; return m },
		func(m Move) Move { m.Notes = "anything at all"; return m },
	}
	for i, mutate := range mutations {
		mutated := mutate(base)
		plan := Plan{Moves: []Move{mutated}}
		rep := Derive(plan, report, in, []DerivedEvidenceFile{evActive(baseTarget())}, Options{})
		got, want := rep.Moves[0], baseReport.Moves[0]
		if got.SubjectFact != want.SubjectFact || got.TargetFact != want.TargetFact ||
			got.FullyEvidenced != want.FullyEvidenced || len(got.Resolves) != len(want.Resolves) ||
			len(got.Unclaimed) != len(want.Unclaimed) {
			t.Fatalf("mutation %d changed a derived fact: got %+v, want facts matching %+v", i, got, want)
		}
	}
}
