package adoptcmd

// Tests for `adopt abandon`, in this package's own house style: a fakeService
// with no ledger, NetBox or cloud observer at all
// (docs/WORK_PLAN.md package H2c). internal/service's own safety rules --
// which allocation states refuse, what the fence/clear/delete sequence does --
// are internal/service's tests to hold (internal/service/abandon_test.go);
// this package tests only flag parsing, exit codes, the JSON report, and that
// every argument reaches the service verbatim.

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mischapogr/platform-ipam/internal/domain"
	"github.com/mischapogr/platform-ipam/internal/service"
)

func abandonReport(deleted, findingResolved bool) *service.AbandonReport {
	return &service.AbandonReport{
		Allocation:      service.AbandonedAllocation{ID: "alloc_1", AllocationKey: "vpc-orders", TenantID: "team-a"},
		Operation:       service.AbandonedOperation{ID: "op_1", Type: "ADOPT", Status: "FAILED"},
		Inventory:       service.AbandonClaim{Claim: "this_operation", NetworkID: "42"},
		Fenced:          true,
		Cleared:         true,
		Deleted:         deleted,
		FindingResolved: findingResolved,
	}
}

func planAbandonReport() *service.AbandonReport {
	return &service.AbandonReport{
		DryRun:     true,
		Allocation: service.AbandonedAllocation{ID: "alloc_1", AllocationKey: "vpc-orders", TenantID: "team-a"},
		Operation:  service.AbandonedOperation{ID: "op_1", Type: "ADOPT", Status: "PENDING"},
		Inventory:  service.AbandonClaim{Claim: "this_operation", NetworkID: "42"},
		WouldDo:    []string{"fence: ...", "clear: ..."},
	}
}

// --- usage errors: exit 2, the service is never touched ---------------------

func TestAbandonMissingAllocationIDIsUsage(t *testing.T) {
	svc := newFakeService()
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), []string{"abandon", "--operator", "alice", "--reason", "wrong record"}, domain.Config{}, svc, &stdout, &stderr)
	if code != ExitUsage {
		t.Fatalf("want exit %d, got %d: stderr=%s", ExitUsage, code, stderr.String())
	}
	if len(svc.abandonCalls)+len(svc.planAbandonCalls) != 0 {
		t.Fatalf("service must not be called for a usage error: abandon=%v planAbandon=%v", svc.abandonCalls, svc.planAbandonCalls)
	}
	if !strings.Contains(stderr.String(), "--allocation-id") {
		t.Fatalf("stderr does not name the missing flag: %q", stderr.String())
	}
}

func TestAbandonMissingOperatorIsUsage(t *testing.T) {
	svc := newFakeService()
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), []string{"abandon", "--allocation-id", "alloc_1", "--reason", "wrong record"}, domain.Config{}, svc, &stdout, &stderr)
	if code != ExitUsage {
		t.Fatalf("want exit %d, got %d: stderr=%s", ExitUsage, code, stderr.String())
	}
	if len(svc.abandonCalls)+len(svc.planAbandonCalls) != 0 {
		t.Fatalf("service must not be called for a usage error")
	}
}

func TestAbandonMissingReasonIsUsage(t *testing.T) {
	svc := newFakeService()
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), []string{"abandon", "--allocation-id", "alloc_1", "--operator", "alice"}, domain.Config{}, svc, &stdout, &stderr)
	if code != ExitUsage {
		t.Fatalf("want exit %d, got %d: stderr=%s", ExitUsage, code, stderr.String())
	}
	if len(svc.abandonCalls)+len(svc.planAbandonCalls) != 0 {
		t.Fatalf("service must not be called for a usage error")
	}
}

func TestAbandonBlankReasonIsUsage(t *testing.T) {
	svc := newFakeService()
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), []string{"abandon", "--allocation-id", "alloc_1", "--operator", "alice", "--reason", "   "}, domain.Config{}, svc, &stdout, &stderr)
	if code != ExitUsage {
		t.Fatalf("want exit %d, got %d: stderr=%s", ExitUsage, code, stderr.String())
	}
	if len(svc.abandonCalls)+len(svc.planAbandonCalls) != 0 {
		t.Fatalf("service must not be called for a blank --reason")
	}
}

func TestAbandonPositionalArgumentIsUsage(t *testing.T) {
	svc := newFakeService()
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(),
		[]string{"abandon", "records.csv", "--allocation-id", "alloc_1", "--operator", "alice", "--reason", "wrong record"},
		domain.Config{}, svc, &stdout, &stderr)
	if code != ExitUsage {
		t.Fatalf("want exit %d for a positional argument, got %d: stderr=%s", ExitUsage, code, stderr.String())
	}
	if len(svc.abandonCalls)+len(svc.planAbandonCalls) != 0 {
		t.Fatalf("service must not be called when a positional argument is present")
	}
}

func TestAbandonUnknownFlagIsUsage(t *testing.T) {
	svc := newFakeService()
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(),
		[]string{"abandon", "--allocation-id", "alloc_1", "--operator", "alice", "--reason", "wrong record", "--bogus", "x"},
		domain.Config{}, svc, &stdout, &stderr)
	if code != ExitUsage {
		t.Fatalf("want exit %d for an unknown flag, got %d: stderr=%s", ExitUsage, code, stderr.String())
	}
	if len(svc.abandonCalls)+len(svc.planAbandonCalls) != 0 {
		t.Fatalf("service must not be called for an unknown flag")
	}
}

// CheckArgs itself (main.go's pre-dependency gate) must reject a bad `adopt
// abandon` invocation the same way, and without needing a working database --
// it is called before cmd/platform-ipam/main.go builds any of that. This
// package cannot assert "no database was opened" directly (there is none to
// open in a unit test), but it can assert CheckArgs's own contract: ok=false
// and the correct exit code, from args alone.
func TestCheckArgsAbandonMissingRequiredFlagsIsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code, ok := CheckArgs([]string{"abandon"}, &stdout, &stderr)
	if ok || code != ExitUsage {
		t.Fatalf("want (usage, false), got (%d, %v): stderr=%s", code, ok, stderr.String())
	}
}

func TestCheckArgsAbandonPositionalArgumentIsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code, ok := CheckArgs([]string{"abandon", "records.csv", "--allocation-id", "a", "--operator", "b", "--reason", "c"}, &stdout, &stderr)
	if ok || code != ExitUsage {
		t.Fatalf("want (usage, false), got (%d, %v): stderr=%s", code, ok, stderr.String())
	}
}

func TestCheckArgsAbandonWellFormedIsOK(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code, ok := CheckArgs([]string{"abandon", "--allocation-id", "a", "--operator", "b", "--reason", "c"}, &stdout, &stderr)
	if !ok || code != ExitOK {
		t.Fatalf("want (ok, true), got (%d, %v): stderr=%s", code, ok, stderr.String())
	}
}

// --- refusals: exit 3 --------------------------------------------------------

func TestAbandonUnknownAllocationExitsValidationWithNoReport(t *testing.T) {
	svc := newFakeService()
	svc.setAbandon("alloc_missing", nil, domain.Err(404, "abandon_unknown_allocation", "no allocation with id alloc_missing exists in the ledger"))
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(),
		[]string{"abandon", "--allocation-id", "alloc_missing", "--operator", "alice", "--reason", "wrong record"},
		domain.Config{}, svc, &stdout, &stderr)
	if code != ExitValidation {
		t.Fatalf("want exit %d, got %d: stderr=%s", ExitValidation, code, stderr.String())
	}
	// No report exists for a refusal at the fence, but stdout is still one JSON
	// document, so a pipeline learns the reason without parsing stderr.
	var document struct {
		Error      *struct{ Code, Message string } `json:"error"`
		Allocation *json.RawMessage                `json:"allocation"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &document); err != nil {
		t.Fatalf("stdout is not one JSON document: %v: %q", err, stdout.String())
	}
	if document.Error == nil || document.Error.Code != "abandon_unknown_allocation" || document.Error.Message == "" {
		t.Fatalf("the document does not carry the refusal: %q", stdout.String())
	}
	if document.Allocation != nil {
		t.Fatalf("a refusal with no report printed report fields: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "abandon_unknown_allocation") {
		t.Fatalf("stderr does not carry the refusal code: %q", stderr.String())
	}
}

func TestAbandonCommittedExitsValidationWithReport(t *testing.T) {
	svc := newFakeService()
	report := abandonReport(false, false)
	report.Allocation.Committed = true
	svc.setAbandon("alloc_1", report, domain.Err(409, "abandon_committed", "allocation alloc_1 is committed"))
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(),
		[]string{"abandon", "--allocation-id", "alloc_1", "--operator", "alice", "--reason", "wrong record"},
		domain.Config{}, svc, &stdout, &stderr)
	if code != ExitValidation {
		t.Fatalf("want exit %d, got %d: stderr=%s", ExitValidation, code, stderr.String())
	}
	var got service.AbandonReport
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout.String())
	}
	if !got.Allocation.Committed {
		t.Fatalf("report was not printed even though the service returned one: %#v", got)
	}
}

func TestAbandon422StatusExitsValidation(t *testing.T) {
	svc := newFakeService()
	svc.setAbandon("alloc_1", nil, domain.Err(422, "abandon_operator_required_hypothetical", "example 422"))
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(),
		[]string{"abandon", "--allocation-id", "alloc_1", "--operator", "alice", "--reason", "wrong record"},
		domain.Config{}, svc, &stdout, &stderr)
	if code != ExitValidation {
		t.Fatalf("want exit %d for a 422 refusal, got %d: stderr=%s", ExitValidation, code, stderr.String())
	}
}

// --- 503s and non-API errors: exit 4, "re-running is safe" ------------------

func TestAbandonUncertainExitsAdapterWithRerunMessage(t *testing.T) {
	svc := newFakeService()
	svc.setAbandon("alloc_1", abandonReport(false, false), domain.Err(503, "abandon_uncertain", "the inventory did not say whether it cleared the network"))
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(),
		[]string{"abandon", "--allocation-id", "alloc_1", "--operator", "alice", "--reason", "wrong record"},
		domain.Config{}, svc, &stdout, &stderr)
	if code != ExitAdapter {
		t.Fatalf("want exit %d, got %d: stderr=%s", ExitAdapter, code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "re-running") {
		t.Fatalf("stderr must say re-running is safe: %q", stderr.String())
	}
	// The report up to the failure is still printed (Cleared/Deleted are
	// false), matching internal/service/abandon.go's own contract that a
	// report is returned beside an error once the fence has succeeded.
	var got service.AbandonReport
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout.String())
	}
}

func TestAbandonDependencyUnavailableExitsAdapter(t *testing.T) {
	svc := newFakeService()
	svc.setAbandon("alloc_1", nil, domain.Err(503, "dependency_unavailable", "inventory adapter is not configured"))
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(),
		[]string{"abandon", "--allocation-id", "alloc_1", "--operator", "alice", "--reason", "wrong record"},
		domain.Config{}, svc, &stdout, &stderr)
	if code != ExitAdapter {
		t.Fatalf("want exit %d, got %d: stderr=%s", ExitAdapter, code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "re-running") {
		t.Fatalf("stderr must say re-running is safe: %q", stderr.String())
	}
}

func TestAbandonNonAPIErrorExitsAdapter(t *testing.T) {
	svc := newFakeService()
	svc.setAbandon("alloc_1", nil, context.DeadlineExceeded)
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(),
		[]string{"abandon", "--allocation-id", "alloc_1", "--operator", "alice", "--reason", "wrong record"},
		domain.Config{}, svc, &stdout, &stderr)
	if code != ExitAdapter {
		t.Fatalf("want exit %d for a non-APIError, got %d: stderr=%s", ExitAdapter, code, stderr.String())
	}
}

// abandon_*_required is the service's own backstop (unreachable through
// CheckArgs, since checkAbandonArgs refuses the same thing first); calling
// Main directly, as this test does, is the one way to reach it, and it must
// still map to ExitUsage rather than ExitValidation.
func TestAbandonServiceRequiredBackstopMapsToUsage(t *testing.T) {
	svc := newFakeService()
	svc.setAbandon("alloc_1", nil, domain.Err(422, "abandon_reason_required", "a reason is required"))
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(),
		[]string{"abandon", "--allocation-id", "alloc_1", "--operator", "alice", "--reason", "wrong record"},
		domain.Config{}, svc, &stdout, &stderr)
	if code != ExitUsage {
		t.Fatalf("want exit %d for the service's own abandon_reason_required backstop, got %d", ExitUsage, code)
	}
}

// --- success: exit 0 ---------------------------------------------------------

func TestAbandonSuccessExitsOKWithDeletedTrue(t *testing.T) {
	svc := newFakeService()
	svc.setAbandon("alloc_1", abandonReport(true, true), nil)
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(),
		[]string{"abandon", "--allocation-id", "alloc_1", "--operator", "alice", "--reason", "wrong record"},
		domain.Config{}, svc, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("want exit %d, got %d: stderr=%s", ExitOK, code, stderr.String())
	}
	var got service.AbandonReport
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout.String())
	}
	if !got.Deleted {
		t.Fatalf("report.deleted must be true: %#v", got)
	}
	if len(svc.planAbandonCalls) != 0 {
		t.Fatalf("a real run must not call PlanAbandonAdoption: %v", svc.planAbandonCalls)
	}
}

// --- --dry-run: PlanAbandonAdoption only, zero AbandonAdoption calls --------

func TestAbandonDryRunCallsOnlyPlanAbandonAdoption(t *testing.T) {
	svc := newFakeService()
	svc.setPlanAbandon("alloc_1", planAbandonReport(), nil)
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(),
		[]string{"abandon", "--allocation-id", "alloc_1", "--operator", "alice", "--reason", "wrong record", "--dry-run"},
		domain.Config{}, svc, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("want exit %d, got %d: stderr=%s", ExitOK, code, stderr.String())
	}
	if len(svc.abandonCalls) != 0 {
		t.Fatalf("--dry-run must never reach Service.AbandonAdoption: %v", svc.abandonCalls)
	}
	if len(svc.planAbandonCalls) != 1 {
		t.Fatalf("want exactly one PlanAbandonAdoption call, got %d", len(svc.planAbandonCalls))
	}
	var got service.AbandonReport
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout.String())
	}
	if !got.DryRun {
		t.Fatalf("report.dry_run must be true: %#v", got)
	}
}

// A dry run that also hits a refusal must still never have called
// AbandonAdoption -- combining --dry-run with any other flag can never reach
// the write path.
func TestAbandonDryRunRefusalStillNeverCallsAbandonAdoption(t *testing.T) {
	svc := newFakeService()
	svc.setPlanAbandon("alloc_1", nil, domain.Err(409, "abandon_committed", "allocation alloc_1 is committed"))
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(),
		[]string{"abandon", "--allocation-id", "alloc_1", "--operator", "alice", "--reason", "wrong record",
			"--operation-id", "op_1", "--dry-run"},
		domain.Config{}, svc, &stdout, &stderr)
	if code != ExitValidation {
		t.Fatalf("want exit %d, got %d: stderr=%s", ExitValidation, code, stderr.String())
	}
	if len(svc.abandonCalls) != 0 {
		t.Fatalf("--dry-run must never reach Service.AbandonAdoption even on a refusal: %v", svc.abandonCalls)
	}
}

// --- arguments reach the service verbatim -----------------------------------

func TestAbandonArgumentsReachTheServiceVerbatim(t *testing.T) {
	svc := newFakeService()
	svc.setAbandon("alloc_1", abandonReport(true, true), nil)
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(),
		[]string{"abandon", "--allocation-id", "alloc_1", "--operator", "alice@example.com",
			"--reason", "reviewed record named the wrong prefix", "--operation-id", "op_7"},
		domain.Config{}, svc, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("want exit %d, got %d: stderr=%s", ExitOK, code, stderr.String())
	}
	if len(svc.abandonCalls) != 1 {
		t.Fatalf("want exactly one AbandonAdoption call, got %d", len(svc.abandonCalls))
	}
	got := svc.abandonCalls[0]
	want := abandonCall{
		allocationID: "alloc_1", operationID: "op_7",
		operator: "alice@example.com", reason: "reviewed record named the wrong prefix",
	}
	if got != want {
		t.Fatalf("arguments did not reach the service verbatim:\n got  %#v\n want %#v", got, want)
	}
}

func TestAbandonOperationIDIsOptional(t *testing.T) {
	svc := newFakeService()
	svc.setAbandon("alloc_1", abandonReport(true, true), nil)
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(),
		[]string{"abandon", "--allocation-id", "alloc_1", "--operator", "alice", "--reason", "wrong record"},
		domain.Config{}, svc, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("want exit %d, got %d: stderr=%s", ExitOK, code, stderr.String())
	}
	if len(svc.abandonCalls) != 1 || svc.abandonCalls[0].operationID != "" {
		t.Fatalf("operation id must be empty when --operation-id is not given: %#v", svc.abandonCalls)
	}
}

// --- no secret in any output -------------------------------------------------

func TestAbandonOutputNeverLeaksASecretLookingValue(t *testing.T) {
	svc := newFakeService()
	svc.setAbandon("alloc_1", abandonReport(true, true), nil)
	svc.setPlanAbandon("alloc_2", planAbandonReport(), nil)
	var stdout, stderr bytes.Buffer
	Main(context.Background(),
		[]string{"abandon", "--allocation-id", "alloc_1", "--operator", "alice", "--reason", "wrong record"},
		domain.Config{}, svc, &stdout, &stderr)
	var stdout2, stderr2 bytes.Buffer
	Main(context.Background(),
		[]string{"abandon", "--allocation-id", "alloc_2", "--operator", "alice", "--reason", "wrong record", "--dry-run"},
		domain.Config{}, svc, &stdout2, &stderr2)
	all := stdout.String() + stderr.String() + stdout2.String() + stderr2.String()
	for _, needle := range []string{"IPAM_NETBOX_TOKEN", "postgres://", "IPAM_DATABASE_URL", "token", "password"} {
		if strings.Contains(strings.ToLower(all), strings.ToLower(needle)) {
			t.Fatalf("output mentions %q, which looks like a credential leak", needle)
		}
	}
}
