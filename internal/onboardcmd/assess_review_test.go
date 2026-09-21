package onboardcmd

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A row the normalizer drops is a resource nobody compared. The report must
// not be produced as though that row had never been in the file.
func TestAssessRefusesWhenARowWasDropped(t *testing.T) {
	dir := t.TempDir()
	dropped := networksCleanCSV + "111111111111,not-a-cidr,eu-central-1,vpc,vpc-0dropped000000001,dropped,available,true,vpc-cidr-assoc-ddd1,2026-09-20T00:00:00Z\n"
	code, stdout, stderr := runAssessArgs(
		"--networks", mustWrite(t, dir, "networks.csv", dropped),
		"--accounts", mustWrite(t, dir, "accounts.json", accountsCleanJSON),
		"--failures", mustWrite(t, dir, "failures.csv", failuresEmptyCSV),
		"--run", mustWrite(t, dir, "run.json", runCleanJSON),
		"--format", "json")
	if code != ExitAdapter || stdout != "" {
		t.Fatalf("exit = %d, stdout = %d bytes; want exit %d and no report. stderr=%s", code, len(stdout), ExitAdapter, stderr)
	}
	if !strings.Contains(stderr, "networks.csv") || !strings.Contains(stderr, "not-a-cidr") {
		t.Fatalf("stderr does not name the file and the value: %s", stderr)
	}
}

// An inventory directory without networks.csv, and a run with no networks
// input at all, have read no network data: neither may answer with a report.
func TestAssessNeedsNetworkData(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, dir, "accounts.json", accountsCleanJSON)
	mustWrite(t, dir, "failures.csv", failuresEmptyCSV)
	mustWrite(t, dir, "run.json", runCleanJSON)
	if _, err := os.Stat(filepath.Join(dir, "networks.csv")); err == nil {
		t.Fatal("fixture unexpectedly has networks.csv")
	}
	code, stdout, stderr := runAssessArgs("--inventory", dir, "--format", "json")
	if code != ExitAdapter || stdout != "" {
		t.Fatalf("--inventory without networks.csv: exit = %d, stdout = %d bytes; stderr=%s", code, len(stdout), stderr)
	}
	code, stdout, stderr = runAssessArgs("--accounts", filepath.Join(dir, "accounts.json"), "--format", "json")
	if code != ExitUsage || stdout != "" {
		t.Fatalf("no networks input: exit = %d, stdout = %d bytes; stderr=%s", code, len(stdout), stderr)
	}
}

// The collector writes regions_attempted "unknown" for an account whose region
// list it never learned. That account was not read, whatever else succeeded.
func TestAssessAccountWithUnknownRegionSetIsNotAttempted(t *testing.T) {
	dir := t.TempDir()
	run := `{"script_version":"2","accounts":[
	  {"account_id":"111111111111","regions_attempted":"known","regions":[{"region":"eu-central-1","outcome":"succeeded","row_count":1}]},
	  {"account_id":"222222222222","regions_attempted":"unknown","not_attempted_reason":"region-list-unavailable","regions":[]}]}`
	accounts := `{"Accounts":[{"Id":"111111111111","Name":"Alpha","Status":"ACTIVE"},{"Id":"222222222222","Name":"Beta","Status":"ACTIVE"}]}`
	code, stdout, stderr := runAssessArgs(
		"--networks", mustWrite(t, dir, "networks.csv", networksCleanCSV),
		"--accounts", mustWrite(t, dir, "accounts.json", accounts),
		"--run", mustWrite(t, dir, "run.json", run), "--format", "json")
	if code != ExitValidation {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, ExitValidation, stderr)
	}
	report := decodeReport(t, stdout)
	if report.Coverage.Complete || len(report.Coverage.NotAttempted) != 1 ||
		report.Coverage.NotAttempted[0].AccountID != "222222222222" || report.Coverage.NotAttempted[0].Region != "" {
		t.Fatalf("coverage = %+v", report.Coverage)
	}
}

// A type that is neither vpc nor subnet cannot be classified, and a table that
// is not a networks table holds no network data: both refuse.
func TestAssessRefusesWhatItCannotClassify(t *testing.T) {
	dir := t.TempDir()
	gateway := strings.Replace(networksCleanCSV, ",vpc,", ",transit-gateway,", 1)
	code, stdout, stderr := runAssessArgs("--networks", mustWrite(t, dir, "networks.csv", gateway), "--format", "json")
	if code != ExitAdapter || stdout != "" || !strings.Contains(stderr, "transit-gateway") {
		t.Fatalf("unknown type: exit = %d, stdout = %d bytes; stderr=%s", code, len(stdout), stderr)
	}
	accountsTable := "account_id,account_name\n111111111111,Alpha\n"
	code, stdout, stderr = runAssessArgs("--networks", mustWrite(t, dir, "accounts.csv", accountsTable), "--format", "json")
	if code != ExitAdapter || stdout != "" {
		t.Fatalf("an accounts table as --networks: exit = %d, stdout = %d bytes; stderr=%s", code, len(stdout), stderr)
	}
}

// A conflict against a fixed range names the fixed table and the entry, and the
// report lists every file it read with the digest of what it read.
func TestAssessNamesTheFixedEntryAndDigestsItsInputs(t *testing.T) {
	dir := t.TempDir()
	networks := mustWrite(t, dir, "networks.csv", networksCleanCSV)
	fixed := mustWrite(t, dir, "fixed.yaml", "# reviewed by the network team\n- cidr: 192.0.2.0/24\n  description: documentation\n- cidr: 10.0.0.0/8\n  description: on-premises\n  owner: network\n")
	code, stdout, stderr := runAssessArgs("--networks", networks, "--fixed", fixed, "--format", "json")
	if code != ExitValidation {
		t.Fatalf("exit = %d; stderr=%s", code, stderr)
	}
	report := decodeReport(t, stdout)
	if len(report.Conflicts) != 1 {
		t.Fatalf("conflicts = %+v", report.Conflicts)
	}
	found := false
	for _, side := range report.Conflicts[0].Sides {
		if side.Fixed {
			found = side.SourceFile == fixed && side.SourceRow == 2
		}
	}
	if !found {
		t.Fatalf("the fixed side does not name %s entry 2: %+v", fixed, report.Conflicts[0].Sides)
	}
	digests := map[string]string{}
	for _, in := range report.Inputs {
		digests[in.Path] = in.SHA256
	}
	for _, path := range []string{networks, fixed} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if want := fmt.Sprintf("%x", sha256.Sum256(data)); digests[path] != want {
			t.Fatalf("digest of %s = %q, want %q", path, digests[path], want)
		}
	}
}

// An interrupted collector run leaves no run.json. Read through --inventory
// that is unknown coverage, not an unreadable input; named explicitly, a file
// that is not there is an error.
func TestAssessInventoryWithoutRunRecordDegrades(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, dir, "networks.csv", networksCleanCSV)
	mustWrite(t, dir, "accounts.json", accountsCleanJSON)
	code, stdout, stderr := runAssessArgs("--inventory", dir, "--format", "json")
	if code != ExitValidation {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, ExitValidation, stderr)
	}
	if report := decodeReport(t, stdout); report.Coverage.Complete || !report.Coverage.RunMissing {
		t.Fatalf("coverage = %+v", report.Coverage)
	}
	code, stdout, _ = runAssessArgs("--inventory", dir, "--run", filepath.Join(dir, "run.json"), "--format", "json")
	if code != ExitAdapter || stdout != "" {
		t.Fatalf("an explicit --run that does not exist: exit = %d, stdout = %d bytes", code, len(stdout))
	}
}
