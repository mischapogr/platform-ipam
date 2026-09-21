package transport

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/mischapogr/platform-ipam/internal/domain"
	"github.com/mischapogr/platform-ipam/internal/service"
)

type Backend interface {
	Reserve(context.Context, domain.Principal, domain.Request, string) (*domain.Allocation, *domain.Operation, int, error)
	Get(context.Context, domain.Principal, string) (domain.Allocation, error)
	List(context.Context, domain.Principal) ([]domain.Allocation, error)
	Patch(context.Context, domain.Principal, string, *string, *map[string]string, int64, string) (domain.Allocation, error)
	Bind(context.Context, domain.Principal, string, domain.Binding, string) (*domain.Allocation, *domain.Operation, int, error)
	Release(context.Context, domain.Principal, string) (domain.Allocation, int, error)
	Operation(context.Context, domain.Principal, string) (domain.Operation, error)
	// CancelReservation is Service.CancelReservation (ADR 0013): withdraws the
	// caller's own uncommitted RESERVE once the platform has declared it
	// stuck. cancelOperation below is its one permitted caller --
	// TestServiceCancelReservationsOnlyNonTestCallerIsTheTransport in
	// internal/service/cancel_test.go pins that, never a process-mode
	// command, because ADR 0013 makes this the consumer's own exit and not an
	// operator's. The method name and signature must match
	// *service.Service.CancelReservation exactly for the real service to
	// satisfy this interface.
	CancelReservation(context.Context, domain.Principal, string) (*service.CancelReport, error)
	Pools(context.Context, domain.Principal) ([]domain.Pool, error)
	Capacity(context.Context, domain.Principal, string) (map[string]any, error)
	Findings(context.Context, domain.Principal) ([]domain.Finding, error)
}
type Server struct {
	backend  Backend
	auth     Authenticator
	ledger   domain.Ledger
	cfg      domain.Config
	requests atomic.Uint64
}

func New(backend Backend, auth Authenticator, ledger domain.Ledger, cfg domain.Config) *Server {
	return &Server{backend: backend, auth: auth, ledger: ledger, cfg: cfg}
}
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, r *http.Request) { write(w, 200, map[string]string{"status": "alive"}) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.ledger.Ready(r.Context()); err != nil {
			s.failure(w, r, domain.Err(503, "dependency_unavailable", "Ledger is not ready."))
			return
		}
		write(w, 200, map[string]string{"status": "ready"})
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintf(w, "# TYPE platform_ipam_http_requests_total counter\nplatform_ipam_http_requests_total %d\n", s.requests.Load())
	})
	mux.HandleFunc("POST /v1/allocations", s.secure(s.reserve))
	mux.HandleFunc("GET /v1/allocations", s.secureRead(s.list))
	mux.HandleFunc("GET /v1/allocations/{id}", s.secureRead(s.get))
	mux.HandleFunc("PATCH /v1/allocations/{id}", s.secure(s.patch))
	mux.HandleFunc("DELETE /v1/allocations/{id}", s.secure(s.release))
	mux.HandleFunc("PUT /v1/allocations/{id}/binding", s.secure(s.bind))
	mux.HandleFunc("GET /v1/operations/{id}", s.secureRead(s.operation))
	// A write, not a read (ADR 0013): through secure, not secureRead. An
	// operator is refused by Service.CancelReservation itself, with the same
	// 404 GET /v1/operations/{id} already answers it -- the transport adds no
	// authorization rule of its own here, exactly as the record requires.
	mux.HandleFunc("DELETE /v1/operations/{id}", s.secure(s.cancelOperation))
	mux.HandleFunc("GET /v1/pools", s.secureRead(s.pools))
	mux.HandleFunc("GET /v1/pools/{id}/capacity", s.secureRead(s.capacity))
	mux.HandleFunc("GET /v1/findings", s.secureRead(s.findings))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		w.Header().Set("X-Request-ID", domain.NewID("req"))
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		mux.ServeHTTP(w, r)
	})
}

type endpoint func(http.ResponseWriter, *http.Request, domain.Principal) error

func (s *Server) secure(next endpoint) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := s.auth.Authenticate(r)
		if err == nil {
			err = next(w, r, p)
		}
		if err != nil {
			s.failure(w, r, err)
		}
	}
}

// statusRecorder captures the status code a read handler wrote, so
// secureRead can log it after the fact without changing what the caller
// receives: every method but WriteHeader passes straight through to the
// wrapped http.ResponseWriter.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (rec *statusRecorder) WriteHeader(code int) {
	rec.status = code
	rec.ResponseWriter.WriteHeader(code)
}

// rowsContextKey carries the number of items a list-shaped read endpoint
// returned (page() sets it) out to secureRead's log line. It exists only
// for this: no handler ever reads it back, and it is never used to make an
// authorization decision.
type rowsContextKey struct{}

// withRowsRecorder attaches a rows counter to r's context, defaulting to -1
// ("not applicable" -- a by-id read such as get or capacity has no row
// count, and secureRead omits the "rows" attribute for those rather than
// logging a fabricated 1 or 0).
func withRowsRecorder(r *http.Request) (*http.Request, *int) {
	rows := -1
	return r.WithContext(context.WithValue(r.Context(), rowsContextKey{}, &rows)), &rows
}

// recordRows lets page() report how many items it wrote, without page()
// needing to know whether anyone is listening (a tenant's request carries no
// recorder, and the type assertion below simply no-ops for it).
func recordRows(r *http.Request, n int) {
	if rows, ok := r.Context().Value(rowsContextKey{}).(*int); ok {
		*rows = n
	}
}

// secureRead wraps a read (GET) endpoint exactly as secure does, and
// additionally logs one application-log line per request for an operator
// principal only (ADR 0011 stage two): subject, method, path, status, and --
// for a list endpoint -- the number of rows returned. This is deliberately
// slog at the transport layer, never a ledger write: every Ledger.Update is a
// stop-the-world rewrite of nine tables under one global advisory lock
// (ADR 0010), and turning each operator GET into one would be a denial of
// service against allocation. The line never carries the Authorization
// header or a token value. A tenant's request produces no such line and pays
// no extra cost -- the fast path below is identical to secure's.
func (s *Server) secureRead(next endpoint) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := s.auth.Authenticate(r)
		if err != nil {
			s.failure(w, r, err)
			return
		}
		if !p.IsOperator() {
			if err := next(w, r, p); err != nil {
				s.failure(w, r, err)
			}
			return
		}
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		withRows, rows := withRowsRecorder(r)
		if err := next(rec, withRows, p); err != nil {
			s.failure(rec, withRows, err)
		}
		attrs := []any{"subject", p.Subject, "method", r.Method, "path", r.URL.Path, "status", rec.status}
		if *rows >= 0 {
			attrs = append(attrs, "rows", *rows)
		}
		slog.Info("operator read", attrs...)
	}
}
func (s *Server) failure(w http.ResponseWriter, r *http.Request, err error) {
	var apierr *domain.APIError
	if !errors.As(err, &apierr) {
		slog.Error("request failed", "request_id", w.Header().Get("X-Request-ID"), "method", r.Method, "error_type", fmt.Sprintf("%T", err))
		apierr = domain.Err(503, "dependency_unavailable", "The operation could not be completed; retry with the same identity.")
	}
	status := apierr.Status
	if status < 400 || status > 599 {
		status = 500
	}
	if status == 503 || status == 429 {
		w.Header().Set("Retry-After", "2")
	}
	details := apierr.Details
	if details == nil {
		details = map[string]any{}
	}
	write(w, status, map[string]any{"error": map[string]any{"code": apierr.Code, "message": apierr.Message, "retryable": apierr.Retryable, "details": details, "request_id": w.Header().Get("X-Request-ID")}})
}
func write(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if status != 204 {
		_ = json.NewEncoder(w).Encode(body)
	}
}
func readJSON(w http.ResponseWriter, r *http.Request, out any) error {
	content, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || content != "application/json" {
		return domain.Err(400, "malformed_json", "Content-Type must be application/json.")
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		return domain.Err(422, "invalid_request", "Request must be a JSON object smaller than 1 MiB.")
	}
	check := json.NewDecoder(bytes.NewReader(data))
	first, err := check.Token()
	if err != nil || first != json.Delim('{') {
		return domain.Err(422, "invalid_request", "Expected a JSON object.")
	}
	if err := checkObject(check); err != nil {
		return domain.Err(422, "invalid_request", "JSON fields must be unique and non-null.")
	}
	if _, err = check.Token(); err != io.EOF {
		return domain.Err(400, "malformed_json", "Only one JSON object is permitted.")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return domain.Err(422, "invalid_request", "Expected a JSON object with only documented fields.")
	}
	return nil
}
func checkObject(d *json.Decoder) error {
	keys := map[string]bool{}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return err
		}
		s, ok := key.(string)
		if !ok || keys[s] {
			return fmt.Errorf("duplicate field")
		}
		keys[s] = true
		if err := checkValue(d); err != nil {
			return err
		}
	}
	end, err := d.Token()
	if err != nil {
		return err
	}
	if end != json.Delim('}') {
		return fmt.Errorf("invalid object")
	}
	return nil
}
func checkValue(d *json.Decoder) error {
	v, err := d.Token()
	if err != nil {
		return err
	}
	if v == nil {
		return fmt.Errorf("null field")
	}
	if delim, ok := v.(json.Delim); ok {
		if delim == '{' {
			return checkObject(d)
		}
		if delim == '[' {
			for d.More() {
				if err := checkValue(d); err != nil {
					return err
				}
			}
			end, err := d.Token()
			if err != nil || end != json.Delim(']') {
				return fmt.Errorf("invalid array")
			}
			return nil
		}
		return fmt.Errorf("invalid delimiter")
	}
	return nil
}
func requestKey(r *http.Request) (string, error) {
	k := r.Header.Get("Idempotency-Key")
	if len(k) < 1 || len(k) > 128 {
		return "", domain.Err(400, "idempotency_key_required", "Idempotency-Key must contain 1 to 128 visible ASCII characters.")
	}
	for _, c := range k {
		if c < 33 || c > 126 {
			return "", domain.Err(400, "idempotency_key_required", "Invalid Idempotency-Key.")
		}
	}
	return k, nil
}
func (s *Server) allocation(w http.ResponseWriter, status int, a domain.Allocation, p domain.Principal) {
	w.Header().Set("ETag", fmt.Sprintf("\"%d\"", a.Revision))
	w.Header().Set("Location", "/v1/allocations/"+a.ID)
	write(w, status, s.publicAllocation(a, p))
}
func (s *Server) outcome(w http.ResponseWriter, a *domain.Allocation, o *domain.Operation, status int, p domain.Principal) error {
	if status == 202 && o != nil {
		w.Header().Set("Location", "/v1/operations/"+o.ID)
		w.Header().Set("Retry-After", "2")
		write(w, 202, publicOperation(*o, w.Header().Get("X-Request-ID")))
		return nil
	}
	if a != nil && a.Committed {
		s.allocation(w, status, *a, p)
		return nil
	}
	return domain.Err(503, "dependency_unavailable", "Missing durable operation result.")
}
func (s *Server) reserve(w http.ResponseWriter, r *http.Request, p domain.Principal) error {
	key, err := requestKey(r)
	if err != nil {
		return err
	}
	var in domain.Request
	if err = readJSON(w, r, &in); err != nil {
		return err
	}
	a, o, status, err := s.backend.Reserve(r.Context(), p, in, key)
	if err != nil {
		return err
	}
	return s.outcome(w, a, o, status, p)
}
func (s *Server) get(w http.ResponseWriter, r *http.Request, p domain.Principal) error {
	a, err := s.backend.Get(r.Context(), p, r.PathValue("id"))
	if err != nil {
		return err
	}
	s.allocation(w, 200, a, p)
	return nil
}
func (s *Server) patch(w http.ResponseWriter, r *http.Request, p domain.Principal) error {
	key, err := requestKey(r)
	if err != nil {
		return err
	}
	header := r.Header.Get("If-Match")
	if len(header) < 3 || header[0] != '"' || header[len(header)-1] != '"' {
		return domain.Err(400, "invalid_request", "If-Match must contain the quoted allocation revision.")
	}
	rev, err := strconv.ParseInt(header[1:len(header)-1], 10, 64)
	if err != nil || rev < 1 {
		return domain.Err(400, "invalid_request", "Invalid allocation revision.")
	}
	var body map[string]json.RawMessage
	if err := readJSON(w, r, &body); err != nil {
		return err
	}
	var description *string
	var labels *map[string]string
	if len(body) == 0 {
		return domain.Err(422, "invalid_request", "Supply description and/or labels.")
	}
	for key, value := range body {
		if string(value) == "null" {
			return domain.Err(422, "invalid_request", "Metadata cannot be null.")
		}
		switch key {
		case "description":
			var v string
			if json.Unmarshal(value, &v) != nil {
				return domain.Err(422, "invalid_request", "Invalid description.")
			}
			description = &v
		case "labels":
			var v map[string]string
			if json.Unmarshal(value, &v) != nil {
				return domain.Err(422, "invalid_request", "Invalid labels.")
			}
			labels = &v
		default:
			return domain.Err(422, "invalid_request", "Only description and labels are mutable.")
		}
	}
	a, err := s.backend.Patch(r.Context(), p, r.PathValue("id"), description, labels, rev, key)
	if err != nil {
		return err
	}
	s.allocation(w, 200, a, p)
	return nil
}
func (s *Server) bind(w http.ResponseWriter, r *http.Request, p domain.Principal) error {
	key, err := requestKey(r)
	if err != nil {
		return err
	}
	var in struct {
		Provider     string `json:"provider"`
		ResourceType string `json:"resource_type"`
		ResourceID   string `json:"resource_id"`
		AccountID    string `json:"account_id"`
		Region       string `json:"region"`
	}
	if err = readJSON(w, r, &in); err != nil {
		return err
	}
	a, o, status, err := s.backend.Bind(r.Context(), p, r.PathValue("id"), domain.Binding{Provider: in.Provider, ResourceType: in.ResourceType, ResourceID: in.ResourceID, AccountID: in.AccountID, Region: in.Region}, key)
	if err != nil {
		return err
	}
	return s.outcome(w, a, o, status, p)
}
func (s *Server) release(w http.ResponseWriter, r *http.Request, p domain.Principal) error {
	a, status, err := s.backend.Release(r.Context(), p, r.PathValue("id"))
	if err != nil {
		return err
	}
	if status == 204 {
		write(w, 204, nil)
	} else {
		s.allocation(w, status, a, p)
	}
	return nil
}
func (s *Server) operation(w http.ResponseWriter, r *http.Request, p domain.Principal) error {
	o, err := s.backend.Operation(r.Context(), p, r.PathValue("id"))
	if err != nil {
		return err
	}
	if o.Status == "PENDING" {
		w.Header().Set("Retry-After", "2")
	}
	write(w, 200, publicOperation(o, w.Header().Get("X-Request-ID")))
	return nil
}

// cancelOperation is DELETE /v1/operations/{id} (ADR 0013). It reads no
// request body -- DELETE is inherently idempotent and needs no body or
// idempotency header (docs/API_V1.md section 5), and a body sent with it is
// simply ignored, exactly as release (DELETE /v1/allocations/{id}) already
// ignores one. Every refusal Service.CancelReservation returns -- the
// operator's 404, the committed allocation's 409 cancel_committed, the
// window's 409s, the inventory's 409/503s -- reaches the client through the
// same error writer as every other endpoint (failure, via s.secure), with
// cancelProgressDetails (work-plan package H9) attaching whatever this run's
// own report had already established before it stopped. A success is the
// whole CancelReport, written as the response body so a caller sees exactly
// what the withdrawal did without a second GET.
func (s *Server) cancelOperation(w http.ResponseWriter, r *http.Request, p domain.Principal) error {
	report, err := s.backend.CancelReservation(r.Context(), p, r.PathValue("id"))
	if err != nil {
		return cancelProgressDetails(err, report)
	}
	write(w, 200, report)
	return nil
}

// cancelProgressDetails is work-plan package H9's answer to a gap H8c's
// review left noted but unchanged: a failed cancel can return a
// *service.CancelReport BESIDE its error (internal/service/cancel.go's own
// doc comment on CancelReport -- "a refusal from the fence returns no
// report, because nothing was established, while a removal that failed
// returns the report of everything up to it"), and until now this transport
// discarded it and sent only the error. The narrowest honest fix, following
// pendingAllocationError's precedent (internal/service/service.go, E3 --
// details.operation_id) and adopt abandon's own CLI precedent
// (internal/adoptcmd/abandon.go: the report's fields are printed whenever
// there is one, whatever the outcome): status, code, message and retryable
// are the service's decision and are never touched here; only error.details
// gains fields, and only when report is non-nil -- a refusal the fence
// itself makes (an unknown operation, a committed allocation, the wrong
// operation type, a healthy hold: cancelCheck) returns no report, because
// nothing was yet established to report, and this function changes nothing
// about that answer. Every APIError apiErr and domain.Err construct in
// cancel.go is freshly allocated per call, never a shared value, so mutating
// Details in place here is safe.
func cancelProgressDetails(err error, report *service.CancelReport) error {
	if report == nil {
		return err
	}
	var apierr *domain.APIError
	if !errors.As(err, &apierr) {
		return err
	}
	if apierr.Details == nil {
		apierr.Details = map[string]any{}
	}
	apierr.Details["allocation_id"] = report.Allocation.ID
	apierr.Details["operation_id"] = report.Operation.ID
	apierr.Details["fenced"] = report.Fenced
	apierr.Details["already_fenced"] = report.AlreadyFenced
	apierr.Details["removed"] = report.Removed
	apierr.Details["deleted"] = report.Deleted
	return err
}

func (s *Server) list(w http.ResponseWriter, r *http.Request, p domain.Principal) error {
	list, err := s.backend.List(r.Context(), p)
	if err != nil {
		return err
	}
	items := []map[string]any{}
	allowed := map[string]bool{"allocation_key": true, "scope": true, "environment": true, "region": true, "state": true, "parent_allocation_id": true}
	for k, v := range r.URL.Query() {
		if k != "cursor" && k != "limit" && (!allowed[k] || len(v) != 1) {
			return domain.Err(422, "invalid_request", "Unknown or repeated list filter.")
		}
	}
	for _, a := range list {
		values := map[string]string{"allocation_key": a.AllocationKey, "scope": a.Scope, "environment": a.Environment, "region": a.Region, "state": a.State, "parent_allocation_id": a.ParentAllocationID}
		match := true
		for k := range allowed {
			if v := r.URL.Query().Get(k); v != "" && v != values[k] {
				match = false
			}
		}
		if match {
			items = append(items, s.publicAllocation(a, p))
		}
	}
	return page(w, r, items)
}
func (s *Server) pools(w http.ResponseWriter, r *http.Request, p domain.Principal) error {
	pools, err := s.backend.Pools(r.Context(), p)
	if err != nil {
		return err
	}
	// ADR 0011 stage one: /v1/pools is the entitlement endpoint, and an empty
	// answer is never a legitimate steady state -- the same configuration
	// makes every reservation from this principal fail with 422
	// policy_violation. The decision is taken here, on Service.Pools' full
	// answer, before any pagination: a principal eligible for one or more
	// pools always gets 200, even when a cursor pages past the end of that
	// list; only a principal eligible for NO pool at all meets 403. The
	// refusal names no pool id, name, or count. Every other endpoint
	// (/v1/allocations, /v1/findings) is untouched by this rule: "there is
	// nothing" must remain a fact the API can state.
	if len(pools) == 0 {
		return domain.Err(403, "no_eligible_pool", "The authenticated identity's tenant is eligible for no pool.")
	}
	items := []map[string]any{}
	for _, pool := range pools {
		items = append(items, map[string]any{"id": pool.ID, "scopes": []string{pool.Scope, "subnet"}, "environment": pool.Environment, "region": pool.Region, "allowed_prefix_lengths": pool.AllowedPrefixLengths, "policy_version": s.cfg.PolicyVersion})
	}
	return page(w, r, items)
}
func (s *Server) capacity(w http.ResponseWriter, r *http.Request, p domain.Principal) error {
	v, err := s.backend.Capacity(r.Context(), p, r.PathValue("id"))
	if err != nil {
		return err
	}
	write(w, 200, v)
	return nil
}
func (s *Server) findings(w http.ResponseWriter, r *http.Request, p domain.Principal) error {
	v, err := s.backend.Findings(r.Context(), p)
	if err != nil {
		return err
	}
	items := []map[string]any{}
	for _, f := range v {
		items = append(items, publicFinding(f, p))
	}
	return page(w, r, items)
}
func page(w http.ResponseWriter, r *http.Request, items []map[string]any) error {
	q := r.URL.Query()
	if len(q["cursor"]) > 1 || len(q["limit"]) > 1 {
		return domain.Err(422, "invalid_request", "Repeated pagination arguments.")
	}
	limit := 50
	if raw := q.Get("limit"); raw != "" {
		var err error
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 200 {
			return domain.Err(422, "invalid_request", "limit must be between 1 and 200.")
		}
	}
	cursor := ""
	if raw := q.Get("cursor"); raw != "" {
		b, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil || len(b) > 256 {
			return domain.Err(422, "invalid_request", "Invalid cursor.")
		}
		cursor = string(b)
	}
	sort.Slice(items, func(i, j int) bool { return items[i]["id"].(string) < items[j]["id"].(string) })
	selected := []map[string]any{}
	var next any
	for _, item := range items {
		id := item["id"].(string)
		if id <= cursor {
			continue
		}
		if len(selected) == limit {
			next = base64.RawURLEncoding.EncodeToString([]byte(selected[len(selected)-1]["id"].(string)))
			break
		}
		selected = append(selected, item)
	}
	write(w, 200, map[string]any{"items": selected, "next_cursor": next})
	recordRows(r, len(selected))
	return nil
}
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func (s *Server) publicAllocation(a domain.Allocation, p domain.Principal) map[string]any {
	labels := a.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	blockers := a.ReleaseBlockers
	if blockers == nil {
		blockers = []string{}
	}
	links := map[string]string{"self": "/v1/allocations/" + a.ID}
	if s.cfg.UI.InventoryLinksEnabled && a.InventoryID != "" && a.State != domain.Released {
		links["inventory"] = strings.TrimRight(s.cfg.UI.NetBoxBaseURL, "/") + "/ipam/prefixes/" + url.PathEscape(a.InventoryID) + "/"
	}
	out := map[string]any{"id": a.ID, "allocation_key": a.AllocationKey, "scope": a.Scope, "environment": a.Environment, "region": a.Region, "account_id": a.AccountID, "address_family": a.AddressFamily, "prefix_length": a.PrefixLength, "cidr": a.CIDR, "pool_id": a.PoolID, "parent_allocation_id": nullable(a.ParentAllocationID), "availability_zone_id": nullable(a.AvailabilityZoneID), "description": a.Description, "labels": labels, "state": a.State, "revision": a.Revision, "policy_version": a.PolicyVersion, "binding": a.Binding, "release_requested_at": a.ReleaseRequestedAt, "quarantine_until": a.QuarantineUntil, "release_blockers": blockers, "inventory_sync": a.InventorySync, "last_observed_at": a.LastObservedAt, "created_at": a.CreatedAt, "updated_at": a.UpdatedAt, "links": links}
	// ADR 0011 stage two: tenant_id exists only for an operator, who reads
	// across tenants and needs to tell whose allocation this is. A tenant's
	// own response never gains this key: api/openapi.yaml declares it
	// optional (not required), and an optional property that does not apply
	// is omitted from the object entirely, never sent as null -- null is
	// reserved for a field every caller always receives but that sometimes
	// has no value (see nullable() above). TestPublicAllocationHidesAdapterState
	// and TestTenantAndInternalFieldsWithOperatorConfigured assert the tenant
	// side of this; TestOperatorGrantedReads asserts the operator side.
	if p.IsOperator() {
		out["tenant_id"] = a.TenantID
	}
	return out
}

// publicFinding is Finding's projection, split out from the findings handler
// so the principal-gated fields have one place to live (ADR 0011 stage two).
// A tenant's response is exactly what it always was: id, severity, code,
// allocation_id, account_id, region, the two timestamps and status, and
// never domain_id, resource_type, resource_id or tenant_id. For an operator:
// domain_id is always present (every finding has a domain); resource_type
// and resource_id are present only when the finding carries them -- a
// finding that names an allocation leaves them empty (domain.Finding's
// comment) -- and tenant_id is present only where allocation_id is set. A
// grouped occupancy row (internal/service/service.go collapseOccupancy)
// clears TenantID to "" because it has no one recipient, so for it the key is
// ABSENT, not null and not empty: same optional-property reasoning as
// publicAllocation's tenant_id above, decided once and applied consistently.
func publicFinding(f domain.Finding, p domain.Principal) map[string]any {
	out := map[string]any{"id": f.ID, "severity": f.Severity, "code": f.Code, "allocation_id": nullable(f.AllocationID), "account_id": nullable(f.AccountID), "region": nullable(f.Region), "first_observed_at": f.FirstObservedAt, "last_observed_at": f.LastObservedAt, "status": f.Status}
	if !p.IsOperator() {
		return out
	}
	out["domain_id"] = f.DomainID
	if f.ResourceType != "" {
		out["resource_type"] = f.ResourceType
	}
	if f.ResourceID != "" {
		out["resource_id"] = f.ResourceID
	}
	if f.AllocationID != "" {
		out["tenant_id"] = f.TenantID
	}
	return out
}
func publicOperation(o domain.Operation, requestID string) map[string]any {
	var problem any
	if o.Error != nil {
		details := o.Error.Details
		if details == nil {
			details = map[string]any{}
		}
		problem = map[string]any{"code": o.Error.Code, "message": o.Error.Message, "retryable": o.Error.Retryable, "details": details, "request_id": requestID}
	}
	return map[string]any{"id": o.ID, "type": o.Type, "status": o.Status, "allocation_id": o.AllocationID, "result": o.Result, "error": problem}
}
