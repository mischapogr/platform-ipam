package onboardcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mischapogr/platform-ipam/internal/assess/estategen"
	"github.com/mischapogr/platform-ipam/internal/cli"
	"github.com/mischapogr/platform-ipam/internal/migrate"
)

// This file is work-plan package M3b4's own end-to-end evidence
// (docs/WORK_PLAN.md, docs/decisions/0015-REVIEWED_MIGRATION_PLAN_AND_DERIVED_PROGRESS.md,
// "finally end to end, from internal/assess/estategen's generated estate"):
// a synthetic estate from internal/assess/estategen, a synthetic
// migration.yaml naming some of its VPCs, and -- unlike
// internal/migrate/derive_estate_test.go's own two estategen fixtures,
// which call Derive directly -- an allocation evidence file produced by
// THIS package's sibling command, `platform-ipam client evidence`
// (internal/cli, package M3b4's own cut), run against a real
// httptest.Server standing in for the platform API, through the REAL
// `platform-ipam onboard progress` command (onboardcmd.Main, exactly as a
// customer would invoke it) rather than by calling migrate.Derive or
// runProgress in process. Everything here is synthetic: RFC 1918/5737
// ranges and repdigit or all-zero account ids, estategen's own
// confidentiality convention (see estategen.go's package doc).
//
// The two fixtures are ADR 0015's own worked example, in its own words:
// "a move whose subject lies in the unread account is unknown and not
// not-observed; the moves over the planted equal-cidr pairs list those
// exact conflict ids as present; evidence.complete is false with no
// allocation evidence supplied; the exit is 3 ... a third fixture, in
// which every move's three facts are affirmative, is the only one that
// exits 0."

// mainAt runs onboardcmd.Main and returns the exit code and both streams,
// mirroring assess_review_test.go's own helper for exactly this reason:
// running the REAL command entry point end to end, not runProgress in
// isolation.
func mainAt(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code = Main(context.Background(), args, &out, &errBuf)
	return code, out.String(), errBuf.String()
}

// runClientEvidence runs the REAL `platform-ipam client evidence` command
// (internal/cli.Main) against server, writing the export to outPath, and
// fails the test if it does not exit 0. It sets the three environment
// variables the client needs and restores them via t.Setenv, exactly as
// internal/cli's own tests do.
func runClientEvidence(t *testing.T, server *httptest.Server, outPath string, extraArgs ...string) {
	t.Helper()
	t.Setenv("PLATFORM_IPAM_URL", server.URL)
	t.Setenv("PLATFORM_IPAM_TOKEN", "e2e-test-token")
	t.Setenv("PLATFORM_IPAM_ALLOW_LOCAL_HTTP", "1")
	args := append([]string{"evidence", "--out", outPath}, extraArgs...)
	var out, errBuf bytes.Buffer
	code := cli.Main(context.Background(), args, &out, &errBuf)
	if code != cli.ExitOK {
		t.Fatalf("client evidence exit = %d, stderr = %q", code, errBuf.String())
	}
}

// operatorAllocationPageJSON renders one GET /v1/allocations page carrying
// exactly one ACTIVE, verified allocation under tenantID/key -- an
// operator's own read, so every item carries tenant_id (ADR 0011), which is
// what lets `client evidence` infer scope "operator" without a flag.
func operatorAllocationPageJSON(tenantID, key, region, accountID string, prefixLength int) string {
	return fmt.Sprintf(`{"items":[{"id":"alloc_e2e_1","tenant_id":%q,"allocation_key":%q,"scope":"vpc","environment":"prod","region":%q,"account_id":%q,"prefix_length":%d,"parent_allocation_id":null,"state":"ACTIVE","binding":{"provider":"aws","resource_type":"vpc","resource_id":"vpc-e2e","account_id":%q,"region":%q,"verified_at":"2026-09-21T00:00:00Z"}}],"next_cursor":null}`,
		tenantID, key, region, accountID, prefixLength, accountID, region)
}

// gappedMoveYAML renders one whole `- subject: ...` move block for the
// incomplete-evidence fixture below, so two calls can be assembled in
// either order without changing which target belongs to which subject --
// the permutation test needs to reorder the LIST, never re-pair a target
// with a different subject.
func gappedMoveYAML(account, vpc, conflictID, tenantID, key string) string {
	return fmt.Sprintf(`  - subject:
      account_id: %q
      region: eu-central-1
      vpc_id: %q
    disposition: replace
    wave: wave-1
    resolves: [%q]
    target:
      tenant_id: %s
      allocation_key: %s
      scope: vpc
      environment: prod
      region: eu-central-1
      account_id: %q
      prefix_length: 20
`, account, vpc, conflictID, tenantID, key, account)
}

// gappedPlanFileYAML wraps ONE move block (gappedMoveYAML) as its own
// complete, one-move plan document, its own wave-1 definition included.
// Two such files, given to onboard progress as two separate --plan flags,
// are how the determinism test below permutes an INPUT'S order (the flag
// order) without touching what either move says -- exactly the shape
// progress_test.go's own TestProgressDeterministicAcrossRunsAndFileOrder
// uses ("permuting --plan and --allocations file order"), rather than
// reordering a list inside one file, which would leave the file's own path
// -- and therefore Report.Inputs[].path -- identical either way and prove
// nothing new (ADR 0015, "a duplicate subject within or across files":
// wave-1 is defined identically in both files, which Validate's own
// duplicate-wave refusal explicitly allows, "identical repeats across
// files stay accepted").
func gappedPlanFileYAML(move string) string {
	return `# reviewed migration plan -- end-to-end synthetic fixture (package M3b4)
version: 1
waves:
  - id: wave-1
    name: Wave 1
    owner: e2e-test
moves:
` + move
}

// TestProgressEndToEndGappedEstateExitsThreeWithNoEvidence is ADR 0015's
// first worked fixture, run through the real onboard progress binary
// surface with no --allocations at all.
func TestProgressEndToEndGappedEstateExitsThreeWithNoEvidence(t *testing.T) {
	dir := t.TempDir()
	facts, err := estategen.Generate(estategen.Params{
		BaselineAccounts: 4, RegionsPerAccount: 1, VPCsPerRegion: 2, EqualCIDR: 1, Gapped: true,
	}, dir)
	if err != nil {
		t.Fatalf("estategen.Generate: %v", err)
	}
	eq := facts.EqualCIDR[0]

	moveA := gappedMoveYAML(eq.AccountA, eq.VPCA, eq.ConflictID, "tenant-a", "alloc-a")
	moveB := gappedMoveYAML(eq.AccountB, eq.VPCB, eq.ConflictID, "tenant-b", "alloc-b")

	planAPath := filepath.Join(dir, "migration-a.yaml")
	if err := os.WriteFile(planAPath, []byte(gappedPlanFileYAML(moveA)), 0o644); err != nil {
		t.Fatalf("writing plan A: %v", err)
	}
	planBPath := filepath.Join(dir, "migration-b.yaml")
	if err := os.WriteFile(planBPath, []byte(gappedPlanFileYAML(moveB)), 0o644); err != nil {
		t.Fatalf("writing plan B: %v", err)
	}

	runOnce := func(format string, planFirst, planSecond string) (code int, stdout string) {
		code, stdout, stderr := mainAt(t, "progress", "--plan", planFirst, "--plan", planSecond, "--inventory", dir, "--format", format)
		if code != ExitValidation {
			t.Fatalf("[%s] exit = %d, want %d (incomplete evidence); stderr = %q\nstdout=%s", format, code, ExitValidation, stderr, stdout)
		}
		return code, stdout
	}

	_, jsonOut := runOnce("json", planAPath, planBPath)
	_, textOut := runOnce("text", planAPath, planBPath)

	var rep migrate.Report
	if err := json.Unmarshal([]byte(jsonOut), &rep); err != nil {
		t.Fatalf("decoding report JSON: %v\n%s", err, jsonOut)
	}
	if rep.Evidence.Complete {
		t.Error("Evidence.Complete = true, want false: no allocation evidence was supplied")
	}
	byIdentity := map[string]migrate.MoveResult{}
	for _, m := range rep.Moves {
		byIdentity[m.Subject] = m
	}
	for _, side := range []struct{ account, vpc string }{{eq.AccountA, eq.VPCA}, {eq.AccountB, eq.VPCB}} {
		identity := "aws:" + side.account + ":eu-central-1:" + side.vpc
		mv, ok := byIdentity[identity]
		if !ok {
			t.Fatalf("no move result for %s; moves = %+v", identity, rep.Moves)
		}
		if mv.SubjectFact != migrate.SubjectObserved {
			t.Errorf("%s: subject fact = %s, want observed", identity, mv.SubjectFact)
		}
		if len(mv.Resolves) != 1 || mv.Resolves[0].ConflictID != eq.ConflictID || mv.Resolves[0].Status != migrate.ResolvePresent {
			t.Errorf("%s: resolves = %+v, want [%s present]", identity, mv.Resolves, eq.ConflictID)
		}
		if mv.FullyEvidenced {
			t.Errorf("%s: fully evidenced with no allocation evidence supplied at all", identity)
		}
	}

	assertE2EProse(t, rep, textOut, jsonOut)

	// Determinism, part one: the same inputs, run twice, byte-identical.
	_, jsonAgain := runOnce("json", planAPath, planBPath)
	if jsonAgain != jsonOut {
		t.Errorf("two runs over the same inputs produced different JSON:\nfirst:  %s\nsecond: %s", jsonOut, jsonAgain)
	}

	// Determinism, part two: the SAME two files, given to --plan in the
	// OPPOSITE order, must produce the identical report -- ADR 0015, "the
	// same plan with its moves and waves reordered producing the same
	// report," exercised here exactly as
	// TestProgressDeterministicAcrossRunsAndFileOrder (progress_test.go)
	// exercises it: "permuting --plan and --allocations file order."
	_, swappedOut := runOnce("json", planBPath, planAPath)
	if swappedOut != jsonOut {
		t.Errorf("permuting --plan file order changed the report:\noriginal: %s\npermuted: %s", jsonOut, swappedOut)
	}
}

// TestProgressEndToEndCleanEstateAllAffirmativeExitsZero is ADR 0015's
// third worked fixture: every move's three facts affirmative at once is
// the only case that exits 0. The subject names a VPC id estategen never
// wrote into its otherwise fully-covered baseline account/region, so its
// subject fact is not-observed (not unknown: coverage for that
// account/region is complete); the allocation evidence -- produced by the
// REAL `client evidence` export against an httptest server -- carries an
// ACTIVE, verified allocation whose immutable fields equal the plan's
// target exactly, and the move resolves no conflict and has none unclaimed.
func TestProgressEndToEndCleanEstateAllAffirmativeExitsZero(t *testing.T) {
	dir := t.TempDir()
	facts, err := estategen.Generate(estategen.Params{
		BaselineAccounts: 1, RegionsPerAccount: 1, VPCsPerRegion: 1, Gapped: false,
	}, dir)
	if err != nil {
		t.Fatalf("estategen.Generate: %v", err)
	}
	accountID := facts.Accounts[0]
	// estategen's own vpcID() sequence starts at "vpc-0000000001" for a
	// single-account, single-region, single-VPC estate: an id it never
	// generates, in the same account and region, which coverage still
	// reads as complete.
	const goneVPC = "vpc-9999999999"
	const tenantID, key = "tenant-a", "alloc-a"
	const prefixLength = 20

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, operatorAllocationPageJSON(tenantID, key, "eu-central-1", accountID, prefixLength))
	}))
	defer server.Close()

	evidencePath := filepath.Join(dir, "evidence.json")
	runClientEvidence(t, server, evidencePath)

	// The evidence file itself must be exactly migrate.EvidenceFile's shape
	// -- the round trip ADR 0015 requires -- before it is ever handed to
	// onboard progress.
	raw, err := os.ReadFile(evidencePath)
	if err != nil {
		t.Fatalf("reading exported evidence: %v", err)
	}
	ev, err := migrate.DecodeEvidence(bytes.NewReader(raw), evidencePath)
	if err != nil {
		t.Fatalf("exported evidence did not decode through migrate.DecodeEvidence: %v", err)
	}
	if ev.Scope != migrate.EvidenceScopeOperator {
		t.Fatalf("exported scope = %q, want %q", ev.Scope, migrate.EvidenceScopeOperator)
	}

	planPath := filepath.Join(dir, "migration.yaml")
	planYAML := fmt.Sprintf(`# reviewed migration plan -- end-to-end synthetic fixture (package M3b4)
version: 1
plan_id: e2e-clean-estate
waves:
  - id: wave-1
    name: Wave 1
    owner: e2e-test
moves:
  - subject:
      account_id: %q
      region: eu-central-1
      vpc_id: %q
    disposition: replace
    wave: wave-1
    target:
      tenant_id: %s
      allocation_key: %s
      scope: vpc
      environment: prod
      region: eu-central-1
      account_id: %q
      prefix_length: %d
`, accountID, goneVPC, tenantID, key, accountID, prefixLength)
	if err := os.WriteFile(planPath, []byte(planYAML), 0o644); err != nil {
		t.Fatalf("writing plan: %v", err)
	}

	runOnce := func(format string) (code int, stdout string) {
		code, stdout, stderr := mainAt(t, "progress", "--plan", planPath, "--inventory", dir, "--allocations", evidencePath, "--format", format)
		if code != ExitOK {
			t.Fatalf("[%s] exit = %d, want %d (every fact affirmative); stderr = %q\nstdout=%s", format, code, ExitOK, stderr, stdout)
		}
		return code, stdout
	}

	_, jsonOut := runOnce("json")
	_, textOut := runOnce("text")

	var rep migrate.Report
	if err := json.Unmarshal([]byte(jsonOut), &rep); err != nil {
		t.Fatalf("decoding report JSON: %v\n%s", err, jsonOut)
	}
	if !rep.Evidence.Complete {
		t.Error("Evidence.Complete = false, want true")
	}
	if len(rep.Moves) != 1 {
		t.Fatalf("moves = %d, want 1", len(rep.Moves))
	}
	mv := rep.Moves[0]
	if mv.SubjectFact != migrate.SubjectNotObserved {
		t.Errorf("subject fact = %s, want not-observed", mv.SubjectFact)
	}
	if mv.TargetFact != migrate.TargetActive {
		t.Errorf("target fact = %s, want active", mv.TargetFact)
	}
	if !mv.FullyEvidenced {
		t.Errorf("move not reported fully evidenced: %+v", mv)
	}
	if rep.Summary.Derived.MovesTotal != 1 || rep.Summary.Derived.FullyEvidenced != 1 {
		t.Errorf("derived summary = %+v, want 1 move, 1 fully evidenced", rep.Summary.Derived)
	}

	assertE2EProse(t, rep, textOut, jsonOut)

	// Determinism across two runs.
	_, jsonAgain := runOnce("json")
	if jsonAgain != jsonOut {
		t.Errorf("two runs over the same inputs produced different JSON:\nfirst:  %s\nsecond: %s", jsonOut, jsonAgain)
	}
}

// assertE2EProse mirrors progress_test.go's own assertProgressReportProse.
// The forbidden-phrase check reads only this report's OWN PROSE --
// Summary.Sentence and Notes, decoded from rep -- never the raw JSON text,
// because the mandated field name itself is "evidence.complete" and
// "complete" is one of ADR 0015's forbidden words: the M3b2 review recorded
// this exact contradiction (docs/WORK_PLAN.md, M3b2 review note) and its
// resolution is that the guard scans prose, not field names. The "%"
// check, by contrast, DOES scan the full raw text of both renderings --
// Go's own format verbs leave no literal "%" behind, so a real one means a
// stray percentage crept in somewhere.
func assertE2EProse(t *testing.T, rep migrate.Report, textOut, jsonOut string) {
	t.Helper()
	ownProse := strings.ToLower(rep.Summary.Sentence + " " + strings.Join(rep.Notes, " "))
	lowerText := strings.ToLower(textOut)
	for _, phrase := range migrate.ForbiddenPhrases() {
		if strings.Contains(ownProse, phrase) {
			t.Errorf("report's own prose contains forbidden phrase %q: %q", phrase, ownProse)
		}
		if strings.Contains(lowerText, phrase) {
			t.Errorf("text output contains forbidden phrase %q", phrase)
		}
	}
	if strings.Contains(jsonOut, "%") {
		t.Error("JSON output contains a % character")
	}
	if strings.Contains(textOut, "%") {
		t.Error("text output contains a % character")
	}
}

// TestProgressOutFlagCreatesExactlyOneFileWhenGivenNoneWhenAbsent closes a
// gap in ADR 0015's own evidence list that progress_test.go (M3b3) does not
// cover: "a test that a full run creates no file when --out is absent and
// exactly one when it is present." assess_test.go's sibling test
// (TestAssessOutFlagWritesSameBytesAsStdout) only checks --out's bytes match
// stdout when --out IS given; this test additionally asserts the absent
// case creates nothing at all.
func TestProgressOutFlagCreatesExactlyOneFileWhenGivenNoneWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	facts, err := estategen.Generate(estategen.Params{
		BaselineAccounts: 1, RegionsPerAccount: 1, VPCsPerRegion: 1, Gapped: false,
	}, dir)
	if err != nil {
		t.Fatalf("estategen.Generate: %v", err)
	}
	acct := facts.Accounts[0]
	planPath := filepath.Join(dir, "migration.yaml")
	planYAML := fmt.Sprintf(`version: 1
waves:
  - id: wave-1
    name: Wave 1
moves:
  - subject:
      account_id: %q
      region: eu-central-1
      vpc_id: vpc-9999999999
    disposition: retire
    wave: wave-1
`, acct)
	if err := os.WriteFile(planPath, []byte(planYAML), 0o644); err != nil {
		t.Fatalf("writing plan: %v", err)
	}

	before, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading dir before: %v", err)
	}
	code, stdout, stderr := mainAt(t, "progress", "--plan", planPath, "--inventory", dir, "--format", "json")
	if code != ExitValidation {
		t.Fatalf("exit = %d, want %d; stderr = %q", code, ExitValidation, stderr)
	}
	if stdout == "" {
		t.Fatal("stdout was empty for a report that should have been produced")
	}
	after, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading dir after: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("directory entries changed from %d to %d with --out absent: no file should have been created", len(before), len(after))
	}

	outPath := filepath.Join(dir, "report.json")
	code, stdoutWithOut, stderr := mainAt(t, "progress", "--plan", planPath, "--inventory", dir, "--format", "json", "--out", outPath)
	if code != ExitValidation {
		t.Fatalf("exit = %d, want %d; stderr = %q", code, ExitValidation, stderr)
	}
	fileBytes, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading --out file: %v", err)
	}
	if string(fileBytes) != stdoutWithOut {
		t.Errorf("--out bytes differ from stdout\n--out:  %q\nstdout: %q", fileBytes, stdoutWithOut)
	}
}
