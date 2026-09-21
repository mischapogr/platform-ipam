package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mischapogr/platform-ipam/internal/migrate"
)

// withEnv points the client at a test server and restores the environment.
func withEnv(t *testing.T, origin string) {
	t.Helper()
	t.Setenv(envURL, origin)
	t.Setenv(envToken, "test-token")
	t.Setenv(envLocalHTTP, "1")
}

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// The reserve command must refuse to invent an allocation key. A key derived
// from a run number or timestamp is the failure mode the ledger cannot undo.
func TestReserveRequiresExplicitAllocationKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("reserve must not reach the API without an allocation key")
	}))
	defer server.Close()
	withEnv(t, server.URL)

	code, stdout, stderr := runCLI(t, "reserve", "--env", "development", "--region", "eu-central-1", "--prefix-length", "22")
	if code != ExitUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitUsage)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "--key is required") {
		t.Errorf("stderr = %q, want a message naming --key", stderr)
	}
}

// Two invocations with one allocation key must present the same idempotency
// key, so the server treats the second as a replay rather than a second
// request for the same logical identity.
func TestReserveDerivesStableIdempotencyKeyFromAllocationKey(t *testing.T) {
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Idempotency-Key"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"id": "alloc_01", "cidr": "10.64.4.0/22", "state": "RESERVED"})
	}))
	defer server.Close()
	withEnv(t, server.URL)

	args := []string{"reserve", "--key", "dev-euc1-orders", "--env", "development", "--region", "eu-central-1", "--prefix-length", "22"}
	if code, _, stderr := runCLI(t, args...); code != ExitOK {
		t.Fatalf("first reserve exit = %d, stderr = %q", code, stderr)
	}
	if code, _, stderr := runCLI(t, args...); code != ExitOK {
		t.Fatalf("second reserve exit = %d, stderr = %q", code, stderr)
	}
	if len(seen) != 2 {
		t.Fatalf("requests = %d, want 2", len(seen))
	}
	if seen[0] != seen[1] {
		t.Errorf("idempotency keys differ across retries: %q vs %q", seen[0], seen[1])
	}
	if seen[0] == "" || len(seen[0]) > 128 {
		t.Errorf("idempotency key %q must be 1-128 characters", seen[0])
	}
	// A different logical identity must not collide with the first.
	other := append([]string{}, args...)
	other[2] = "dev-euc1-billing"
	if code, _, _ := runCLI(t, other...); code != ExitOK {
		t.Fatalf("third reserve failed")
	}
	if len(seen) != 3 {
		t.Fatalf("requests = %d, want 3", len(seen))
	}
	if seen[2] == seen[0] {
		t.Errorf("distinct allocation keys produced the same idempotency key %q", seen[2])
	}
}

// A 202 is an unresolved durable operation. The client must poll it, and must
// never re-post the reservation.
func TestReservePollsOperationInsteadOfReposting(t *testing.T) {
	var posts, polls atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/allocations", func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]any{"id": "op_01", "status": "PENDING", "allocation_id": "alloc_01"})
	})
	mux.HandleFunc("GET /v1/operations/{id}", func(w http.ResponseWriter, r *http.Request) {
		status := "PENDING"
		if polls.Add(1) >= 2 {
			status = "SUCCEEDED"
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "op_01", "status": status, "allocation_id": "alloc_01"})
	})
	mux.HandleFunc("GET /v1/allocations/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "alloc_01", "cidr": "10.64.8.0/22", "state": "RESERVED"})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	withEnv(t, server.URL)

	code, stdout, stderr := runCLI(t, "reserve", "--key", "dev-euc1-orders",
		"--env", "development", "--region", "eu-central-1", "--prefix-length", "22")
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if got := posts.Load(); got != 1 {
		t.Errorf("reservation posted %d times, want exactly 1", got)
	}
	if got := polls.Load(); got < 2 {
		t.Errorf("operation polled %d times, want at least 2", got)
	}
	if !strings.Contains(stdout, "10.64.8.0/22") {
		t.Errorf("stdout = %q, want the committed CIDR", stdout)
	}
}

// An operation that never settles is not a failure to retry blindly: it still
// owns address space, so it gets its own exit code.
func TestReserveReportsUnresolvedOperationDistinctly(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/allocations", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]any{"id": "op_01", "status": "PENDING"})
	})
	mux.HandleFunc("GET /v1/operations/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "op_01", "status": "PENDING"})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	withEnv(t, server.URL)

	code, stdout, stderr := runCLI(t, "reserve", "--key", "dev-euc1-orders",
		"--env", "development", "--region", "eu-central-1", "--prefix-length", "22",
		"--timeout", "1s")
	if code != ExitPending {
		t.Fatalf("exit = %d, want %d", code, ExitPending)
	}
	if strings.Contains(stdout, "cidr") {
		t.Errorf("stdout = %q, must not report a CIDR for an unresolved operation", stdout)
	}
	if !strings.Contains(stderr, "op_01") {
		t.Errorf("stderr = %q, want the operation id so the caller can inspect it", stderr)
	}
}

// awaitOperation's defect (ADR 0013): it treats every terminal status as
// success-shaped, so on a FAILED reserve it reads operation.AllocationID --
// always present per the schema, on PendingOperation, SucceededOperation and
// FailedOperation alike -- finds it non-empty, and fetches the allocation
// instead of showing the operation's own reason. That mattered most for a
// cancelled reservation: ADR 0013's cancel deletes the allocation row, so the
// fetch answers 404 and the consumer is shown "not found" instead of why
// their reservation failed. This test seeds a FAILED operation whose
// allocation the server answers 404 for -- exactly a cancelled reservation's
// shape -- and is written against today's cli.go, unmodified, to show the
// defect before the fix: it currently FAILS (the allocation is fetched, and
// the operation's own reservation_cancelled reason never reaches stdout).
func TestAwaitOperationShowsAFailedOperationsOwnReasonRatherThanChasingItsGoneAllocation(t *testing.T) {
	var allocationReads atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/allocations", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]any{"id": "op_01", "status": "PENDING", "allocation_id": "alloc_01"})
	})
	mux.HandleFunc("GET /v1/operations/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "op_01", "type": "RESERVE", "status": "FAILED", "allocation_id": "alloc_01", "result": nil,
			"error": map[string]any{"code": "reservation_cancelled", "message": "Reservation cancelled by the tenant that requested it; the uncommitted hold was withdrawn.", "retryable": false, "request_id": "req_01"},
		})
	})
	mux.HandleFunc("GET /v1/allocations/{id}", func(w http.ResponseWriter, r *http.Request) {
		allocationReads.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "not_found", "message": "allocation not found"}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	withEnv(t, server.URL)

	code, stdout, stderr := runCLI(t, "reserve", "--key", "dev-euc1-orders",
		"--env", "development", "--region", "eu-central-1", "--prefix-length", "22")
	if code != ExitAPI {
		t.Fatalf("exit = %d, want %d (a definite failure, distinct from ExitPending); stdout=%q stderr=%q", code, ExitAPI, stdout, stderr)
	}
	if allocationReads.Load() != 0 {
		t.Errorf("GET /v1/allocations/{id} was called %d times; a FAILED operation's own reason must be shown without chasing its allocation", allocationReads.Load())
	}
	if !strings.Contains(stdout, "reservation_cancelled") {
		t.Errorf("stdout = %q, want the terminal operation printed with its own reservation_cancelled code", stdout)
	}
	if strings.Contains(stdout, "not_found") || strings.Contains(stderr, "not_found") {
		t.Errorf("stdout=%q stderr=%q, must not show the allocation's not-found error in place of the operation's own reason", stdout, stderr)
	}
}

// cancel's success path: it sends DELETE with no body, reads no
// Idempotency-Key requirement (there is none for DELETE), and prints the
// whole CancelReport on stdout.
func TestCancelSendsDeleteAndPrintsTheReport(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"at": "2026-09-20T12:00:00Z", "actor": "t:carla",
			"allocation": map[string]any{"id": "alloc_01", "allocation_key": "orders", "tenant_id": "orders-team", "domain_id": "d1", "cidr": "10.64.0.0/22", "state": "RESERVED", "committed": false},
			"operation":  map[string]any{"id": "op_01", "type": "RESERVE", "status": "FAILED"},
			"fenced":     true, "already_fenced": false, "removed": true, "deleted": true, "finding_resolved": true,
		})
	}))
	defer server.Close()
	withEnv(t, server.URL)

	code, stdout, stderr := runCLI(t, "cancel", "--operation-id", "op_01")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, ExitOK, stderr)
	}
	if gotMethod != http.MethodDelete || gotPath != "/v1/operations/op_01" {
		t.Fatalf("method=%s path=%s, want DELETE /v1/operations/op_01", gotMethod, gotPath)
	}
	if gotBody != "" {
		t.Errorf("request body = %q, want none", gotBody)
	}
	if !strings.Contains(stdout, "alloc_01") || !strings.Contains(stdout, "finding_resolved") {
		t.Errorf("stdout = %q, want the cancel report printed", stdout)
	}
}

// A refusal still prints one JSON document -- the server's error object --
// and exits with the API-error code, the house rule package H2c set for a
// withdrawal command's own refusals.
func TestCancelRefusalPrintsTheErrorDocumentAndExitsAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"code": "reservation_not_stuck", "message": "no open reservation_stuck finding names allocation alloc_01", "retryable": false},
		})
	}))
	defer server.Close()
	withEnv(t, server.URL)

	code, stdout, stderr := runCLI(t, "cancel", "--operation-id", "op_01")
	if code != ExitAPI {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, ExitAPI, stderr)
	}
	if !strings.Contains(stdout, "reservation_not_stuck") {
		t.Errorf("stdout = %q, want the refusal's error document printed", stdout)
	}
	if !strings.Contains(stderr, "reservation_not_stuck") {
		t.Errorf("stderr = %q, want the stable error code for a human reader too", stderr)
	}
}

// TestCancelRefusalPrintsProgressDetailsWhenTheServerSendsThem is work-plan
// package H9's CLI proof: cancel already prints the server's whole error
// object verbatim (TestCancelRefusalPrintsTheErrorDocumentAndExitsAPIError
// above proves the passthrough), so a refusal whose error.details carries
// the cancel's own progress -- fenced/already_fenced/removed/deleted and the
// allocation/operation ids, which internal/transport/http.go's
// cancelProgressDetails now attaches for a refusal that has a report behind
// it -- needs no CLI code change to reach the operator. This is the
// passthrough's proof, not a fix, exactly as package E3's
// TestGetOfAPendingAllocationExitsAPIErrorWithTheOperationIDVisible is for get.
func TestCancelRefusalPrintsProgressDetailsWhenTheServerSendsThem(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"code":      "cancel_uncertain",
				"message":   "the inventory did not say whether it removed the network this reservation created",
				"retryable": true,
				"details": map[string]any{
					"allocation_id": "alloc_01", "operation_id": "op_01",
					"fenced": true, "already_fenced": false, "removed": false, "deleted": false,
				},
				"request_id": "req_01",
			},
		})
	}))
	defer server.Close()
	withEnv(t, server.URL)

	code, stdout, stderr := runCLI(t, "cancel", "--operation-id", "op_01")
	if code != ExitAPI {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, ExitAPI, stderr)
	}
	for _, want := range []string{`"fenced": true`, `"already_fenced": false`, `"removed": false`, `"deleted": false`, `"allocation_id": "alloc_01"`, `"operation_id": "op_01"`} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want it to contain %q", stdout, want)
		}
	}
}

// A repeat cancel of one that already finished converges (200, the same
// report) rather than refusing -- exit 0, not a distinct "already done" code.
func TestCancelOfAnAlreadyFinishedWithdrawalConvergesAtExitOK(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"at": "2026-09-20T12:05:00Z", "actor": "t:carla",
			"allocation": map[string]any{"id": "alloc_01", "allocation_key": "orders", "tenant_id": "orders-team", "domain_id": "d1", "cidr": "10.64.0.0/22", "state": "RESERVED", "committed": false},
			"operation":  map[string]any{"id": "op_01", "type": "RESERVE", "status": "FAILED"},
			"fenced":     false, "already_fenced": true, "removed": false, "deleted": true, "finding_resolved": false,
		})
	}))
	defer server.Close()
	withEnv(t, server.URL)

	code, _, stderr := runCLI(t, "cancel", "--operation-id", "op_01")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, ExitOK, stderr)
	}
}

// TestGetOfAPendingAllocationExitsAPIErrorWithTheOperationIDVisible is
// package E3's CLI test, written first against the pre-existing generic
// error path: `get` never special-cased any status code before this
// package (decodeAPIError/Main already route any 4xx/5xx apiError through
// the same branch), so a server answering the new 409 allocation_pending
// needs no CLI code change -- this test is the proof, not a fix. It prints
// nothing on stdout (get, unlike cancel, only writes on success) and its
// stderr names the operation id from details so a human reader can act on
// it without decoding JSON.
func TestGetOfAPendingAllocationExitsAPIErrorWithTheOperationIDVisible(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"code": "allocation_pending", "message": "The allocation is not yet committed; its reservation is still being worked on. Read the named operation.",
				"retryable": true, "details": map[string]any{"operation_id": "op_pending_01"}, "request_id": "req_01",
			},
		})
	}))
	defer server.Close()
	withEnv(t, server.URL)

	code, stdout, stderr := runCLI(t, "get", "--id", "alloc_01")
	if code != ExitAPI {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, ExitAPI, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty: get prints nothing on refusal", stdout)
	}
	if !strings.Contains(stderr, "allocation_pending") {
		t.Errorf("stderr = %q, want the stable error code", stderr)
	}
}

func TestCancelRequiresExplicitOperationID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("cancel must not reach the API without an operation id")
	}))
	defer server.Close()
	withEnv(t, server.URL)

	code, stdout, stderr := runCLI(t, "cancel")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d", code, ExitUsage)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "--operation-id is required") {
		t.Errorf("stderr = %q, want a message naming --operation-id", stderr)
	}
}

// An unauthorized result must be distinguishable from an ordinary API error,
// and the contract's stable error code must reach the operator.
func TestUnauthorizedUsesItsOwnExitCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"code": "unauthenticated", "message": "Invalid token."},
		})
	}))
	defer server.Close()
	withEnv(t, server.URL)

	code, _, stderr := runCLI(t, "get", "--id", "alloc_01")
	if code != ExitAuth {
		t.Fatalf("exit = %d, want %d", code, ExitAuth)
	}
	if !strings.Contains(stderr, "unauthenticated") {
		t.Errorf("stderr = %q, want the API error code", stderr)
	}
}

// The bearer token must never travel over a plaintext non-loopback origin, and
// must not be carried across a redirect.
func TestOriginRulesProtectTheBearerToken(t *testing.T) {
	cases := []struct {
		name       string
		origin     string
		localHTTP  bool
		wantReject bool
	}{
		{name: "https", origin: "https://ipam.example.com", wantReject: false},
		{name: "plaintext remote", origin: "http://ipam.example.com", wantReject: true},
		{name: "plaintext remote with local flag", origin: "http://ipam.example.com", localHTTP: true, wantReject: true},
		{name: "loopback without flag", origin: "http://127.0.0.1:8080", wantReject: true},
		{name: "loopback with flag", origin: "http://127.0.0.1:8080", localHTTP: true, wantReject: false},
		{name: "embedded credentials", origin: "https://user:pass@ipam.example.com", wantReject: true},
		{name: "origin with path", origin: "https://ipam.example.com/v1", wantReject: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkOrigin(tc.origin, tc.localHTTP)
			if tc.wantReject && err == nil {
				t.Errorf("checkOrigin(%q, %v) = nil, want rejection", tc.origin, tc.localHTTP)
			}
			if !tc.wantReject && err != nil {
				t.Errorf("checkOrigin(%q, %v) = %v, want accepted", tc.origin, tc.localHTTP, err)
			}
		})
	}
}

func TestRedirectDoesNotForwardCredentials(t *testing.T) {
	var authOnSecondHop string
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authOnSecondHop = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer final.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL+"/v1/allocations/alloc_01", http.StatusFound)
	}))
	defer server.Close()
	withEnv(t, server.URL)

	runCLI(t, "get", "--id", "alloc_01")
	if authOnSecondHop != "" {
		t.Errorf("Authorization header followed the redirect: %q", authOnSecondHop)
	}
}

// An unknown flag or a stray positional argument is a usage error, not a
// silently ignored input that changes what gets reserved.
func TestUnknownFlagsAndStrayArgumentsAreUsageErrors(t *testing.T) {
	withEnv(t, "https://ipam.example.com")
	for _, args := range [][]string{
		{"reserve", "--key", "k", "--env", "development", "--region", "eu-central-1", "--prefix-length", "22", "--nope"},
		{"get", "--id", "alloc_01", "extra"},
		{"nonsense"},
	} {
		if code, _, _ := runCLI(t, args...); code != ExitUsage {
			t.Errorf("%v exit = %d, want %d", args, code, ExitUsage)
		}
	}
}

// findings must print the server's list envelope unchanged: the verb adds no
// interpretation except the exit code --fail-if-open selects.
func TestFindingsPrintsServerPayloadUnchanged(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{"id": "finding_01", "code": "cloud_occupancy", "severity": "WARNING", "status": "OPEN", "account_id": "000000000000", "region": "eu-central-1"},
				{"id": "finding_02", "code": "coverage_incomplete", "severity": "WARNING", "status": "RESOLVED"},
			},
			"next_cursor": nil,
		})
	}))
	defer server.Close()
	withEnv(t, server.URL)

	code, stdout, stderr := runCLI(t, "findings")
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	var got struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout did not decode as JSON: %v\nstdout: %s", err, stdout)
	}
	if len(got.Items) != 2 {
		t.Fatalf("items = %d, want 2 (the full list, unfiltered)", len(got.Items))
	}
	if got.Items[0]["id"] != "finding_01" || got.Items[0]["status"] != "OPEN" {
		t.Errorf("first item = %v, want finding_01/OPEN preserved unchanged", got.Items[0])
	}
	if got.Items[1]["id"] != "finding_02" || got.Items[1]["status"] != "RESOLVED" {
		t.Errorf("second item = %v, want finding_02/RESOLVED preserved unchanged", got.Items[1])
	}
}

// The findings handler (internal/transport/http.go) reads only cursor and
// limit; findings() must forward exactly those two and nothing invented, such
// as a client-side status or severity filter the API does not implement.
func TestFindingsSendsOnlySupportedQueryParameters(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want url.Values
	}{
		{name: "no flags", args: nil, want: url.Values{}},
		{name: "cursor only", args: []string{"--cursor", "abc123"}, want: url.Values{"cursor": {"abc123"}}},
		{name: "limit only", args: []string{"--limit", "25"}, want: url.Values{"limit": {"25"}}},
		{name: "cursor and limit", args: []string{"--cursor", "abc123", "--limit", "10"}, want: url.Values{"cursor": {"abc123"}, "limit": {"10"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotQuery url.Values
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotQuery = r.URL.Query()
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}, "next_cursor": nil})
			}))
			defer server.Close()
			withEnv(t, server.URL)

			args := append([]string{"findings"}, tc.args...)
			if code, _, stderr := runCLI(t, args...); code != ExitOK {
				t.Fatalf("exit = %d, stderr = %q", code, stderr)
			}
			if len(gotQuery) != len(tc.want) {
				t.Fatalf("query = %v, want %v", gotQuery, tc.want)
			}
			for key, want := range tc.want {
				if got := gotQuery[key]; len(got) != 1 || got[0] != want[0] {
					t.Errorf("query[%q] = %v, want %v", key, got, want)
				}
			}
		})
	}
}

// --fail-if-open must turn "at least one printed finding is OPEN" into the
// distinct ExitFindings code, and must never suppress the printed list. Every
// other status vocabulary observed from the server (internal/service/worker.go,
// service.go: "OPEN"/"RESOLVED") must still exit 0 without the flag, and an
// OPEN finding without the flag must still exit 0 -- the flag is opt-in.
func TestFindingsFailIfOpen(t *testing.T) {
	cases := []struct {
		name         string
		statuses     []string
		failIfOpen   bool
		wantExitCode int
	}{
		{name: "open finding with flag exits ExitFindings", statuses: []string{"OPEN"}, failIfOpen: true, wantExitCode: ExitFindings},
		{name: "mixed statuses with flag exits ExitFindings", statuses: []string{"RESOLVED", "OPEN"}, failIfOpen: true, wantExitCode: ExitFindings},
		{name: "all resolved with flag exits OK", statuses: []string{"RESOLVED", "RESOLVED"}, failIfOpen: true, wantExitCode: ExitOK},
		{name: "empty list with flag exits OK", statuses: []string{}, failIfOpen: true, wantExitCode: ExitOK},
		{name: "open finding without flag exits OK", statuses: []string{"OPEN"}, failIfOpen: false, wantExitCode: ExitOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				// --fail-if-open also queries GET /v1/pools (package G2); this
				// table is only exercising the OPEN/RESOLVED interpretation,
				// so give it one eligible pool regardless of path -- the
				// eligibility interaction itself is covered by
				// TestFindingsFailIfOpenChecksPoolEligibility below.
				if r.URL.Path == "/v1/pools" {
					fmt.Fprint(w, `{"items":[{"id":"pool_dev_euc1"}],"next_cursor":null}`)
					return
				}
				items := []map[string]any{}
				for i, status := range tc.statuses {
					items = append(items, map[string]any{"id": fmt.Sprintf("finding_%02d", i), "code": "x", "severity": "WARNING", "status": status})
				}
				json.NewEncoder(w).Encode(map[string]any{"items": items, "next_cursor": nil})
			}))
			defer server.Close()
			withEnv(t, server.URL)

			args := []string{"findings"}
			if tc.failIfOpen {
				args = append(args, "--fail-if-open")
			}
			code, stdout, stderr := runCLI(t, args...)
			if code != tc.wantExitCode {
				t.Fatalf("exit = %d, want %d; stderr = %q", code, tc.wantExitCode, stderr)
			}
			var got struct {
				Items []map[string]any `json:"items"`
			}
			if err := json.Unmarshal([]byte(stdout), &got); err != nil {
				t.Fatalf("stdout did not decode as JSON: %v\nstdout: %s", err, stdout)
			}
			if len(got.Items) != len(tc.statuses) {
				t.Errorf("printed items = %d, want %d: --fail-if-open must never hide a returned finding", len(got.Items), len(tc.statuses))
			}
		})
	}
}

// An unknown flag on findings is a usage error, matching every other verb.
func TestFindingsUnknownFlagIsUsageError(t *testing.T) {
	withEnv(t, "https://ipam.example.com")
	code, _, _ := runCLI(t, "findings", "--status", "OPEN")
	if code != ExitUsage {
		t.Errorf("exit = %d, want %d", code, ExitUsage)
	}
}

// A 401 from /v1/findings must surface as ExitAuth like every other verb.
func TestFindingsUnauthorizedUsesAuthExitCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"code": "unauthenticated", "message": "Invalid token."},
		})
	}))
	defer server.Close()
	withEnv(t, server.URL)

	code, _, stderr := runCLI(t, "findings")
	if code != ExitAuth {
		t.Fatalf("exit = %d, want %d", code, ExitAuth)
	}
	if !strings.Contains(stderr, "unauthenticated") {
		t.Errorf("stderr = %q, want the API error code", stderr)
	}
}

// The bearer token must not be carried across a redirect on findings either,
// matching TestRedirectDoesNotForwardCredentials for get.
func TestFindingsRedirectDoesNotForwardCredentials(t *testing.T) {
	var authOnSecondHop string
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authOnSecondHop = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer final.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL+"/v1/findings", http.StatusFound)
	}))
	defer server.Close()
	withEnv(t, server.URL)

	runCLI(t, "findings")
	if authOnSecondHop != "" {
		t.Errorf("Authorization header followed the redirect: %q", authOnSecondHop)
	}
}

func TestLabelFlagParsesRepeatedPairs(t *testing.T) {
	labels := labelFlag{}
	if err := labels.Set("service=orders"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := labels.Set("tier=private"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := labels.Set("malformed"); err == nil {
		t.Error("Set(\"malformed\") = nil, want an error")
	}
	if got := labels.String(); got != "service=orders,tier=private" {
		t.Errorf("String() = %q", got)
	}
}

// A gate that reads only the first page passes while an OPEN finding sits on
// the second. Added in review of package E1.
//
// --fail-if-open also queries GET /v1/pools before it judges findings
// (package G2); this test is about findings pagination specifically, so the
// server answers /v1/pools with one eligible pool on its own path and keeps
// counting only the /v1/findings requests it always counted.
func TestFindingsFailIfOpenWalksEveryPage(t *testing.T) {
	var findingsRequests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/pools" {
			fmt.Fprint(w, `{"items":[{"id":"pool_dev_euc1"}],"next_cursor":null}`)
			return
		}
		findingsRequests = append(findingsRequests, r.URL.RawQuery)
		if r.URL.Query().Get("cursor") == "" {
			fmt.Fprint(w, `{"items":[{"id":"f1","status":"RESOLVED"}],"next_cursor":"p2"}`)
			return
		}
		fmt.Fprint(w, `{"items":[{"id":"f2","status":"OPEN"}],"next_cursor":null}`)
	}))
	defer server.Close()
	withEnv(t, server.URL)

	code, stdout, _ := runCLI(t, "findings", "--fail-if-open")
	if code != ExitFindings {
		t.Fatalf("exit = %d, want %d: the OPEN finding is on page two", code, ExitFindings)
	}
	if len(findingsRequests) != 2 {
		t.Fatalf("findings requests = %v, want two pages", findingsRequests)
	}
	if !strings.Contains(stdout, `"f1"`) || !strings.Contains(stdout, `"f2"`) {
		t.Errorf("stdout must carry every page that was judged: %q", stdout)
	}

	// Without the flag the verb stays a single, unchanged request, and
	// /v1/pools is never requested at all (TestFindingsWithoutFailIfOpenNeverRequestsPools
	// below asserts that directly).
	findingsRequests = nil
	if code, _, _ := runCLI(t, "findings"); code != ExitOK {
		t.Fatalf("exit without the flag = %d, want %d", code, ExitOK)
	}
	if len(findingsRequests) != 1 {
		t.Errorf("requests without the flag = %v, want exactly one", findingsRequests)
	}
}

// A payload the gate cannot read is not "no open findings". /v1/pools is
// given a valid, eligible response so this test keeps exercising an
// unreadable *findings* payload specifically; the pools-unreadable case is
// TestFindingsFailIfOpenChecksPoolEligibility below.
func TestFindingsFailIfOpenRefusesUnreadablePayload(t *testing.T) {
	for name, body := range map[string]string{
		"not json":      `<html>gateway error</html>`,
		"no items list": `{"error":"unexpected shape"}`,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/pools" {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"items":[{"id":"pool_dev_euc1"}],"next_cursor":null}`)
					return
				}
				fmt.Fprint(w, body)
			}))
			defer server.Close()
			withEnv(t, server.URL)
			if code, _, _ := runCLI(t, "findings", "--fail-if-open"); code == ExitOK {
				t.Errorf("gate passed on an unreadable payload %q", body)
			}
		})
	}
}

// A server whose pagination loops must not hang a pipeline or pass its gate.
// /v1/pools is given a valid, eligible, non-looping response so this test
// keeps exercising a looping *findings* pagination specifically; the
// pools-loops-its-cursor case is TestFindingsFailIfOpenChecksPoolEligibility
// below.
func TestFindingsFailIfOpenStopsOnRepeatedCursor(t *testing.T) {
	var findingsRequests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/pools" {
			fmt.Fprint(w, `{"items":[{"id":"pool_dev_euc1"}],"next_cursor":null}`)
			return
		}
		findingsRequests.Add(1)
		fmt.Fprint(w, `{"items":[],"next_cursor":"same"}`)
	}))
	defer server.Close()
	withEnv(t, server.URL)
	if code, _, _ := runCLI(t, "findings", "--fail-if-open"); code == ExitOK {
		t.Error("gate passed although pagination never ended")
	}
	// The page cap would also end this walk, a thousand requests later. The
	// repeated cursor has to be what stops it: the first page, then the page
	// that hands the same cursor back.
	if got := findingsRequests.Load(); got != 2 {
		t.Errorf("walk made %d findings requests, want 2", got)
	}
}

// --fail-if-open must not be satisfiable merely by choosing who runs it
// (package G2, docs/WORK_PLAN.md Track G, following ADR 0008's finding that
// every API read is scoped by the caller's tenant). Before judging any
// returned finding it asks GET /v1/pools: Pools filters on eligible_tenants
// and Findings filters on TenantID (internal/service/service.go), so an
// identity whose tenant is eligible for no pool always sees an empty
// findings list, and a clean --fail-if-open pass for it would be
// meaningless.
//
// The "no pools, an open finding returned anyway" case documents and tests
// the precedence decision: ExitNotEligible wins even over a returned OPEN
// finding. That combination should never happen against a correct server --
// Findings and Pools are scoped by the same TenantID -- but if it ever does,
// the eligibility failure is the more fundamental problem with the result,
// so the exit code says "this verdict is untrustworthy" rather than "open
// findings found" (see notEligibleError in cli.go for the full reasoning).
//
// A failure to determine eligibility (transport, 401/403, 5xx, or an
// unreadable/malformed pools payload) is reported with the CLI's existing
// error exit codes, is never ExitOK, is never ExitNotEligible ("no pools"
// must never be inferred from a request that could not be judged), and
// short-circuits before /v1/findings is requested at all.
func TestFindingsFailIfOpenChecksPoolEligibility(t *testing.T) {
	const onePool = `{"items":[{"id":"pool_dev_euc1"}],"next_cursor":null}`
	const noPools = `{"items":[],"next_cursor":null}`

	cases := []struct {
		name                  string
		poolsHandler          http.HandlerFunc
		findingsBody          string
		wantExitCode          int   // checked when wantExitNot is empty
		wantExitNot           []int // checked instead of wantExitCode when non-empty
		wantFindingsRequested bool
	}{
		{
			name:                  "no pools, zero findings",
			poolsHandler:          func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, noPools) },
			findingsBody:          `{"items":[],"next_cursor":null}`,
			wantExitCode:          ExitNotEligible,
			wantFindingsRequested: true,
		},
		{
			name:                  "no pools, an open finding returned anyway (defensive: a server bug)",
			poolsHandler:          func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, noPools) },
			findingsBody:          `{"items":[{"id":"f1","status":"OPEN"}],"next_cursor":null}`,
			wantExitCode:          ExitNotEligible,
			wantFindingsRequested: true,
		},
		{
			name:                  "pools present, nothing open",
			poolsHandler:          func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, onePool) },
			findingsBody:          `{"items":[{"id":"f1","status":"RESOLVED"}],"next_cursor":null}`,
			wantExitCode:          ExitOK,
			wantFindingsRequested: true,
		},
		{
			name:                  "pools present, an open finding",
			poolsHandler:          func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, onePool) },
			findingsBody:          `{"items":[{"id":"f1","status":"OPEN"}],"next_cursor":null}`,
			wantExitCode:          ExitFindings,
			wantFindingsRequested: true,
		},
		{
			name: "pools request fails with a 500",
			poolsHandler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, `{"error":{"code":"internal","message":"boom"}}`)
			},
			wantExitNot:           []int{ExitOK, ExitNotEligible},
			wantFindingsRequested: false,
		},
		{
			name: "pools request is unauthorized",
			poolsHandler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, `{"error":{"code":"unauthenticated","message":"Invalid token."}}`)
			},
			wantExitCode:          ExitAuth,
			wantFindingsRequested: false,
		},
		{
			name:                  "pools payload is not JSON",
			poolsHandler:          func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `<html>gateway error</html>`) },
			wantExitNot:           []int{ExitOK, ExitNotEligible},
			wantFindingsRequested: false,
		},
		{
			name:                  "pools payload has no items list",
			poolsHandler:          func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"error":"unexpected shape"}`) },
			wantExitNot:           []int{ExitOK, ExitNotEligible},
			wantFindingsRequested: false,
		},
		{
			name: "pools paginated: empty first page, a pool on the second",
			poolsHandler: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("cursor") == "" {
					fmt.Fprint(w, `{"items":[],"next_cursor":"p2"}`)
					return
				}
				fmt.Fprint(w, onePool)
			},
			findingsBody:          `{"items":[],"next_cursor":null}`,
			wantExitCode:          ExitOK,
			wantFindingsRequested: true,
		},
		{
			name:                  "pools pagination repeats its cursor",
			poolsHandler:          func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"items":[],"next_cursor":"same"}`) },
			wantExitNot:           []int{ExitOK, ExitNotEligible},
			wantFindingsRequested: false,
		},
		// ADR 0011 stage one: GET /v1/pools now answers 403 no_eligible_pool
		// (rather than 200 with an empty items list) for a caller whose
		// tenant is eligible for no pool. hasEligiblePool must read that one
		// specific 403 as "not eligible" and nothing else.
		{
			name: "pools request is 403 no_eligible_pool (ADR 0011 stage one)",
			poolsHandler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, `{"error":{"code":"no_eligible_pool","message":"The authenticated identity's tenant is eligible for no pool."}}`)
			},
			findingsBody:          `{"items":[],"next_cursor":null}`,
			wantExitCode:          ExitNotEligible,
			wantFindingsRequested: true,
		},
		{
			name: "pools request is 403 no_eligible_pool with an open finding anyway",
			poolsHandler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, `{"error":{"code":"no_eligible_pool","message":"The authenticated identity's tenant is eligible for no pool."}}`)
			},
			findingsBody:          `{"items":[{"id":"f1","status":"OPEN"}],"next_cursor":null}`,
			wantExitCode:          ExitNotEligible,
			wantFindingsRequested: true,
		},
		{
			// A different 403 code (the identity is not onboarded at all, or
			// any other reason) must stay a genuine auth failure, never
			// reinterpreted as "not eligible" and never fetch findings.
			name: "pools request is 403 forbidden, not no_eligible_pool",
			poolsHandler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, `{"error":{"code":"forbidden","message":"Identity is not onboarded to platform-ipam."}}`)
			},
			wantExitCode:          ExitAuth,
			wantFindingsRequested: false,
		},
		{
			// A 403 whose body cannot be decoded (so it carries no code at
			// all) must also stay ExitAuth, never ExitNotEligible: an
			// unreadable body must not be treated as proof of ineligibility.
			name: "pools request is 403 with an unreadable body",
			poolsHandler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, `<html>gateway error</html>`)
			},
			wantExitCode:          ExitAuth,
			wantFindingsRequested: false,
		},
		{
			// The code alone is not the refusal: only the server's 403 is.
			// The same code on a failing status is a failing request, and
			// must never be read as "not eligible".
			name: "pools request is 500 carrying the no_eligible_pool code",
			poolsHandler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, `{"error":{"code":"no_eligible_pool","message":"boom"}}`)
			},
			wantExitNot:           []int{ExitOK, ExitNotEligible},
			wantFindingsRequested: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var findingsRequested bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v1/pools":
					tc.poolsHandler(w, r)
				case "/v1/findings":
					findingsRequested = true
					fmt.Fprint(w, tc.findingsBody)
				default:
					t.Errorf("unexpected request path %q", r.URL.Path)
				}
			}))
			defer server.Close()
			withEnv(t, server.URL)

			code, stdout, stderr := runCLI(t, "findings", "--fail-if-open")
			if len(tc.wantExitNot) > 0 {
				for _, forbidden := range tc.wantExitNot {
					if code == forbidden {
						t.Fatalf("exit = %d, must not be %d; stderr = %q", code, forbidden, stderr)
					}
				}
			} else if code != tc.wantExitCode {
				t.Fatalf("exit = %d, want %d; stderr = %q", code, tc.wantExitCode, stderr)
			}
			if findingsRequested != tc.wantFindingsRequested {
				t.Errorf("findings requested = %v, want %v: a genuine pools error must short-circuit before findings is ever requested", findingsRequested, tc.wantFindingsRequested)
			}
			if tc.wantFindingsRequested {
				var got struct {
					Items []map[string]any `json:"items"`
				}
				if err := json.Unmarshal([]byte(stdout), &got); err != nil {
					t.Fatalf("stdout did not decode as JSON: %v\nstdout: %s", err, stdout)
				}
			} else if stdout != "" {
				t.Errorf("stdout = %q, want empty: findings was never requested so nothing should be printed", stdout)
			}
		})
	}
}

// Without --fail-if-open nothing changes: /v1/pools must never be requested,
// and findings still makes exactly one request. Complements the "without the
// flag" section of TestFindingsFailIfOpenWalksEveryPage with an explicit
// pools-request count.
func TestFindingsWithoutFailIfOpenNeverRequestsPools(t *testing.T) {
	var poolsRequests, findingsRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/pools":
			poolsRequests++
			fmt.Fprint(w, `{"items":[{"id":"pool_dev_euc1"}],"next_cursor":null}`)
		case "/v1/findings":
			findingsRequests++
			fmt.Fprint(w, `{"items":[{"id":"f1","status":"OPEN"}],"next_cursor":null}`)
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
		}
	}))
	defer server.Close()
	withEnv(t, server.URL)

	// An OPEN finding is present, but without --fail-if-open the verb never
	// interprets it and never looks at eligibility.
	code, _, stderr := runCLI(t, "findings")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d; stderr = %q", code, ExitOK, stderr)
	}
	if poolsRequests != 0 {
		t.Errorf("pools requests = %d, want 0: --fail-if-open was not passed", poolsRequests)
	}
	if findingsRequests != 1 {
		t.Errorf("findings requests = %d, want exactly 1", findingsRequests)
	}
}

// The bearer token must be sent on the pools request too, and must not be
// carried across a redirect from it, matching
// TestFindingsRedirectDoesNotForwardCredentials for the findings request
// itself.
func TestFindingsFailIfOpenSendsAuthToPoolsAndDoesNotForwardAcrossRedirect(t *testing.T) {
	var authOnRedirectHop string
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authOnRedirectHop = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer final.Close()

	var poolsAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/pools" {
			poolsAuth = r.Header.Get("Authorization")
			http.Redirect(w, r, final.URL+"/v1/pools", http.StatusFound)
			return
		}
		t.Errorf("unexpected request path %q", r.URL.Path)
	}))
	defer server.Close()
	withEnv(t, server.URL)

	runCLI(t, "findings", "--fail-if-open")
	if poolsAuth == "" {
		t.Error("Authorization header was not sent on the pools request")
	}
	if authOnRedirectHop != "" {
		t.Errorf("Authorization header followed the redirect from the pools request: %q", authOnRedirectHop)
	}
}

// --- evidence (work-plan package M3b4, ADR 0015) ---

// operatorAllocationJSON renders one GET /v1/allocations item as an
// operator would see it: it carries tenant_id (ADR 0011). id/parentID let a
// test exercise buildEvidenceFile's parent_allocation_key resolution.
func operatorAllocationJSON(id, tenantID, key, parentID string) string {
	parent := "null"
	if parentID != "" {
		parent = `"` + parentID + `"`
	}
	return fmt.Sprintf(`{"id":%q,"tenant_id":%q,"allocation_key":%q,"scope":"vpc","environment":"prod","region":"eu-central-1","account_id":"111111111111","prefix_length":16,"parent_allocation_id":%s,"state":"ACTIVE","binding":{"provider":"aws","resource_type":"vpc","resource_id":"vpc-1","account_id":"111111111111","region":"eu-central-1","verified_at":"2026-09-21T00:00:00Z"}}`,
		id, tenantID, key, parent)
}

// tenantAllocationJSON renders one item as an ordinary tenant's own read
// would see it: no tenant_id key at all (docs/API_V1.md section 5: "The key
// is absent -- not null -- from every other caller's response").
func tenantAllocationJSON(id, key string) string {
	return fmt.Sprintf(`{"id":%q,"allocation_key":%q,"scope":"vpc","environment":"prod","region":"eu-central-1","account_id":"111111111111","prefix_length":16,"parent_allocation_id":null,"state":"RESERVED"}`,
		id, key)
}

// TestEvidenceWalksEveryPageAndInfersOperatorScope pins ADR 0015's evidence
// export: every page of GET /v1/allocations is walked (findings's own
// pattern), the scope is inferred as "operator" because every row carries
// tenant_id, and the output decodes through migrate.DecodeEvidence into the
// same rows this command read -- the round trip the record requires.
func TestEvidenceWalksEveryPageAndInfersOperatorScope(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("cursor") == "" {
			fmt.Fprintf(w, `{"items":[%s],"next_cursor":"p2"}`, operatorAllocationJSON("alloc_1", "tenant-a", "alloc-a", ""))
			return
		}
		// Second page's row names alloc_1 as its parent, exercising the
		// parent_allocation_id -> parent_allocation_key resolution against a
		// row read on an EARLIER page.
		fmt.Fprintf(w, `{"items":[%s],"next_cursor":null}`, operatorAllocationJSON("alloc_2", "tenant-b", "alloc-b", "alloc_1"))
	}))
	defer server.Close()
	withEnv(t, server.URL)

	code, stdout, stderr := runCLI(t, "evidence")
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %v, want two pages", requests)
	}

	ev, err := migrate.DecodeEvidence(strings.NewReader(stdout), "stdout")
	if err != nil {
		t.Fatalf("output did not decode through migrate.DecodeEvidence: %v\n%s", err, stdout)
	}
	if ev.Scope != migrate.EvidenceScopeOperator {
		t.Errorf("scope = %q, want %q (inferred: every row carried tenant_id)", ev.Scope, migrate.EvidenceScopeOperator)
	}
	if ev.ReadAt == "" {
		t.Error("read_at was not recorded")
	}
	if len(ev.Allocations) != 2 {
		t.Fatalf("allocations = %d, want 2", len(ev.Allocations))
	}
	byKey := map[string]migrate.EvidenceAllocation{}
	for _, a := range ev.Allocations {
		byKey[a.AllocationKey] = a
	}
	a, ok := byKey["alloc-a"]
	if !ok || a.TenantID != "tenant-a" || a.State != "ACTIVE" || a.BindingVerifiedAt == "" {
		t.Errorf("alloc-a = %+v, ok=%v", a, ok)
	}
	b, ok := byKey["alloc-b"]
	if !ok || b.TenantID != "tenant-b" {
		t.Errorf("alloc-b = %+v, ok=%v", b, ok)
	}
	if b.ParentAllocationKey != "alloc-a" {
		t.Errorf("alloc-b's parent_allocation_key = %q, want %q (resolved from alloc_1's own row)", b.ParentAllocationKey, "alloc-a")
	}
}

// TestEvidenceTenantReadRequiresExplicitScope pins the scope rule's other
// half: an ordinary tenant's own read carries tenant_id on NO row (the
// contract never sends it to a non-operator caller, docs/API_V1.md section
// 5), so this command cannot infer which tenant it is and must refuse
// rather than guess.
func TestEvidenceTenantReadRequiresExplicitScope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"items":[%s],"next_cursor":null}`, tenantAllocationJSON("alloc_1", "alloc-a"))
	}))
	defer server.Close()
	withEnv(t, server.URL)

	code, stdout, stderr := runCLI(t, "evidence")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d (no tenant_id on any row, no --scope given); stderr = %q", code, ExitUsage, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty: no file should be produced on a refusal", stdout)
	}
	if !strings.Contains(stderr, "--scope") {
		t.Errorf("stderr = %q, want it to name --scope", stderr)
	}
}

// TestEvidenceTenantReadWithExplicitScopeFillsEveryRow pins the mitigation:
// with --scope <tenant-id>, every row is exported carrying that tenant id,
// because a tenant's own read of GET /v1/allocations already returns only
// that tenant's own allocations.
func TestEvidenceTenantReadWithExplicitScopeFillsEveryRow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"items":[%s],"next_cursor":null}`, tenantAllocationJSON("alloc_1", "alloc-a"))
	}))
	defer server.Close()
	withEnv(t, server.URL)

	code, stdout, stderr := runCLI(t, "evidence", "--scope", "tenant-a")
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	ev, err := migrate.DecodeEvidence(strings.NewReader(stdout), "stdout")
	if err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout)
	}
	if ev.Scope != "tenant-a" {
		t.Errorf("scope = %q, want tenant-a", ev.Scope)
	}
	if len(ev.Allocations) != 1 || ev.Allocations[0].TenantID != "tenant-a" {
		t.Errorf("allocations = %+v, want one row carrying tenant_id=tenant-a", ev.Allocations)
	}
}

// TestEvidenceOutputIsSortedRegardlessOfServerOrder pins determinism: the
// server returns allocations in an order that is already the OPPOSITE of
// tenant/key order, so a mutant that dropped buildEvidenceFile's own sort
// (relying on the server to have returned rows in order) is caught even
// though TestEvidenceWalksEveryPageAndInfersOperatorScope's two pages
// happen to arrive pre-sorted.
func TestEvidenceOutputIsSortedRegardlessOfServerOrder(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"items":[%s,%s],"next_cursor":null}`,
			operatorAllocationJSON("alloc_z", "tenant-z", "alloc-z", ""),
			operatorAllocationJSON("alloc_a", "tenant-a", "alloc-a", ""))
	}))
	defer server.Close()
	withEnv(t, server.URL)

	code, stdout, stderr := runCLI(t, "evidence")
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	ev, err := migrate.DecodeEvidence(strings.NewReader(stdout), "stdout")
	if err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout)
	}
	if len(ev.Allocations) != 2 || ev.Allocations[0].TenantID != "tenant-a" || ev.Allocations[1].TenantID != "tenant-z" {
		t.Errorf("allocations = %+v, want sorted tenant-a before tenant-z regardless of server order", ev.Allocations)
	}
}

// TestEvidenceRejectsInconsistentScopeClaims covers both directions of a
// claimed scope disagreeing with what the read actually returned: never
// silently trust the flag over the evidence, or the evidence over the flag.
func TestEvidenceRejectsInconsistentScopeClaims(t *testing.T) {
	t.Run("scope operator but a row has no tenant_id", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"items":[%s],"next_cursor":null}`, tenantAllocationJSON("alloc_1", "alloc-a"))
		}))
		defer server.Close()
		withEnv(t, server.URL)
		if code, stdout, _ := runCLI(t, "evidence", "--scope", "operator"); code != ExitUsage || stdout != "" {
			t.Errorf("exit = %d, stdout = %q, want %d and empty", code, stdout, ExitUsage)
		}
	})
	t.Run("scope tenant-a but every row has tenant_id", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"items":[%s],"next_cursor":null}`, operatorAllocationJSON("alloc_1", "tenant-a", "alloc-a", ""))
		}))
		defer server.Close()
		withEnv(t, server.URL)
		if code, stdout, _ := runCLI(t, "evidence", "--scope", "tenant-a"); code != ExitUsage || stdout != "" {
			t.Errorf("exit = %d, stdout = %q, want %d and empty", code, stdout, ExitUsage)
		}
	})
	t.Run("no rows at all, no scope given", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"items":[],"next_cursor":null}`)
		}))
		defer server.Close()
		withEnv(t, server.URL)
		if code, stdout, _ := runCLI(t, "evidence"); code != ExitUsage || stdout != "" {
			t.Errorf("exit = %d, stdout = %q, want %d and empty", code, stdout, ExitUsage)
		}
	})
	// Mixed rows -- one carrying tenant_id, one not -- with no --scope given
	// at all: this must refuse rather than infer either answer, catching a
	// mutant that would infer "operator" from >=1 tenant_id row instead of
	// requiring EVERY row to carry one.
	t.Run("mixed tenant_id, no scope given", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"items":[%s,%s],"next_cursor":null}`,
				operatorAllocationJSON("alloc_1", "tenant-a", "alloc-a", ""), tenantAllocationJSON("alloc_2", "alloc-b"))
		}))
		defer server.Close()
		withEnv(t, server.URL)
		if code, stdout, _ := runCLI(t, "evidence"); code != ExitUsage || stdout != "" {
			t.Errorf("exit = %d, stdout = %q, want %d and empty", code, stdout, ExitUsage)
		}
	})
}

// TestEvidenceRefusesUnreadablePayload mirrors
// TestFindingsFailIfOpenRefusesUnreadablePayload: a page this client cannot
// read must never become an evidence file with the missing rows silently
// absent, and nothing must be printed.
func TestEvidenceRefusesUnreadablePayload(t *testing.T) {
	for name, body := range map[string]string{
		"not json":      `<html>gateway error</html>`,
		"no items list": `{"error":"unexpected shape"}`,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, body)
			}))
			defer server.Close()
			withEnv(t, server.URL)
			code, stdout, _ := runCLI(t, "evidence")
			// ExitTransport specifically: a read failure (bad JSON, or an
			// items list this client cannot trust) is a transport-level
			// problem, distinct from ExitUsage (which a scope problem
			// produces for an otherwise well-formed, empty response) -- a
			// mutant that silently treats an unreadable page as "zero rows
			// read" would still land on ExitUsage (no rows -> ambiguous
			// scope) rather than surfacing the read failure itself, so this
			// assertion is deliberately exact rather than merely "not OK".
			if code != ExitTransport {
				t.Errorf("exit = %d, want %d (ExitTransport) on an unreadable payload %q", code, ExitTransport, body)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty: no partial file on a read failure", stdout)
			}
		})
	}
}

// TestEvidenceUnauthorizedUsesAuthExitCode mirrors
// TestFindingsUnauthorizedUsesAuthExitCode: a 401 on the very first page
// must surface as ExitAuth, never as an empty, "successful" export.
func TestEvidenceUnauthorizedUsesAuthExitCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"code":"unauthenticated","message":"no valid token"}}`)
	}))
	defer server.Close()
	withEnv(t, server.URL)

	code, stdout, _ := runCLI(t, "evidence")
	if code != ExitAuth {
		t.Fatalf("exit = %d, want %d", code, ExitAuth)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
}

// TestEvidenceOutFlagWritesTheSameBytesStdoutCarries pins the atomic write:
// --out receives exactly the bytes stdout already carries.
func TestEvidenceOutFlagWritesTheSameBytesStdoutCarries(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"items":[%s],"next_cursor":null}`, operatorAllocationJSON("alloc_1", "tenant-a", "alloc-a", ""))
	}))
	defer server.Close()
	withEnv(t, server.URL)

	outPath := filepath.Join(t.TempDir(), "evidence.json")
	code, stdout, stderr := runCLI(t, "evidence", "--out", outPath)
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	fileBytes, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading --out file: %v", err)
	}
	if string(fileBytes) != stdout {
		t.Errorf("--out bytes differ from stdout\n--out:  %q\nstdout: %q", fileBytes, stdout)
	}

	// No temp file is left behind: writeFileAtomic's os.Rename must have
	// moved it into place, not left a copy alongside it.
	entries, err := os.ReadDir(filepath.Dir(outPath))
	if err != nil {
		t.Fatalf("reading --out directory: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "evidence.json" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("--out directory contains %v, want exactly [evidence.json] (no leftover temp file)", names)
	}
}

// TestEvidenceOutFlagToUnwritableDirectoryFailsWithoutPartialFile pins
// "never a partial file on error" for the --out path itself: a destination
// directory that cannot be written to must fail the whole command (stdout
// still carries nothing on this path, since --out is checked after the
// buffered document already exists) and must leave no file at outPath.
func TestEvidenceOutFlagToUnwritableDirectoryFailsWithoutPartialFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"items":[%s],"next_cursor":null}`, operatorAllocationJSON("alloc_1", "tenant-a", "alloc-a", ""))
	}))
	defer server.Close()
	withEnv(t, server.URL)

	outPath := filepath.Join(t.TempDir(), "does-not-exist", "evidence.json")
	code, _, stderr := runCLI(t, "evidence", "--out", outPath)
	if code == ExitOK {
		t.Fatalf("exit = OK writing to a nonexistent directory; stderr = %q", stderr)
	}
	if _, err := os.Stat(outPath); err == nil {
		t.Errorf("a file was created at %s despite the write failing", outPath)
	}
}

// TestEvidenceUnknownFlagIsUsageError mirrors
// TestUnknownFlagsAndStrayArgumentsAreUsageErrors for this verb.
func TestEvidenceUnknownFlagIsUsageError(t *testing.T) {
	withEnv(t, "https://example.invalid")
	if code, _, _ := runCLI(t, "evidence", "--bogus"); code != ExitUsage {
		t.Errorf("exit = %d, want %d", code, ExitUsage)
	}
}

func TestMainHelpTextNamesEvidence(t *testing.T) {
	if !strings.Contains(helpText, "evidence") {
		t.Error("help text does not mention the evidence command")
	}
}

// A write that fails after the temporary file was created leaves nothing
// behind: the directory holds neither the document nor a stray temporary.
func TestEvidenceOutFlagLeavesNoTemporaryWhenTheRenameFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"items":[%s],"next_cursor":null}`, operatorAllocationJSON("alloc_1", "tenant-a", "alloc-a", ""))
	}))
	defer server.Close()
	withEnv(t, server.URL)

	// The target path is a directory, so the temporary file is created beside
	// it and the rename onto it fails.
	dir := t.TempDir()
	outPath := filepath.Join(dir, "evidence.json")
	if err := os.Mkdir(outPath, 0o755); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runCLI(t, "evidence", "--out", outPath)
	if code == ExitOK {
		t.Fatalf("exit = OK renaming onto a directory; stderr = %q", stderr)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "evidence.json" {
			t.Fatalf("a file was left behind after the failed write: %s", e.Name())
		}
	}
}
