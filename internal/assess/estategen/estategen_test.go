package estategen

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/mischapogr/platform-ipam/internal/assess"
	"github.com/mischapogr/platform-ipam/internal/onboardcmd"
)

// gappedParams is the "gapped" estate: every planted conflict kind and every
// coverage gap docs/WORK_PLAN.md's M1b4 block and ADR 0014's evidence
// paragraph name together.
func gappedParams() Params {
	return Params{
		// 8 baseline accounts: 6 distinct accounts for the three planted
		// pairs (2 each, never shared -- see Generate's own comment on
		// pick) plus the 2 the empty-region and partial-region accounts
		// need.
		Seed: 1, BaselineAccounts: 8, RegionsPerAccount: 2, VPCsPerRegion: 2,
		SubnetsUnderOneVPC: 3, EqualCIDR: 2, Contains: 1, Gapped: true,
	}
}

func runAssessOnDir(t *testing.T, dir string, extraArgs ...string) (code int, report assess.Report, stderr string) {
	t.Helper()
	args := append([]string{"assess", "--inventory", dir,
		"--matrix", filepath.Join(dir, "matrix.yaml"),
		"--ownership", filepath.Join(dir, "ownership.yaml"),
		"--fixed", filepath.Join(dir, "fixed.yaml"),
		"--decisions", filepath.Join(dir, "decisions.yaml"),
		"--format", "json"}, extraArgs...)
	var stdout, errBuf bytes.Buffer
	code = onboardcmd.Main(context.Background(), args, &stdout, &errBuf)
	if stdout.Len() > 0 {
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatalf("decoding report JSON: %v\n%s", err, stdout.String())
		}
	}
	return code, report, errBuf.String()
}

// TestGappedEstateProducesExactlyThePlantedRelationships is this package's
// central test: docs/WORK_PLAN.md's M1b4 block requires "the generator's
// own tests assert the planted facts come back out of `onboard assess`":
// the exact number of relationships by kind, the named pairs, the
// three-valued impact against the generated matrix, the coverage lists
// entry by entry, complete: false, exit 3.
func TestGappedEstateProducesExactlyThePlantedRelationships(t *testing.T) {
	dir := t.TempDir()
	facts, err := Generate(gappedParams(), dir)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	code, report, stderr := runAssessOnDir(t, dir)
	if code != onboardcmd.ExitValidation {
		t.Fatalf("exit = %d, want ExitValidation (3); stderr=%s", code, stderr)
	}

	// --- exact count and kind breakdown ---
	if report.Summary.TotalRelationships != facts.TotalVPCToVPCRelationships {
		t.Errorf("TotalRelationships = %d, want %d", report.Summary.TotalRelationships, facts.TotalVPCToVPCRelationships)
	}
	wantEqual, wantContains := len(facts.EqualCIDR), len(facts.Contains)
	gotEqual, gotContains := 0, 0
	byID := map[string]assess.Conflict{}
	for _, c := range report.Conflicts {
		byID[c.ID] = c
		switch c.Kind {
		case assess.KindEqualCIDR:
			gotEqual++
		case assess.KindContains:
			gotContains++
		}
	}
	if gotEqual != wantEqual {
		t.Errorf("equal-cidr conflicts = %d, want %d", gotEqual, wantEqual)
	}
	if gotContains != wantContains {
		t.Errorf("contains conflicts = %d, want %d", gotContains, wantContains)
	}

	// --- the named pairs, by conflict id ---
	for i, f := range facts.EqualCIDR {
		c, ok := byID[f.ConflictID]
		if !ok {
			t.Fatalf("equal-cidr[%d]: conflict id %s not in report", i, f.ConflictID)
		}
		assertSidesName(t, c, f.VPCA, f.VPCB, f.CIDR, f.CIDR)
	}
	for i, f := range facts.Contains {
		c, ok := byID[f.ConflictID]
		if !ok {
			t.Fatalf("contains[%d]: conflict id %s not in report", i, f.ConflictID)
		}
		assertSidesName(t, c, f.VPCOuter, f.VPCInner, f.OuterCIDR, f.InnerCIDR)
	}

	// --- three-valued impact against the generated matrix ---
	if c := byID[facts.ConfirmedConflictID]; c.Impact != assess.ImpactConfirmed {
		t.Errorf("confirmed pair's Impact = %q, want confirmed (matrix.yaml requires its two groups to communicate)", c.Impact)
	}
	if c := byID[facts.IsolatedConflictID]; c.Impact != assess.ImpactPotential || c.MatrixRelation != assess.RelationMustStayIsolated {
		t.Errorf("isolated pair's Impact/MatrixRelation = %q/%q, want potential/must_stay_isolated", c.Impact, c.MatrixRelation)
	}
	if facts.UnknownConflictID != "" {
		if c := byID[facts.UnknownConflictID]; c.Impact != assess.ImpactUnknown {
			t.Errorf("unmatrixed pair's Impact = %q, want unknown", c.Impact)
		}
	}

	// --- decision echoed back for the confirmed conflict ---
	if c := byID[facts.ConfirmedConflictID]; c.Decision == nil || c.Decision.Decision != "accepted-risk" {
		t.Errorf("confirmed conflict's Decision = %+v, want the decisions.yaml entry echoed back", c.Decision)
	}

	// --- coverage lists, entry by entry ---
	if report.Coverage.Complete {
		t.Fatal("Coverage.Complete = true, want false: this estate plants an unassumable account, a partial region and a never-attempted account")
	}
	assertAccountRegionPresent(t, "Coverage.Failed", failedAccounts(report.Coverage.Failed), facts.UnassumableAccount, "")
	assertAccountRegionPresent(t, "Coverage.Failed", failedAccounts(report.Coverage.Failed), facts.PartialRegion.AccountID, facts.PartialRegion.Region)
	assertAccountRegionPresent(t, "Coverage.Partial", toAccountRegions(report.Coverage.Partial), facts.PartialRegion.AccountID, facts.PartialRegion.Region)
	assertAccountRegionPresent(t, "Coverage.NotAttempted", toAccountRegions(report.Coverage.NotAttempted), facts.UnassumableAccount, "")
	assertAccountRegionPresent(t, "Coverage.NotAttempted", toAccountRegions(report.Coverage.NotAttempted), facts.NeverAttemptedAccount, "")
	assertAccountRegionPresent(t, "Coverage.ReadEmpty", toAccountRegions(report.Coverage.ReadEmpty), facts.EmptyRegion.AccountID, facts.EmptyRegion.Region)

	// --- duplicate observation collapsed and counted ---
	if report.Summary.DuplicateObservations != 1 {
		t.Errorf("DuplicateObservations = %d, want 1 (the planted duplicate row)", report.Summary.DuplicateObservations)
	}

	// --- the owner's sentence names the gap ---
	if !strings.Contains(report.Summary.Sentence, "this report cannot make a complete statement for") {
		t.Errorf("sentence = %q, want the incomplete template", report.Summary.Sentence)
	}
	for _, phrase := range assess.ForbiddenCompletePhrases() {
		if strings.Contains(report.Summary.Sentence, phrase) {
			t.Errorf("sentence contains forbidden phrase %q: %s", phrase, report.Summary.Sentence)
		}
	}

	if report.Clean() {
		t.Fatal("Report.Clean() = true, want false: the confirmed conflict and the coverage gaps must both fail it")
	}
}

func assertSidesName(t *testing.T, c assess.Conflict, vpcA, vpcB, cidrA, cidrB string) {
	t.Helper()
	if len(c.Sides) != 2 {
		t.Fatalf("conflict %s has %d sides, want 2", c.ID, len(c.Sides))
	}
	want := map[string]string{vpcA: cidrA, vpcB: cidrB}
	for _, s := range c.Sides {
		wantCIDR, ok := want[s.VPCID]
		if !ok {
			t.Errorf("conflict %s: unexpected side VPC %s (want %s or %s)", c.ID, s.VPCID, vpcA, vpcB)
			continue
		}
		if s.CIDR != wantCIDR {
			t.Errorf("conflict %s: side %s CIDR = %s, want %s", c.ID, s.VPCID, s.CIDR, wantCIDR)
		}
		delete(want, s.VPCID)
	}
	if len(want) != 0 {
		t.Errorf("conflict %s: missing side(s) %v", c.ID, want)
	}
}

func toAccountRegions(list []assess.AccountRegion) []assess.AccountRegion { return list }

func failedAccounts(list []assess.FailedEntry) []assess.AccountRegion {
	out := make([]assess.AccountRegion, len(list))
	for i, f := range list {
		out[i] = assess.AccountRegion{AccountID: f.AccountID, Region: f.Region}
	}
	return out
}

func assertAccountRegionPresent(t *testing.T, label string, list []assess.AccountRegion, acct, region string) {
	t.Helper()
	for _, e := range list {
		if e.AccountID == acct && e.Region == region {
			return
		}
	}
	t.Errorf("%s does not contain {%s %s}: %+v", label, acct, region, list)
}

// TestCleanEstateIsCleanAndCompletelyRead is the second half
// docs/WORK_PLAN.md's M1b4 block requires: "a second, fully covered estate
// with no confirmed conflict: complete: true, exit 0."
func TestCleanEstateIsCleanAndCompletelyRead(t *testing.T) {
	dir := t.TempDir()
	facts, err := Generate(Params{Seed: 1, BaselineAccounts: 3, RegionsPerAccount: 2, VPCsPerRegion: 2, Gapped: false}, dir)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(facts.EqualCIDR) != 0 || len(facts.Contains) != 0 {
		t.Fatalf("a Gapped=false, EqualCIDR=0, Contains=0 estate planted a conflict: %+v", facts)
	}

	code, report, stderr := runAssessOnDir(t, dir)
	if code != onboardcmd.ExitOK {
		t.Fatalf("exit = %d, want ExitOK (0); stderr=%s", code, stderr)
	}
	if !report.Coverage.Complete {
		t.Errorf("Coverage.Complete = false, want true: every account and region was fully read")
	}
	if len(report.Conflicts) != 0 {
		t.Errorf("Conflicts = %+v, want none", report.Conflicts)
	}
	if !report.Clean() {
		t.Errorf("Report.Clean() = false, want true")
	}
}

// TestGeneratedFilesAreConsumedThroughTheRealReader is a narrower sanity
// check than the two tests above: it asserts the files Generate wrote
// parse at all through the collector-shaped decoders (a malformed run.json
// or networks.csv would otherwise surface only as an opaque ExitAdapter in
// the tests above, which name a symptom rather than the file at fault).
func TestGeneratedFilesAreConsumedThroughTheRealReader(t *testing.T) {
	dir := t.TempDir()
	if _, err := Generate(gappedParams(), dir); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for _, name := range []string{"networks.csv", "accounts.json", "failures.csv", "run.json", "matrix.yaml", "ownership.yaml", "fixed.yaml", "decisions.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("Generate did not write %s: %v", name, err)
		}
	}
	code, _, stderr := runAssessOnDir(t, dir)
	if code != onboardcmd.ExitValidation {
		t.Fatalf("exit = %d, want ExitValidation (3); stderr=%s", code, stderr)
	}
}

// --- confidentiality ---

// TestGeneratedFilesContainNoTwelveDigitSequenceOtherThanSynthetic scans
// every byte this package wrote to disk (not Go source -- see this
// package's own doc comment) for a twelve-digit sequence and asserts each
// one is one of the account ids Generate itself reported in Facts.Accounts.
// This is the confidentiality guard ADR 0014 requires ("A test asserts that
// no fixture in the package contains a twelve-digit account id other than
// the all-zero one") applied to this package's OWN output files, which are
// not Go string literals and so are invisible to
// internal/assess/assess_test.go's AST-scanning guard.
func TestGeneratedFilesContainNoTwelveDigitSequenceOtherThanSynthetic(t *testing.T) {
	dir := t.TempDir()
	facts, err := Generate(gappedParams(), dir)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	synthetic := map[string]bool{}
	for _, id := range facts.Accounts {
		synthetic[id] = true
	}
	twelveDigits := regexp.MustCompile(`[0-9]{12}`)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range twelveDigits.FindAllString(string(data), -1) {
			if !synthetic[match] {
				t.Errorf("%s contains a twelve-digit sequence %q that is not one of Facts.Accounts", entry.Name(), match)
			}
		}
	}
}

// TestGenerateRejectsInsufficientBaselineAccounts exercises Generate's own
// parameter guard.
func TestGenerateRejectsInsufficientBaselineAccounts(t *testing.T) {
	dir := t.TempDir()
	_, err := Generate(Params{BaselineAccounts: 2, RegionsPerAccount: 1, VPCsPerRegion: 1, Gapped: true}, dir)
	if err == nil {
		t.Fatal("Generate with Gapped and too few BaselineAccounts did not error")
	}
}
