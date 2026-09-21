package migrate

import (
	"encoding/csv"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mischapogr/platform-ipam/internal/assess"
	"github.com/mischapogr/platform-ipam/internal/assess/estategen"
)

// This file is the end-to-end fixture ADR 0015's evidence paragraph
// requires: "from internal/assess/estategen's generated estate. The gapped
// estate ... gains a synthetic plan over its planted conflicts, and the run
// must reproduce every planted fact." It imports
// internal/assess/estategen, which derive.go's own package doc explains is
// permitted in TEST files only (see derive_imports_test.go's own doc
// comment): the guard that limits this package's PRODUCTION source to
// internal/assess and the standard library scans non-test files only,
// mirroring internal/assess's own import-graph guard's scope.
//
// estategen writes a collector-shaped inventory directory to disk
// (networks.csv, accounts.json, failures.csv, run.json); this file parses
// those four files back into an assess.Input with a small, self-contained
// reader (encoding/csv and encoding/json, standard library only) rather
// than importing internal/onboard's own reader, which this package may not
// import and which does far more normalization than synthetic,
// well-formed generator output ever needs.

func loadEstateInput(t *testing.T, dir string) assess.Input {
	t.Helper()
	return assess.Input{
		Records:  loadNetworksCSV(t, filepath.Join(dir, "networks.csv")),
		Accounts: loadAccountsJSON(t, filepath.Join(dir, "accounts.json")),
		Failures: loadFailuresCSV(t, filepath.Join(dir, "failures.csv")),
		Run:      loadRunJSON(t, filepath.Join(dir, "run.json")),
	}
}

func loadNetworksCSV(t *testing.T, path string) []assess.ResourceRecord {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out []assess.ResourceRecord
	for i, row := range rows {
		if i == 0 {
			continue // header: account_id,account_name,region,type,resource_id,cidr,parent_id,az_id,name,state,primary,association_id,observed_at
		}
		typ := assess.TypeVPC
		if row[3] == "subnet" {
			typ = assess.TypeSubnet
		}
		rec := assess.ResourceRecord{
			AccountID: row[0], AccountName: row[1], Region: row[2], Type: typ, ResourceID: row[4], CIDR: row[5],
			ParentID: row[6], AZID: row[7], Name: row[8], State: row[9], Primary: assess.Tri(row[10]),
			SourceFile: "networks.csv", SourceRow: i,
		}
		if row[11] != "" {
			v := row[11]
			rec.AssociationID = &v
		}
		if row[12] != "" {
			v := row[12]
			rec.ObservedAt = &v
		}
		out = append(out, rec)
	}
	return out
}

func loadAccountsJSON(t *testing.T, path string) []assess.AccountRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var raw struct {
		Accounts []struct{ Id, Name, Status string } `json:"Accounts"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	out := make([]assess.AccountRecord, len(raw.Accounts))
	for i, a := range raw.Accounts {
		out[i] = assess.AccountRecord{AccountID: a.Id, Name: a.Name, Status: a.Status}
	}
	return out
}

func loadFailuresCSV(t *testing.T, path string) []assess.FailureRow {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out []assess.FailureRow
	for i, row := range rows {
		if i == 0 {
			continue // header: account_id,account_name,region,stage,error
		}
		out = append(out, assess.FailureRow{
			AccountID: row[0], AccountName: row[1], Region: row[2], Stage: assess.FailureStage(row[3]), Error: row[4],
			SourceFile: "failures.csv", SourceRow: i,
		})
	}
	return out
}

func loadRunJSON(t *testing.T, path string) *assess.RunRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var raw struct {
		ScriptVersion         string   `json:"script_version"`
		StartedAt             string   `json:"started_at"`
		FinishedAt            string   `json:"finished_at"`
		RoleName              string   `json:"role_name"`
		ManagementAccountUsed bool     `json:"management_account_used"`
		ConfiguredRegions     []string `json:"configured_regions"`
		Accounts              []struct {
			AccountID string `json:"account_id"`
			Regions   []struct {
				Region     string `json:"region"`
				Outcome    string `json:"outcome"`
				Stage      string `json:"stage"`
				RowCount   *int   `json:"row_count"`
				ObservedAt string `json:"observed_at"`
			} `json:"regions"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	run := &assess.RunRecord{
		StartedAt: raw.StartedAt, FinishedAt: raw.FinishedAt, RoleName: raw.RoleName,
		ManagementCredentials: raw.ManagementAccountUsed, ConfiguredRegions: raw.ConfiguredRegions,
		ScriptVersion: raw.ScriptVersion, SourceFile: "run.json",
	}
	for _, a := range raw.Accounts {
		for _, r := range a.Regions {
			run.Attempts = append(run.Attempts, assess.RunAttempt{
				AccountID: a.AccountID, Region: r.Region, Outcome: assess.AttemptOutcome(r.Outcome), Stage: r.Stage, RowCount: r.RowCount,
			})
		}
	}
	return run
}

func moveResultByIdentity(rep Report) map[string]MoveResult {
	out := map[string]MoveResult{}
	for _, m := range rep.Moves {
		out[m.Subject] = m
	}
	return out
}

// TestEndToEndAgainstEstategenGappedEstate is ADR 0015's own worked
// example: "a move whose subject lies in the unread account is unknown and
// not not-observed; the moves over the planted equal-cidr pairs list those
// exact conflict ids as present; evidence.complete is false with no
// allocation evidence supplied."
func TestEndToEndAgainstEstategenGappedEstate(t *testing.T) {
	dir := t.TempDir()
	facts, err := estategen.Generate(estategen.Params{
		BaselineAccounts: 4, RegionsPerAccount: 1, VPCsPerRegion: 2, EqualCIDR: 1, Gapped: true,
	}, dir)
	if err != nil {
		t.Fatalf("estategen.Generate: %v", err)
	}
	input := loadEstateInput(t, dir)
	report := mustAssess(t, input, assess.Options{})

	eq := facts.EqualCIDR[0]
	moveA := Move{Subject: subject(eq.AccountA, eq.Region, eq.VPCA), Disposition: DispositionReplace, Resolves: []string{eq.ConflictID}}
	moveB := Move{Subject: subject(eq.AccountB, eq.Region, eq.VPCB), Disposition: DispositionReplace, Resolves: []string{eq.ConflictID}}
	unassumableMove := Move{Subject: subject(facts.UnassumableAccount, "eu-central-1", "vpc-ghost")}
	partialMove := Move{Subject: subject(facts.PartialRegion.AccountID, facts.PartialRegion.Region, "vpc-ghost")}
	neverMove := Move{Subject: subject(facts.NeverAttemptedAccount, "eu-central-1", "vpc-ghost")}
	emptyMove := Move{Subject: subject(facts.EmptyRegion.AccountID, facts.EmptyRegion.Region, "vpc-ghost")}

	plan := Plan{Moves: []Move{moveA, moveB, unassumableMove, partialMove, neverMove, emptyMove}}
	rep := Derive(plan, report, input, nil, Options{})
	byID := moveResultByIdentity(rep)

	gotA := byID[subject(eq.AccountA, eq.Region, eq.VPCA).Identity()]
	if gotA.SubjectFact != SubjectObserved {
		t.Errorf("planted equal-cidr side A: subject fact = %s, want observed", gotA.SubjectFact)
	}
	if len(gotA.Resolves) != 1 || gotA.Resolves[0].ConflictID != eq.ConflictID || gotA.Resolves[0].Status != ResolvePresent {
		t.Errorf("planted equal-cidr side A: resolves = %+v, want [%s present]", gotA.Resolves, eq.ConflictID)
	}
	gotB := byID[subject(eq.AccountB, eq.Region, eq.VPCB).Identity()]
	if gotB.SubjectFact != SubjectObserved || len(gotB.Resolves) != 1 || gotB.Resolves[0].Status != ResolvePresent {
		t.Errorf("planted equal-cidr side B did not reproduce the planted conflict: %+v", gotB)
	}

	gotUnassumable := byID[subject(facts.UnassumableAccount, "eu-central-1", "vpc-ghost").Identity()]
	if gotUnassumable.SubjectFact != SubjectUnknown || gotUnassumable.Unmatched {
		t.Errorf("unassumable account: subject = %s, unmatched = %v; want unknown, false", gotUnassumable.SubjectFact, gotUnassumable.Unmatched)
	}

	gotPartial := byID[subject(facts.PartialRegion.AccountID, facts.PartialRegion.Region, "vpc-ghost").Identity()]
	if gotPartial.SubjectFact != SubjectUnknown {
		t.Errorf("partial region: subject = %s, want unknown", gotPartial.SubjectFact)
	}

	gotNever := byID[subject(facts.NeverAttemptedAccount, "eu-central-1", "vpc-ghost").Identity()]
	if gotNever.SubjectFact != SubjectUnknown {
		t.Errorf("never-attempted account: subject = %s, want unknown", gotNever.SubjectFact)
	}

	// "Read and empty" IS a complete statement (ADR 0014, carried into ADR
	// 0015): a subject in that region reads not-observed, not unknown.
	gotEmpty := byID[subject(facts.EmptyRegion.AccountID, facts.EmptyRegion.Region, "vpc-ghost").Identity()]
	if gotEmpty.SubjectFact != SubjectNotObserved {
		t.Errorf("read-and-empty region: subject = %s, want not-observed", gotEmpty.SubjectFact)
	}

	if rep.Evidence.Complete {
		t.Error("Evidence.Complete = true, want false: no allocation evidence was supplied at all")
	}
	for _, m := range rep.Moves {
		if m.FullyEvidenced {
			t.Errorf("move %s reported fully evidenced with no evidence supplied at all", m.Subject)
		}
	}
}

// TestEndToEndCleanEstateWithFullEvidenceButNothingMovedIsNotFullyEvidenced
// is ADR 0015's second named fixture: "its clean sibling, with a plan whose
// targets are all supplied by a synthetic operator-scoped evidence file and
// whose subjects are all still observed ... because nothing has moved yet"
// -- a RESERVED (not ACTIVE) target and an observed (not not-observed)
// subject must both keep FullyEvidenced false, even though evidence.complete
// is true.
func TestEndToEndCleanEstateWithFullEvidenceButNothingMovedIsNotFullyEvidenced(t *testing.T) {
	dir := t.TempDir()
	facts, err := estategen.Generate(estategen.Params{
		BaselineAccounts: 2, RegionsPerAccount: 1, VPCsPerRegion: 1, Gapped: false,
	}, dir)
	if err != nil {
		t.Fatalf("estategen.Generate: %v", err)
	}
	input := loadEstateInput(t, dir)
	report := mustAssess(t, input, assess.Options{})
	if !report.Coverage.Complete {
		t.Fatalf("estategen's clean estate must have complete coverage, got %+v", report.Coverage)
	}

	// The one baseline VPC of the first account: still present in the
	// estate (subject stays `observed`), with a target this test supplies
	// as RESERVED (the ledger holds the space, but the platform has not
	// verified a binding).
	acctA := facts.Accounts[0]
	vpc, ok := firstVPCFor(input, acctA)
	if !ok {
		t.Fatalf("no baseline VPC found for account %s", acctA)
	}
	target := &Target{TenantID: "tenant-a", AllocationKey: "vpc-a", Scope: "vpc", Environment: "prod", Region: vpc.Region, AccountID: acctA, PrefixLength: intPtr(20)}
	evidence := DerivedEvidenceFile{Scope: scopeOperator, Path: "evidence.json", Allocations: []DerivedEvidenceAllocation{
		{TenantID: target.TenantID, AllocationKey: target.AllocationKey, Scope: target.Scope, Environment: target.Environment,
			Region: target.Region, AccountID: target.AccountID, PrefixLength: *target.PrefixLength, State: StateReserved},
	}}

	plan := Plan{Moves: []Move{{Subject: subject(acctA, vpc.Region, vpc.ResourceID), Disposition: DispositionReplace, Target: target}}}
	rep := Derive(plan, report, input, []DerivedEvidenceFile{evidence}, Options{})

	if !rep.Evidence.Complete {
		t.Fatal("Evidence.Complete = false, want true: full operator-scoped evidence was supplied over complete coverage")
	}
	got := rep.Moves[0]
	if got.SubjectFact != SubjectObserved {
		t.Errorf("subject fact = %s, want observed (the VPC has not moved)", got.SubjectFact)
	}
	if got.TargetFact != TargetReserved {
		t.Errorf("target fact = %s, want reserved (not yet ACTIVE/verified)", got.TargetFact)
	}
	if got.FullyEvidenced {
		t.Error("FullyEvidenced = true, want false: nothing has moved yet, even though evidence is complete")
	}
}

func firstVPCFor(input assess.Input, accountID string) (assess.ResourceRecord, bool) {
	for _, r := range input.Records {
		if r.Type == assess.TypeVPC && r.AccountID == accountID {
			return r, true
		}
	}
	return assess.ResourceRecord{}, false
}

// The third named fixture -- every move's three facts affirmative at once,
// the only one that is fully evidenced -- is exercised against hand-built
// (non-estategen) fixtures by TestForbiddenPhraseGuardOnTheCompleteTemplateToo
// in report_test.go and by TestNoMoveIsFullyEvidencedOnTypedInputAlone's
// baseline in derive_test.go, rather than through estategen: the generator
// has no primitive for "a VPC that used to exist and no longer does" (every
// account/region either has its planted filler rows or is one of the seven
// gapped facts), so building that specific state requires either extending
// estategen (out of this package's scope) or a hand-built assess.Input, which
// TestNoMoveIsFullyEvidencedOnTypedInputAlone already provides. See the report
// to the lead.
