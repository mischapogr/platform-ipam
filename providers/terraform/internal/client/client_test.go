package client

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCreateRetriesSameIdempotencyKeyAfterTransientResponse(t *testing.T) {
	t.Setenv("PLATFORM_IPAM_ALLOW_LOCAL_HTTP", "1")
	var calls atomic.Int32
	var keys []string
	server := newTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"code":"dependency_unavailable","message":"try again","retryable":true}}`))
			return
		}
		w.Header().Set("ETag", `"7"`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"alloc_01","allocation_key":"orders-v1","scope":"vpc","state":"RESERVED","cidr":"10.0.0.0/20","revision":7}`))
	}))
	defer server.Close()
	api, err := New(Config{Endpoint: server.URL, HTTPClient: server.Client(), PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	got, err := api.CreateAllocation(context.Background(), AllocationRequest{AllocationKey: "orders-v1", Scope: "vpc", PrefixLength: 20}, "reserve-orders-v1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "alloc_01" || len(keys) != 2 || keys[0] != keys[1] {
		t.Fatalf("allocation=%+v calls=%d keys=%v", got, calls.Load(), keys)
	}
}

func TestCreateRetriesSameKeyAfterLostResponse(t *testing.T) {
	var calls atomic.Int32
	var keys []string
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		if calls.Add(1) == 1 {
			return nil, lostResponseError{}
		}
		return &http.Response{StatusCode: http.StatusCreated, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"alloc_01","allocation_key":"orders-v1","scope":"vpc","state":"RESERVED","cidr":"10.0.0.0/20"}`)), Request: r}, nil
	})
	api, err := New(Config{Endpoint: "https://ipam.example.test", HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := api.CreateAllocation(context.Background(), AllocationRequest{AllocationKey: "orders-v1"}, "reserve-orders-v1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "alloc_01" || len(keys) != 2 || keys[0] != keys[1] {
		t.Fatalf("allocation=%+v calls=%d keys=%v", got, calls.Load(), keys)
	}
}

func TestCreatePollsAsyncOperationThenReadsCommittedAllocation(t *testing.T) {
	t.Setenv("PLATFORM_IPAM_ALLOW_LOCAL_HTTP", "1")
	var polls atomic.Int32
	server := newTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/allocations":
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"op_01","status":"PENDING","allocation_id":"alloc_01"}`))
		case "/v1/operations/op_01":
			if polls.Add(1) == 1 {
				_, _ = w.Write([]byte(`{"id":"op_01","status":"PENDING"}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"op_01","status":"SUCCEEDED","result":{"allocation_id":"alloc_01"}}`))
		case "/v1/allocations/alloc_01":
			_, _ = w.Write([]byte(`{"id":"alloc_01","allocation_key":"orders-v1","scope":"vpc","state":"ACTIVE","cidr":"10.0.0.0/20"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	api, err := New(Config{Endpoint: server.URL, HTTPClient: server.Client(), PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	got, err := api.CreateAllocation(context.Background(), AllocationRequest{AllocationKey: "orders-v1"}, "reserve-orders-v1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "ACTIVE" || polls.Load() != 2 {
		t.Fatalf("allocation=%+v polls=%d", got, polls.Load())
	}
}

// TestCreateSurfacesAFailedOperationWithoutFabricatingAnHTTPStatus is package
// H8c's fix for the two cosmetic defects ADR 0013 named: waitForAllocation's
// FAILED case used to build an HTTPError with no Status set at all, and
// Error() printed it as "HTTP 0", claiming an HTTP failure that never
// happened -- the GET that read the terminal operation succeeded; it is the
// operation itself that did not. This drives the shape a cancelled
// reservation leaves (ADR 0013's reservation_cancelled), the case that
// matters most, and checks that the operation's own code and message reach
// the returned error, and that "HTTP 0" never appears in it.
func TestCreateSurfacesAFailedOperationWithoutFabricatingAnHTTPStatus(t *testing.T) {
	t.Setenv("PLATFORM_IPAM_ALLOW_LOCAL_HTTP", "1")
	server := newTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/allocations":
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"op_01","status":"PENDING","allocation_id":"alloc_01"}`))
		case "/v1/operations/op_01":
			_, _ = w.Write([]byte(`{"id":"op_01","status":"FAILED","allocation_id":"alloc_01","error":{"code":"reservation_cancelled","message":"Reservation cancelled by the tenant that requested it; the uncommitted hold was withdrawn.","request_id":"req_01","retryable":false}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	api, err := New(Config{Endpoint: server.URL, HTTPClient: server.Client(), PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	_, err = api.CreateAllocation(context.Background(), AllocationRequest{AllocationKey: "orders-v1"}, "reserve-orders-v1")
	if err == nil {
		t.Fatal("expected an error for a FAILED operation")
	}
	apiErr, ok := err.(*HTTPError)
	if !ok {
		t.Fatalf("error=%T %v, want *HTTPError", err, err)
	}
	if apiErr.Code != "reservation_cancelled" {
		t.Fatalf("Code=%q, want reservation_cancelled", apiErr.Code)
	}
	if !strings.Contains(apiErr.Message, "withdrawn") {
		t.Fatalf("Message=%q, want the operation's own message", apiErr.Message)
	}
	if apiErr.Status != 0 {
		t.Fatalf("Status=%d, want 0: a terminal operation's failure carries no HTTP status of its own", apiErr.Status)
	}
	if strings.Contains(err.Error(), "HTTP 0") {
		t.Fatalf("Error()=%q must not fabricate an HTTP status that never existed", err.Error())
	}
	if !strings.Contains(err.Error(), "reservation_cancelled") {
		t.Fatalf("Error()=%q, want the operation's own code", err.Error())
	}
}

func TestRead404IsTypedAndPreservedForFailClosedHandling(t *testing.T) {
	t.Setenv("PLATFORM_IPAM_ALLOW_LOCAL_HTTP", "1")
	server := newTestServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "not_found", "message": "not found"}})
	}))
	defer server.Close()
	api, err := New(Config{Endpoint: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = api.GetAllocation(context.Background(), "alloc_missing")
	if apiErr, ok := err.(*HTTPError); !ok || !apiErr.IsStatus(http.StatusNotFound) {
		t.Fatalf("error=%T %v", err, err)
	}
}

// TestReadAllocationToleratesOperatorOnlyFields is package G3b4's evidence
// that this provider tolerates the new operator-only response fields ADR 0011
// stage two adds (tenant_id on Allocation; domain_id, resource_type,
// resource_id and tenant_id on Finding -- none of which this provider models
// at all). doOnce decodes every response with plain json.Unmarshal (no
// json.Decoder.DisallowUnknownFields anywhere in this package), which is
// already tolerant of fields a struct does not declare, so no client change
// was needed; this test is the demonstration, not a fix.
// TestGetAllocationDecodesAllocationPendingDetails is package E3's client
// test: GetAllocation's 409 allocation_pending decodes into an *HTTPError
// carrying the operation id from details.operation_id, which the resource's
// and data source's Read use to name the pending operation in their
// diagnostics rather than losing it in a generic error string.
func TestGetAllocationDecodesAllocationPendingDetails(t *testing.T) {
	t.Setenv("PLATFORM_IPAM_ALLOW_LOCAL_HTTP", "1")
	server := newTestServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"code": "allocation_pending", "message": "still pending", "retryable": true,
			"details": map[string]any{"operation_id": "op_pending_client_01"},
		}})
	}))
	defer server.Close()
	api, err := New(Config{Endpoint: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = api.GetAllocation(context.Background(), "alloc_pending")
	apiErr, ok := err.(*HTTPError)
	if !ok || !apiErr.IsStatus(http.StatusConflict) {
		t.Fatalf("error=%T %v", err, err)
	}
	if apiErr.Code != "allocation_pending" || !apiErr.Retryable {
		t.Fatalf("Code=%q Retryable=%v, want allocation_pending/true", apiErr.Code, apiErr.Retryable)
	}
	if apiErr.Details["operation_id"] != "op_pending_client_01" {
		t.Fatalf("Details=%#v, want operation_id=op_pending_client_01", apiErr.Details)
	}
}

func TestReadAllocationToleratesOperatorOnlyFields(t *testing.T) {
	t.Setenv("PLATFORM_IPAM_ALLOW_LOCAL_HTTP", "1")
	server := newTestServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"alloc_01","allocation_key":"orders-v1","scope":"vpc","state":"RESERVED","cidr":"10.0.0.0/20","revision":7,"tenant_id":"orders-team"}`))
	}))
	defer server.Close()
	api, err := New(Config{Endpoint: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	got, err := api.GetAllocation(context.Background(), "alloc_01")
	if err != nil {
		t.Fatalf("an operator-only field broke decoding: %v", err)
	}
	if got.ID != "alloc_01" || got.CIDR != "10.0.0.0/20" || got.Revision != 7 {
		t.Fatalf("allocation=%+v, want the documented fields decoded despite the unmodeled tenant_id", got)
	}
}

func TestHTTPRequiresExplicitLoopbackOptIn(t *testing.T) {
	t.Setenv("PLATFORM_IPAM_ALLOW_LOCAL_HTTP", "0")
	if _, err := New(Config{Endpoint: "http://127.0.0.1:8080"}); err == nil {
		t.Fatal("expected HTTP endpoint rejection")
	}
}

func newTestServer(handler http.Handler) *httptest.Server {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	server := &httptest.Server{Listener: listener, Config: &http.Server{Handler: handler}}
	server.Start()
	return server
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type lostResponseError struct{}

func (lostResponseError) Error() string   { return "connection reset after commit" }
func (lostResponseError) Timeout() bool   { return true }
func (lostResponseError) Temporary() bool { return true }
