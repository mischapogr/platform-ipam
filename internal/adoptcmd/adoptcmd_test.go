package adoptcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMainWithNoArgsIsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), nil, domain.Config{}, newFakeService(), &stdout, &stderr)
	if code != ExitUsage {
		t.Fatalf("want exit %d, got %d", ExitUsage, code)
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Fatalf("stderr does not show usage: %q", stderr.String())
	}
}

func TestMainUnknownCommandIsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), []string{"bogus"}, domain.Config{}, newFakeService(), &stdout, &stderr)
	if code != ExitUsage {
		t.Fatalf("want exit %d, got %d", ExitUsage, code)
	}
}

func TestMainPlanMissingTablePathIsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), []string{"plan"}, domain.Config{}, newFakeService(), &stdout, &stderr)
	if code != ExitUsage {
		t.Fatalf("want exit %d, got %d", ExitUsage, code)
	}
}

func TestMainApplyMissingOperatorIsUsage(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "records.csv", validHeader+"team-a,vpc-orders,vpc,prod,eu-central-1,123456789012,10.1.0.0/24,vpc-0abc,42,,\n")
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), []string{"apply", path}, testConfig(), newFakeService(), &stdout, &stderr)
	if code != ExitUsage {
		t.Fatalf("want exit %d (missing --operator), got %d: stderr=%s", ExitUsage, code, stderr.String())
	}
}

func TestMainPlanUnknownColumnExitsValidation(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "records.csv",
		strings.TrimSuffix(validHeader, "\n")+",prefix_length\n"+
			"team-a,vpc-orders,vpc,prod,eu-central-1,123456789012,10.1.0.0/24,vpc-0abc,42,,,24\n")
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), []string{"plan", path}, testConfig(), newFakeService(), &stdout, &stderr)
	if code != ExitValidation {
		t.Fatalf("want exit %d, got %d: stderr=%s", ExitValidation, code, stderr.String())
	}
}

func TestMainPlanHappyPathPrintsValidJSONAndExitsOK(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "records.csv", validHeader+"team-a,vpc-orders,vpc,prod,eu-central-1,123456789012,10.1.0.0/24,vpc-0abc,42,,\n")
	svc := newFakeService()
	svc.setPlan("team-a", "vpc-orders", freshVerdict("10.1.0.0/24", "pool1", "d1", 24), nil)
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), []string{"plan", path}, testConfig(), svc, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("want exit %d, got %d: stderr=%s", ExitOK, code, stderr.String())
	}
	var rep Report
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout.String())
	}
	if len(rep.Records) != 1 || rep.Records[0].Verdict != VerdictWouldAdopt {
		t.Fatalf("report: %#v", rep)
	}
}

func TestMainApplyHappyPathPrintsValidJSONAndExitsOK(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "records.csv", validHeader+"team-a,vpc-orders,vpc,prod,eu-central-1,123456789012,10.1.0.0/24,vpc-0abc,42,,\n")
	svc := newFakeService()
	svc.setPlan("team-a", "vpc-orders", freshVerdict("10.1.0.0/24", "pool1", "d1", 24), nil)
	svc.setAdopt("team-a", "vpc-orders", &domain.Allocation{ID: "alloc_1", CIDR: "10.1.0.0/24"}, 201, nil)
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), []string{"apply", path, "--operator", "ops:alice"}, testConfig(), svc, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("want exit %d, got %d: stderr=%s", ExitOK, code, stderr.String())
	}
	var rep ApplyReport
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout.String())
	}
	if len(rep.Outcomes) != 1 || rep.Outcomes[0].Status != OutcomeAdopted {
		t.Fatalf("report: %#v", rep)
	}
}

func TestMainApplyRefusedPlanExitsValidationAndWritesJSON(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "records.csv", validHeader+"team-a,vpc-orders,vpc,prod,eu-central-1,123456789012,10.1.0.0/24,vpc-0abc,42,,\n")
	svc := newFakeService()
	svc.setPlan("team-a", "vpc-orders", nil, domain.Err(409, "adoption_refused", "occupied"))
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), []string{"apply", path, "--operator", "alice"}, testConfig(), svc, &stdout, &stderr)
	if code != ExitValidation {
		t.Fatalf("want exit %d, got %d: stderr=%s", ExitValidation, code, stderr.String())
	}
	var rep ApplyReport
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout.String())
	}
	if len(svc.adoptCalls) != 0 {
		t.Fatalf("Adopt must not be called: %#v", svc.adoptCalls)
	}
}

func TestMainApplyPendingExitsValidation(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "records.csv", validHeader+"team-a,vpc-orders,vpc,prod,eu-central-1,123456789012,10.1.0.0/24,vpc-0abc,42,,\n")
	svc := newFakeService()
	svc.setPlan("team-a", "vpc-orders", freshVerdict("10.1.0.0/24", "pool1", "d1", 24), nil)
	svc.setAdopt("team-a", "vpc-orders", &domain.Allocation{ID: "alloc_1"}, 202, nil)
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), []string{"apply", path, "--operator", "alice"}, testConfig(), svc, &stdout, &stderr)
	if code != ExitValidation {
		t.Fatalf("want exit %d for a pending adoption, got %d: stderr=%s", ExitValidation, code, stderr.String())
	}
}

func TestMainOutputNeverLeaksASecretLookingValue(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "records.csv", validHeader+"team-a,vpc-orders,vpc,prod,eu-central-1,123456789012,10.1.0.0/24,vpc-0abc,42,,\n")
	svc := newFakeService()
	svc.setPlan("team-a", "vpc-orders", freshVerdict("10.1.0.0/24", "pool1", "d1", 24), nil)
	svc.setAdopt("team-a", "vpc-orders", &domain.Allocation{ID: "alloc_1", CIDR: "10.1.0.0/24"}, 201, nil)
	var stdout, stderr bytes.Buffer
	Main(context.Background(), []string{"apply", path, "--operator", "alice"}, testConfig(), svc, &stdout, &stderr)
	for _, needle := range []string{"IPAM_NETBOX_TOKEN", "postgres://", "IPAM_DATABASE_URL", "token", "password"} {
		if strings.Contains(strings.ToLower(stdout.String()), strings.ToLower(needle)) ||
			strings.Contains(strings.ToLower(stderr.String()), strings.ToLower(needle)) {
			t.Fatalf("output mentions %q, which looks like a credential leak", needle)
		}
	}
}
