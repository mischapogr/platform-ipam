// Package cli implements the first-party consumer client described in
// ADR 0003. It is a transport client only: it holds no allocation policy, does
// not select CIDRs or pools, and never infers tenant, account, or role. The
// API owns all of that. What the client does own are the consumer-side
// obligations of the v1 contract — a caller-supplied stable allocation key, a
// derived idempotency key, polling a 202 operation instead of re-posting, and
// never printing a CIDR that is not committed.
package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mischapogr/platform-ipam/internal/migrate"
)

// Exit codes are part of the interface: a pipeline has to tell an outright
// failure from an operation that has not settled, because the second case
// still owns address space.
const (
	ExitOK        = 0
	ExitUsage     = 2
	ExitAuth      = 3
	ExitPending   = 4
	ExitAPI       = 5
	ExitTransport = 6
	// ExitFindings is returned by `findings --fail-if-open` when the list it
	// already printed contains at least one OPEN finding. It is the one piece
	// of interpretation this verb performs; it never hides or filters output.
	ExitFindings = 7
	// ExitNotEligible is returned by `findings --fail-if-open` when the
	// caller's tenant is eligible for no pool: GET /v1/pools returned no
	// items on any page. Every API read is scoped by the caller's tenant
	// (internal/service/service.go's Pools filters on eligible_tenants,
	// Findings on TenantID -- see docs/WORK_PLAN.md Track G and ADR 0008), so
	// such an identity's findings list is always empty and a clean verdict
	// over it proves nothing. It takes priority over both ExitOK and
	// ExitFindings -- see notEligibleError -- but never over a genuine
	// transport or API error.
	ExitNotEligible = 8
)

const defaultTimeout = 10 * time.Minute

// Env names match the Python example and the provider so a developer moving
// between them does not have to relearn the credential source.
const (
	envURL       = "PLATFORM_IPAM_URL"
	envToken     = "PLATFORM_IPAM_TOKEN"
	envLocalHTTP = "PLATFORM_IPAM_ALLOW_LOCAL_HTTP"
)

type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

// apiError carries the server's problem envelope so the caller sees the
// contract's stable error code rather than a transport-level guess.
type apiError struct {
	status  int
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *apiError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("HTTP %d", e.status)
	}
	return fmt.Sprintf("HTTP %d %s: %s", e.status, e.Code, e.Message)
}

// pendingError means the operation reached its deadline without a terminal
// status. The reservation is not lost; it is unresolved, and retrying with the
// same allocation key resumes the same identity.
type pendingError struct{ operationID string }

func (e *pendingError) Error() string {
	return fmt.Sprintf("operation %s did not reach a terminal state before the deadline; retry with the same allocation key", e.operationID)
}

// findingsOpenError signals that `findings --fail-if-open` found at least one
// OPEN finding in a list that was already printed in full. It is not a
// transport or API failure -- the request succeeded -- so it carries its own
// exit code rather than one of the generic failure codes.
type findingsOpenError struct{ count int }

func (e *findingsOpenError) Error() string {
	if e.count == 1 {
		return "1 open finding"
	}
	return fmt.Sprintf("%d open findings", e.count)
}

// notEligibleError means `findings --fail-if-open` printed a findings list
// under an identity whose tenant is eligible for no pool (GET /v1/pools
// returned no items on any page). Every API read is scoped by the caller's
// tenant, so such an identity's findings list is always empty by
// construction; a clean pass over it does not mean there is no drift, it
// means the gate was never able to see any. It takes priority over a clean
// pass (ExitOK) and, deliberately, over ExitFindings too: if pools ever
// reports no eligible pool while findings somehow reports an OPEN item (a
// defensive case -- that combination should not occur against a correct
// server), the exit code says "this verdict is untrustworthy" rather than
// "open findings found", because the eligibility failure is the more
// fundamental problem with the result. It never wins over a genuine
// transport or API error: if GET /v1/pools or GET /v1/findings itself
// fails, that failure is reported with the CLI's existing error exit codes,
// exactly as it always has been.
type notEligibleError struct{}

func (e *notEligibleError) Error() string {
	return "this identity's tenant is eligible for no pool, so its findings list is always empty and a passing verdict over it proves nothing; run --fail-if-open under an identity that belongs to the tenant being checked"
}

// Main runs one client invocation and returns the process exit code. Output
// intended for consumption is written to stdout as JSON; everything else goes
// to stderr.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	err := dispatch(ctx, args, stdout)
	if err == nil {
		return ExitOK
	}
	var usage *usageError
	if errors.As(err, &usage) {
		fmt.Fprintf(stderr, "platform-ipam client: %s\n\n%s", usage.msg, helpText)
		return ExitUsage
	}
	var notEligible *notEligibleError
	if errors.As(err, &notEligible) {
		fmt.Fprintf(stderr, "platform-ipam client: %v\n", err)
		return ExitNotEligible
	}
	var open *findingsOpenError
	if errors.As(err, &open) {
		fmt.Fprintf(stderr, "platform-ipam client: %v\n", err)
		return ExitFindings
	}
	fmt.Fprintf(stderr, "platform-ipam client: %v\n", err)
	var pending *pendingError
	if errors.As(err, &pending) {
		return ExitPending
	}
	// A terminal operation that did not succeed (ADR 0013's awaitOperation
	// fix): no HTTP status ever accompanied it, since the read that surfaced
	// it succeeded, so it is its own exit family rather than apiError's.
	var opFailed *operationFailedError
	if errors.As(err, &opFailed) {
		return ExitAPI
	}
	var api *apiError
	if errors.As(err, &api) {
		if api.status == http.StatusUnauthorized || api.status == http.StatusForbidden {
			return ExitAuth
		}
		return ExitAPI
	}
	return ExitTransport
}

const helpText = `usage: platform-ipam client <command> [flags]

commands:
  reserve    reserve a network for a stable allocation key
  get        read one allocation by id
  list       list authorized allocations
  capacity   read remaining capacity for a pool
  bind       submit an AWS resource candidate for verification
  release    request release and quarantine of an allocation
  findings   list authorized drift and coverage findings
  cancel     withdraw your own pending reservation the platform has declared
             stuck (DELETE /v1/operations/{operation_id}, ADR 0013); refused
             unless an open reservation_stuck finding names the allocation,
             and never for a committed allocation -- release that instead
  evidence   export an allocation evidence file (walks every page of
             GET /v1/allocations, refusing an unreadable one, exactly as
             findings does): the JSON platform-ipam onboard progress
             --allocations expects (ADR 0015)

environment:
  PLATFORM_IPAM_URL              API origin (required)
  PLATFORM_IPAM_TOKEN            bearer token (required)
  PLATFORM_IPAM_ALLOW_LOCAL_HTTP set to 1 to permit a loopback http:// origin

exit codes:
  0 success   2 usage   3 unauthorized   4 operation unresolved
  5 api error 6 transport error   7 findings: at least one OPEN (--fail-if-open)
  8 findings --fail-if-open: caller's tenant is eligible for no pool
`

func dispatch(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return usagef("a command is required")
	}
	command, rest := args[0], args[1:]
	switch command {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, helpText)
		return nil
	case "reserve", "get", "list", "capacity", "bind", "release", "findings", "cancel", "evidence":
	default:
		return usagef("unknown command %q", command)
	}
	client, err := newClientFromEnv()
	if err != nil {
		return err
	}
	switch command {
	case "reserve":
		return client.reserve(ctx, rest, stdout)
	case "get":
		return client.get(ctx, rest, stdout)
	case "list":
		return client.list(ctx, rest, stdout)
	case "capacity":
		return client.capacity(ctx, rest, stdout)
	case "bind":
		return client.bind(ctx, rest, stdout)
	case "release":
		return client.release(ctx, rest, stdout)
	case "findings":
		return client.findings(ctx, rest, stdout)
	case "cancel":
		return client.cancel(ctx, rest, stdout)
	case "evidence":
		return client.evidence(ctx, rest, stdout)
	}
	return usagef("unknown command %q", command)
}

type client struct {
	origin string
	token  string
	http   *http.Client
}

func newClientFromEnv() (*client, error) {
	origin := os.Getenv(envURL)
	if origin == "" {
		return nil, usagef("%s is required", envURL)
	}
	token := os.Getenv(envToken)
	if token == "" {
		return nil, usagef("%s is required", envToken)
	}
	if strings.ContainsAny(token, " \t\r\n") {
		return nil, usagef("%s must not contain whitespace", envToken)
	}
	if err := checkOrigin(origin, os.Getenv(envLocalHTTP) == "1"); err != nil {
		return nil, err
	}
	return &client{
		origin: strings.TrimRight(origin, "/"),
		token:  token,
		http: &http.Client{
			// A bearer token must not follow a redirect to another origin.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// checkOrigin refuses anything that could leak the bearer token: a plaintext
// non-loopback origin, embedded credentials, or a path/query the caller
// believes is part of the API root.
func checkOrigin(origin string, allowLocalHTTP bool) error {
	parsed, err := url.Parse(origin)
	if err != nil {
		return usagef("invalid %s: %v", envURL, err)
	}
	if parsed.Host == "" || parsed.Hostname() == "" {
		return usagef("%s must include a host", envURL)
	}
	if parsed.User != nil {
		return usagef("%s must not embed credentials", envURL)
	}
	if (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return usagef("%s must be a bare origin", envURL)
	}
	switch parsed.Scheme {
	case "https":
		return nil
	case "http":
		if allowLocalHTTP && isLoopback(parsed.Hostname()) {
			return nil
		}
		return usagef("%s must use https, or be a loopback origin with %s=1", envURL, envLocalHTTP)
	default:
		return usagef("%s must use https", envURL)
	}
}

func isLoopback(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

func (c *client) do(ctx context.Context, method, path string, body any, headers map[string]string) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.origin+path, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	if resp.StatusCode >= 400 {
		return resp.StatusCode, payload, decodeAPIError(resp.StatusCode, payload)
	}
	return resp.StatusCode, payload, nil
}

func decodeAPIError(status int, payload []byte) error {
	var envelope struct {
		Error apiError `json:"error"`
	}
	if json.Unmarshal(payload, &envelope) == nil && envelope.Error.Code != "" {
		envelope.Error.status = status
		return &envelope.Error
	}
	return &apiError{status: status}
}

// idempotencyKey derives a stable request key from the tenant-scoped
// allocation key. Two invocations with the same allocation key are a replay,
// not a second reservation; the hash keeps the key inside the contract's
// 1-128 visible ASCII range regardless of the caller's key.
func idempotencyKey(operation, allocationKey string) string {
	sum := sha256.Sum256([]byte(operation + "\x00" + allocationKey))
	return operation + "-" + hex.EncodeToString(sum[:16])
}

type labelFlag map[string]string

func (l labelFlag) String() string {
	if len(l) == 0 {
		return ""
	}
	keys := make([]string, 0, len(l))
	for key := range l {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, key := range keys {
		pairs = append(pairs, key+"="+l[key])
	}
	return strings.Join(pairs, ",")
}

func (l labelFlag) Set(value string) error {
	name, content, found := strings.Cut(value, "=")
	if !found || name == "" {
		return fmt.Errorf("expected key=value, got %q", value)
	}
	l[name] = content
	return nil
}

func newFlagSet(name string) *flag.FlagSet {
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	set.SetOutput(io.Discard)
	return set
}

func parseFlags(set *flag.FlagSet, args []string) error {
	if err := set.Parse(args); err != nil {
		return usagef("%s: %v", set.Name(), err)
	}
	if set.NArg() > 0 {
		return usagef("%s: unexpected argument %q", set.Name(), set.Arg(0))
	}
	return nil
}

func (c *client) reserve(ctx context.Context, args []string, stdout io.Writer) error {
	set := newFlagSet("reserve")
	key := set.String("key", "", "permanent allocation key (required)")
	scope := set.String("scope", "vpc", "vpc or subnet")
	environment := set.String("env", "", "authorized environment (required)")
	region := set.String("region", "", "onboarded AWS region (required)")
	account := set.String("account", "", "authorized 12-digit AWS account id")
	family := set.String("address-family", "", "address family")
	prefixLength := set.Int("prefix-length", 0, "requested prefix length (required)")
	parent := set.String("parent", "", "parent allocation id, for subnet scope")
	zone := set.String("az-id", "", "availability zone id, for subnet scope")
	description := set.String("description", "", "operator description")
	timeout := set.Duration("timeout", defaultTimeout, "deadline for resolving a pending operation")
	labels := labelFlag{}
	set.Var(labels, "label", "label as key=value; repeatable")
	if err := parseFlags(set, args); err != nil {
		return err
	}
	// The CLI never invents an allocation key. An accidental key -- a run
	// number, a timestamp, a hostname -- is precisely the failure this system
	// exists to prevent, so the caller has to decide it deliberately.
	if *key == "" {
		return usagef("reserve: --key is required and must be a stable logical identity")
	}
	if *environment == "" || *region == "" {
		return usagef("reserve: --env and --region are required")
	}
	if *prefixLength <= 0 {
		return usagef("reserve: --prefix-length is required")
	}

	body := map[string]any{
		"allocation_key": *key,
		"scope":          *scope,
		"environment":    *environment,
		"region":         *region,
		"prefix_length":  *prefixLength,
		"description":    *description,
		"labels":         map[string]string(labels),
	}
	if *account != "" {
		body["account_id"] = *account
	}
	if *family != "" {
		body["address_family"] = *family
	}
	if *parent != "" {
		body["parent_allocation_id"] = *parent
	}
	if *zone != "" {
		body["availability_zone_id"] = *zone
	}

	deadline, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	status, payload, err := c.do(deadline, http.MethodPost, "/v1/allocations", body,
		map[string]string{"Idempotency-Key": idempotencyKey("reserve", *key)})
	if err != nil {
		return err
	}
	if status != http.StatusAccepted {
		return writeJSON(stdout, payload)
	}
	// 202 is an unresolved durable operation. Poll it; never re-post the
	// reservation, which would be a second request for one logical identity.
	return c.awaitOperation(deadline, payload, stdout)
}

func (c *client) awaitOperation(ctx context.Context, accepted []byte, stdout io.Writer) error {
	var operation struct {
		ID           string `json:"id"`
		Status       string `json:"status"`
		AllocationID string `json:"allocation_id"`
	}
	if err := json.Unmarshal(accepted, &operation); err != nil || operation.ID == "" {
		return fmt.Errorf("accepted response did not contain an operation id")
	}
	backoff := 500 * time.Millisecond
	for {
		if terminal(operation.Status) {
			break
		}
		select {
		case <-ctx.Done():
			return &pendingError{operationID: operation.ID}
		case <-time.After(backoff):
		}
		if backoff < 5*time.Second {
			backoff *= 2
		}
		_, payload, err := c.do(ctx, http.MethodGet, "/v1/operations/"+url.PathEscape(operation.ID), nil, nil)
		if err != nil {
			var api *apiError
			// A transient dependency failure is not proof the operation is
			// gone; keep polling until the deadline decides.
			if errors.As(err, &api) && api.status >= 500 {
				continue
			}
			return err
		}
		if err := json.Unmarshal(payload, &operation); err != nil {
			return err
		}
		if terminal(operation.Status) {
			if !strings.EqualFold(operation.Status, "SUCCEEDED") {
				// A FAILED (or otherwise non-SUCCEEDED terminal) operation
				// carries its own reason in .error, and that is the answer --
				// not a fetch of the allocation_id every terminal operation
				// carries regardless of outcome (ADR 0013). Fetching it used
				// to be the defect: a cancelled reservation's allocation row
				// is gone by the time this polls, so the fetch answered 404
				// and hid the operation's own reservation_cancelled reason
				// behind a generic not-found. Print the operation as-is and
				// exit on its own error rather than a fabricated one.
				if err := writeJSON(stdout, payload); err != nil {
					return err
				}
				return operationError(operation.ID, payload)
			}
			if operation.AllocationID == "" {
				return writeJSON(stdout, payload)
			}
			// Print the committed allocation rather than the operation, so a
			// successful reserve has one output shape whatever path it took.
			_, allocation, err := c.do(ctx, http.MethodGet, "/v1/allocations/"+url.PathEscape(operation.AllocationID), nil, nil)
			if err != nil {
				return err
			}
			return writeJSON(stdout, allocation)
		}
	}
	return nil
}

// operationFailedError reports a terminal operation that did not succeed,
// carrying its own durable code and message rather than any HTTP status --
// there is none, since the operation failed asynchronously and this read of
// it succeeded. Main() maps it to ExitAPI, the same exit family a synchronous
// refusal of the same request would have produced.
type operationFailedError struct {
	operationID, Code, Message string
}

func (e *operationFailedError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("operation %s did not succeed", e.operationID)
	}
	return fmt.Sprintf("operation %s did not succeed: %s: %s", e.operationID, e.Code, e.Message)
}

// operationError decodes the terminal operation's own error object (present
// on every non-SUCCEEDED terminal operation the contract defines) into an
// operationFailedError. A payload this client cannot decode still produces
// one, with an empty code and message, rather than a false success.
func operationError(operationID string, payload []byte) error {
	var envelope struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(payload, &envelope)
	out := &operationFailedError{operationID: operationID}
	if envelope.Error != nil {
		out.Code, out.Message = envelope.Error.Code, envelope.Error.Message
	}
	return out
}

func terminal(status string) bool {
	switch strings.ToUpper(status) {
	case "SUCCEEDED", "FAILED", "CANCELLED", "CANCELED":
		return true
	}
	return false
}

func (c *client) get(ctx context.Context, args []string, stdout io.Writer) error {
	set := newFlagSet("get")
	id := set.String("id", "", "allocation id (required)")
	if err := parseFlags(set, args); err != nil {
		return err
	}
	if *id == "" {
		return usagef("get: --id is required")
	}
	_, payload, err := c.do(ctx, http.MethodGet, "/v1/allocations/"+url.PathEscape(*id), nil, nil)
	if err != nil {
		return err
	}
	return writeJSON(stdout, payload)
}

func (c *client) list(ctx context.Context, args []string, stdout io.Writer) error {
	set := newFlagSet("list")
	key := set.String("key", "", "filter by allocation key")
	scope := set.String("scope", "", "filter by scope")
	environment := set.String("env", "", "filter by environment")
	region := set.String("region", "", "filter by region")
	state := set.String("state", "", "filter by lifecycle state")
	parent := set.String("parent", "", "filter by parent allocation id")
	if err := parseFlags(set, args); err != nil {
		return err
	}
	query := url.Values{}
	for name, value := range map[string]string{
		"allocation_key":       *key,
		"scope":                *scope,
		"environment":          *environment,
		"region":               *region,
		"state":                *state,
		"parent_allocation_id": *parent,
	} {
		if value != "" {
			query.Set(name, value)
		}
	}
	path := "/v1/allocations"
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	_, payload, err := c.do(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return err
	}
	return writeJSON(stdout, payload)
}

func (c *client) capacity(ctx context.Context, args []string, stdout io.Writer) error {
	set := newFlagSet("capacity")
	pool := set.String("pool", "", "pool id (required)")
	if err := parseFlags(set, args); err != nil {
		return err
	}
	if *pool == "" {
		return usagef("capacity: --pool is required")
	}
	_, payload, err := c.do(ctx, http.MethodGet, "/v1/pools/"+url.PathEscape(*pool)+"/capacity", nil, nil)
	if err != nil {
		return err
	}
	return writeJSON(stdout, payload)
}

// findings lists authorized drift and coverage findings (GET /v1/findings).
// The only query parameters the API declares for this path are cursor and
// limit (api/openapi.yaml, internal/transport/http.go's page()); the handler
// itself reads no other filter, so the client forwards nothing else. The
// full list the server returns is always printed. --fail-if-open is the
// interpretation this verb performs: after printing, it turns "at least one
// returned finding has status OPEN" into a distinct exit code so a pipeline
// can gate on drift. It filters nothing and hides nothing.
//
// A gate that can be satisfied by choosing who runs it is not a gate. Every
// API read is scoped by the caller's tenant (Pools filters on
// eligible_tenants, Findings on TenantID -- internal/service/service.go), so
// an identity whose tenant is eligible for no pool always sees an empty
// findings list and --fail-if-open would pass regardless of what is
// actually true. --fail-if-open therefore first asks GET /v1/pools -- never
// printed, since the pools list is evidence for the verdict, not CLI output
// -- and reports ExitNotEligible instead of a clean pass when the caller's
// tenant sees no pool at all. See notEligibleError for exactly which exit
// code wins when eligibility and open findings disagree.
func (c *client) findings(ctx context.Context, args []string, stdout io.Writer) error {
	set := newFlagSet("findings")
	cursor := set.String("cursor", "", "pagination cursor from a previous page's next_cursor")
	limit := set.Int("limit", 0, "maximum items per page (server default 50, max 200)")
	failIfOpen := set.Bool("fail-if-open", false, "exit with a distinct code if any returned finding has status OPEN; first checks GET /v1/pools (not printed -- evidence for the verdict, not output) and exits with the ExitNotEligible code instead if the caller's tenant is eligible for no pool, since a verdict over an always-empty findings list proves nothing")
	if err := parseFlags(set, args); err != nil {
		return err
	}
	query := url.Values{}
	if *cursor != "" {
		query.Set("cursor", *cursor)
	}
	if *limit != 0 {
		query.Set("limit", strconv.Itoa(*limit))
	}
	path := "/v1/findings"
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	if !*failIfOpen {
		_, payload, err := c.do(ctx, http.MethodGet, path, nil, nil)
		if err != nil {
			return err
		}
		return writeJSON(stdout, payload)
	}

	// The gate has to know who it is judging before it judges anything: an
	// identity eligible for no pool can only ever see an empty findings
	// list. A failure here (transport, 401/403, 5xx, or a pools payload
	// this client cannot read) is a genuine error, reported with the CLI's
	// existing error exit codes -- never a pass, and never treated as "no
	// pools" -- and it short-circuits before /v1/findings is requested at
	// all.
	eligible, err := c.hasEligiblePool(ctx)
	if err != nil {
		return err
	}

	// A gate has to see everything it is judging. The server pages findings
	// (50 by default), so an OPEN finding on the second page would let a gate
	// that read only the first one pass. With --fail-if-open the verb follows
	// next_cursor to the end and prints every page exactly as the server sent
	// it, one JSON document after another, whether or not the caller turns
	// out to be eligible for any pool -- the printed output never changes.
	// An unreadable page fails the gate: a verdict over data that could not
	// be read is not a clean verdict.
	open := 0
	err = c.walkPages(ctx, "findings", path,
		func(cursor string) string {
			query.Set("cursor", cursor)
			return "/v1/findings?" + query.Encode()
		},
		func(payload []byte) (next string, stop bool, err error) {
			if err := writeJSON(stdout, payload); err != nil {
				return "", false, err
			}
			count, next, err := readFindingsPage(payload)
			if err != nil {
				return "", false, fmt.Errorf("cannot judge findings: %w", err)
			}
			open += count
			return next, false, nil
		})
	if err != nil {
		return err
	}

	// Eligibility wins over both a clean pass and an OPEN finding -- see
	// notEligibleError for why.
	if !eligible {
		return &notEligibleError{}
	}
	if open > 0 {
		return &findingsOpenError{count: open}
	}
	return nil
}

// hasEligiblePool asks GET /v1/pools, walking its pagination the same way
// --fail-if-open walks findings, to answer one yes/no question: does the
// caller's tenant see at least one pool? It stops requesting further pages
// the instant one item is seen -- the gate only needs presence, not the full
// list -- and it never prints the response: the pools list is evidence for
// the --fail-if-open verdict, never CLI output. A page this client cannot
// read is an error, not "not eligible": an unreadable pools payload must
// never be treated as proof of ineligibility.
//
// ADR 0011 stage one changed how the server states "not eligible": GET
// /v1/pools now answers 403 no_eligible_pool for a caller whose tenant is
// eligible for no pool, rather than 200 with an empty items list. A 403
// whose error code is exactly "no_eligible_pool" -- and nothing else -- is
// therefore also read as "not eligible", never as a request failure. Any
// other error, including a 403 with a different code (forbidden: the
// identity is not onboarded at all), a 403 whose body this client cannot
// decode, or any non-403 failure, is a genuine error and is returned as one:
// it must never be reinterpreted as "not eligible" and must never let the
// caller reach a false ExitOK.
func (c *client) hasEligiblePool(ctx context.Context) (bool, error) {
	found := false
	query := url.Values{}
	err := c.walkPages(ctx, "pool eligibility", "/v1/pools",
		func(cursor string) string {
			query.Set("cursor", cursor)
			return "/v1/pools?" + query.Encode()
		},
		func(payload []byte) (next string, stop bool, err error) {
			count, next, err := readPoolsPage(payload)
			if err != nil {
				return "", false, fmt.Errorf("cannot judge pool eligibility: %w", err)
			}
			if count > 0 {
				found = true
				return "", true, nil
			}
			return next, false, nil
		})
	if err != nil {
		var api *apiError
		if errors.As(err, &api) && api.status == http.StatusForbidden && api.Code == "no_eligible_pool" {
			return false, nil
		}
		return false, err
	}
	return found, nil
}

// maxPaginationPages bounds any page walk this client performs -- the
// --fail-if-open findings walk and the pool-eligibility check that precedes
// it -- so a server whose pagination never ends cannot hang a pipeline.
const maxPaginationPages = 1000

// walkPages walks a cursor-paginated list endpoint sharing the shape of
// internal/transport/http.go's page() helper (an "items" array and a
// "next_cursor"). It requests firstPath, then repeatedly calls handlePage
// with each page's raw payload; handlePage reports that page's next cursor
// (empty at the end of the list) and whether the walk should stop early
// without requesting any further page. nextPath computes the path for the
// next request from the cursor handlePage returned. label names the walk in
// error messages.
//
// walkPages enforces the two structural safety checks any walk over an
// untrusted server's pagination needs, regardless of what it is walking for:
// it refuses if the walk exceeds maxPaginationPages pages, and if a
// next_cursor repeats one already seen in this walk -- either could
// otherwise hang a pipeline or let a gate return a false verdict.
func (c *client) walkPages(ctx context.Context, label, firstPath string, nextPath func(cursor string) string, handlePage func(payload []byte) (next string, stop bool, err error)) error {
	seen := map[string]bool{}
	path := firstPath
	for page := 0; ; page++ {
		if page >= maxPaginationPages {
			return fmt.Errorf("%s did not end after %d pages; refusing to report a verdict", label, maxPaginationPages)
		}
		_, payload, err := c.do(ctx, http.MethodGet, path, nil, nil)
		if err != nil {
			return err
		}
		next, stop, err := handlePage(payload)
		if err != nil {
			return err
		}
		if stop || next == "" {
			return nil
		}
		if seen[next] {
			return fmt.Errorf("%s pagination repeated cursor %q; refusing to report a verdict", label, next)
		}
		seen[next] = true
		path = nextPath(next)
	}
}

// readPoolsPage reads only what hasEligiblePool needs from one page of
// GET /v1/pools: how many items it carries, and where the next page is. Like
// readFindingsPage, a page without an items list is an error, not zero
// pools.
func readPoolsPage(payload []byte) (count int, next string, err error) {
	var envelope struct {
		Items      *[]json.RawMessage `json:"items"`
		NextCursor *string            `json:"next_cursor"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return 0, "", fmt.Errorf("pools response is not valid JSON: %w", err)
	}
	if envelope.Items == nil {
		return 0, "", errors.New("pools response has no items list")
	}
	if envelope.NextCursor != nil {
		next = *envelope.NextCursor
	}
	return len(*envelope.Items), next, nil
}

// readFindingsPage reads only what --fail-if-open needs from one page: how
// many items are OPEN, and where the next page is. It reinterprets no
// severity and drops nothing from what was already printed. A page without an
// items list is an error, not zero findings.
func readFindingsPage(payload []byte) (open int, next string, err error) {
	var envelope struct {
		Items *[]struct {
			Status string `json:"status"`
		} `json:"items"`
		NextCursor *string `json:"next_cursor"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return 0, "", fmt.Errorf("findings response is not valid JSON: %w", err)
	}
	if envelope.Items == nil {
		return 0, "", errors.New("findings response has no items list")
	}
	for _, item := range *envelope.Items {
		if strings.EqualFold(item.Status, "OPEN") {
			open++
		}
	}
	if envelope.NextCursor != nil {
		next = *envelope.NextCursor
	}
	return open, next, nil
}

func (c *client) bind(ctx context.Context, args []string, stdout io.Writer) error {
	set := newFlagSet("bind")
	id := set.String("id", "", "allocation id (required)")
	provider := set.String("provider", "aws", "cloud provider")
	resourceType := set.String("resource-type", "", "resource type, for example vpc (required)")
	resourceID := set.String("resource-id", "", "cloud resource id (required)")
	account := set.String("account", "", "AWS account id (required)")
	region := set.String("region", "", "AWS region (required)")
	timeout := set.Duration("timeout", defaultTimeout, "deadline for resolving verification")
	if err := parseFlags(set, args); err != nil {
		return err
	}
	if *id == "" || *resourceType == "" || *resourceID == "" || *account == "" || *region == "" {
		return usagef("bind: --id, --resource-type, --resource-id, --account and --region are required")
	}
	body := map[string]any{
		"provider":      *provider,
		"resource_type": *resourceType,
		"resource_id":   *resourceID,
		"account_id":    *account,
		"region":        *region,
	}
	deadline, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	status, payload, err := c.do(deadline, http.MethodPut, "/v1/allocations/"+url.PathEscape(*id)+"/binding", body,
		map[string]string{"Idempotency-Key": idempotencyKey("bind", *id+"/"+*resourceID)})
	if err != nil {
		return err
	}
	if status != http.StatusAccepted {
		return writeJSON(stdout, payload)
	}
	return c.awaitOperation(deadline, payload, stdout)
}

func (c *client) release(ctx context.Context, args []string, stdout io.Writer) error {
	set := newFlagSet("release")
	id := set.String("id", "", "allocation id (required)")
	if err := parseFlags(set, args); err != nil {
		return err
	}
	if *id == "" {
		return usagef("release: --id is required")
	}
	status, payload, err := c.do(ctx, http.MethodDelete, "/v1/allocations/"+url.PathEscape(*id), nil, nil)
	if err != nil {
		return err
	}
	// 204 already RELEASED carries no body; report the outcome rather than
	// printing nothing at all.
	if status == http.StatusNoContent || len(bytes.TrimSpace(payload)) == 0 {
		return writeJSON(stdout, []byte(`{"id":`+mustQuote(*id)+`,"state":"RELEASED"}`))
	}
	return writeJSON(stdout, payload)
}

// cancel is DELETE /v1/operations/{operation_id} (ADR 0013): withdraw the
// caller's own pending reservation once the platform has declared it stuck.
// Unlike every other verb in this file, it prints one JSON document on
// stdout for BOTH outcomes -- the CancelReport's fields on success, the
// server's error object on a refusal -- the house rule package H2c set for a
// withdrawal's own command (internal/adoptcmd/abandon.go: "the command now
// always prints one JSON document -- the report's fields when there is a
// report, an error object when there is an error"), so tooling driving a
// cancel gets a machine-readable reason even when it fails. It needs no
// request body or Idempotency-Key: DELETE is inherently idempotent
// (docs/API_V1.md section 5), and a repeat of a cancel that already finished
// converges on the same report rather than refusing, which is what makes
// exit 0 the right code for that case and exit 5 the right code for every
// refusal (Main's existing apiError handling, unchanged here).
func (c *client) cancel(ctx context.Context, args []string, stdout io.Writer) error {
	set := newFlagSet("cancel")
	id := set.String("operation-id", "", "operation id to cancel (required)")
	if err := parseFlags(set, args); err != nil {
		return err
	}
	if *id == "" {
		return usagef("cancel: --operation-id is required")
	}
	_, payload, err := c.do(ctx, http.MethodDelete, "/v1/operations/"+url.PathEscape(*id), nil, nil)
	if len(bytes.TrimSpace(payload)) > 0 {
		if writeErr := writeJSON(stdout, payload); writeErr != nil {
			return writeErr
		}
	}
	return err
}

// evidence is the allocation evidence export ADR 0015 ("Tenancy,
// authentication and the allocation evidence") assigns to this package:
// "the evidence export must do what findings does -- walk every page,
// refuse an unreadable payload -- and additionally record the read instant
// and the principal's scope." Its output is exactly migrate.EvidenceFile's
// JSON, so `platform-ipam onboard progress --allocations <this file>` reads
// it unchanged.
//
// Unlike findings --fail-if-open, which prints one JSON document PER PAGE
// as it walks (each page is itself valid output), this command's output is
// ONE document -- read_at, scope and the full allocations list -- so every
// page has to be collected before anything is written. That buffering is
// also what makes "never a partial file on error" free: nothing is written
// to stdout or --out until every page has been read successfully, so a
// walk that fails partway leaves no output at all rather than a half
// document.
//
// Scope. ADR 0015 names two ways to obtain it: infer it, or change the
// contract so the server states it. This command takes the first, and only
// as far as the read alone can support: an operator's own read of
// GET /v1/allocations carries tenant_id on every returned row -- ADR 0011,
// "no other caller receives that field" -- so when every row this walk saw
// carries a non-empty tenant_id, scope "operator" is inferred without being
// told. A TENANT's own read carries tenant_id on NO row at all, and there
// is nothing in the transport that tells a caller its own tenant id (the
// API "never accept[s] tenant_id ... as authorization", docs/API_V1.md
// section 3) -- so for that case this command cannot infer the answer and
// does not guess one. The caller must pass --scope <tenant-id> naming the
// tenant this credential belongs to; every exported row's tenant_id is then
// filled from that flag, which is safe because a tenant's read of
// GET /v1/allocations already returns only that tenant's own allocations
// (internal/service/service.go's own TenantID filter). The consequence,
// spelled out in docs/MIGRATION_PROGRESS.md: run this command under a
// tenant credential without --scope and it refuses (a usage error) rather
// than export a file with an empty or wrong scope -- a wrong scope would
// silently change a later `onboard progress` run's target facts (ADR 0015's
// "unknown", never "none", for evidence that could not have seen a tenant)
// into the wrong answer instead of a visible one. --scope operator is
// accepted explicitly too, and is cross-checked against what the read
// actually returned: a row missing tenant_id under a claimed operator scope
// refuses rather than exporting a row this command cannot honestly attest
// to.
func (c *client) evidence(ctx context.Context, args []string, stdout io.Writer) error {
	set := newFlagSet("evidence")
	scopeFlag := set.String("scope", "",
		"operator, or the tenant id this credential exports for; required whenever the read's own rows carry no tenant_id to infer it from (an ordinary tenant credential's read never carries one -- see the scope rule in docs/MIGRATION_PROGRESS.md)")
	outFlag := set.String("out", "",
		"additionally write the same bytes to this path, atomically (temp file then rename); stdout always carries them")
	if err := parseFlags(set, args); err != nil {
		return err
	}

	// The read instant is recorded before the walk starts: this is an
	// export of what a read observed, not a report generated after the
	// fact, and ADR 0015 is explicit that "the wall clock is right here."
	readAt := time.Now().UTC().Format(time.RFC3339)

	var rows []evidenceRow
	query := url.Values{}
	err := c.walkPages(ctx, "evidence", "/v1/allocations",
		func(cursor string) string {
			query.Set("cursor", cursor)
			return "/v1/allocations?" + query.Encode()
		},
		func(payload []byte) (next string, stop bool, err error) {
			pageRows, next, err := readAllocationsPage(payload)
			if err != nil {
				return "", false, fmt.Errorf("cannot export allocation evidence: %w", err)
			}
			rows = append(rows, pageRows...)
			return next, false, nil
		})
	if err != nil {
		return err
	}

	scope, err := resolveEvidenceScope(*scopeFlag, rows)
	if err != nil {
		return err
	}

	file := buildEvidenceFile(readAt, scope, rows)
	encoded, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')

	// Both destinations receive the SAME already-fully-buffered bytes: a
	// failed write to one never leaves a truncated document behind, because
	// nothing was written before the whole export succeeded.
	if _, err := stdout.Write(encoded); err != nil {
		return err
	}
	if *outFlag != "" {
		if err := writeFileAtomic(*outFlag, encoded); err != nil {
			return err
		}
	}
	return nil
}

// evidenceRow is this command's own minimal reading of one GET
// /v1/allocations item -- exactly the fields the export needs, read
// leniently (a field this client does not recognize is ignored, matching
// readFindingsPage's and readPoolsPage's own tolerance) rather than
// decoded against the full Allocation schema api/openapi.yaml declares.
type evidenceRow struct {
	ID                 string
	TenantID           string // empty unless this is an operator's own read (ADR 0011)
	AllocationKey      string
	Scope              string
	Environment        string
	Region             string
	AccountID          string
	PrefixLength       *int
	ParentAllocationID string
	State              string
	BindingVerifiedAt  string
}

// readAllocationsPage reads one page of GET /v1/allocations the way
// readFindingsPage and readPoolsPage read their own endpoints: an "items"
// list and a "next_cursor" are required; anything else this client does not
// use is ignored. A page without an items list is an error, not zero
// allocations -- the same rule every page walk in this file already
// enforces.
func readAllocationsPage(payload []byte) (rows []evidenceRow, next string, err error) {
	var envelope struct {
		Items *[]struct {
			ID                 string  `json:"id"`
			TenantID           *string `json:"tenant_id"`
			AllocationKey      string  `json:"allocation_key"`
			Scope              string  `json:"scope"`
			Environment        string  `json:"environment"`
			Region             string  `json:"region"`
			AccountID          string  `json:"account_id"`
			PrefixLength       *int    `json:"prefix_length"`
			ParentAllocationID *string `json:"parent_allocation_id"`
			State              string  `json:"state"`
			Binding            *struct {
				VerifiedAt string `json:"verified_at"`
			} `json:"binding"`
		} `json:"items"`
		NextCursor *string `json:"next_cursor"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, "", fmt.Errorf("allocations response is not valid JSON: %w", err)
	}
	if envelope.Items == nil {
		return nil, "", errors.New("allocations response has no items list")
	}
	out := make([]evidenceRow, 0, len(*envelope.Items))
	for _, item := range *envelope.Items {
		row := evidenceRow{
			ID:            item.ID,
			AllocationKey: item.AllocationKey,
			Scope:         item.Scope,
			Environment:   item.Environment,
			Region:        item.Region,
			AccountID:     item.AccountID,
			PrefixLength:  item.PrefixLength,
			State:         item.State,
		}
		if item.TenantID != nil {
			row.TenantID = *item.TenantID
		}
		if item.ParentAllocationID != nil {
			row.ParentAllocationID = *item.ParentAllocationID
		}
		if item.Binding != nil {
			row.BindingVerifiedAt = item.Binding.VerifiedAt
		}
		out = append(out, row)
	}
	if envelope.NextCursor != nil {
		next = *envelope.NextCursor
	}
	return out, next, nil
}

// resolveEvidenceScope implements the scope rule this file's evidence
// doc comment explains: infer "operator" when every returned row carries
// tenant_id, require --scope <tenant-id> when none do (nothing else can
// tell a tenant credential its own tenant id), and refuse rather than guess
// when the evidence is inconsistent with what was claimed or observed.
func resolveEvidenceScope(flagValue string, rows []evidenceRow) (string, error) {
	withTenantID := 0
	for _, r := range rows {
		if r.TenantID != "" {
			withTenantID++
		}
	}
	switch flagValue {
	case migrate.EvidenceScopeOperator:
		if withTenantID != len(rows) {
			return "", usagef("evidence: --scope operator was given, but %d of %d returned allocation(s) carried no tenant_id; an operator's own read of GET /v1/allocations carries it on every row (ADR 0011), so this credential's read does not look like an operator export", len(rows)-withTenantID, len(rows))
		}
		return migrate.EvidenceScopeOperator, nil
	case "":
		switch {
		case len(rows) == 0:
			return "", usagef("evidence: GET /v1/allocations returned no rows at all, so the scope cannot be inferred from them; pass --scope operator or --scope <tenant-id>")
		case withTenantID == len(rows):
			return migrate.EvidenceScopeOperator, nil
		case withTenantID == 0:
			return "", usagef("evidence: none of the %d returned allocation(s) carried tenant_id, so this looks like a tenant's own read, which the platform never labels with its own tenant id; pass --scope <tenant-id> naming the tenant this credential belongs to", len(rows))
		default:
			return "", usagef("evidence: %d of %d returned allocation(s) carried tenant_id and the rest did not, so the scope cannot be inferred consistently; pass --scope explicitly", withTenantID, len(rows))
		}
	default:
		// An explicit tenant id.
		if withTenantID != 0 {
			return "", usagef("evidence: --scope %s was given, but %d of %d returned allocation(s) carried tenant_id, a field only an operator's own read receives; pass --scope operator instead if this credential is an operator identity", flagValue, withTenantID, len(rows))
		}
		return flagValue, nil
	}
}

// buildEvidenceFile maps evidenceRow (this command's own reading of a page)
// onto migrate.EvidenceFile/EvidenceAllocation field by field, so the bytes
// this command writes decode through migrate.DecodeEvidence unchanged --
// the round trip cli_test.go's own evidence tests assert.
//
// ParentAllocationKey is resolved from the SAME read: the export walked
// every page before this function ever runs, so the parent's own row -- an
// ordinary allocation the same credential is authorized to read, in any
// state -- is present in rows whenever it exists at all under this scope.
// A parent whose row this read never saw (impossible for an unfiltered
// operator or tenant-wide read, since neither applies a state filter) leaves
// ParentAllocationKey empty; that limit is named in docs/MIGRATION_PROGRESS.md
// rather than assumed away.
func buildEvidenceFile(readAt, scope string, rows []evidenceRow) migrate.EvidenceFile {
	keyByID := make(map[string]string, len(rows))
	for _, r := range rows {
		keyByID[r.ID] = r.AllocationKey
	}
	allocations := make([]migrate.EvidenceAllocation, 0, len(rows))
	for _, r := range rows {
		tenantID := r.TenantID
		if scope != migrate.EvidenceScopeOperator {
			// A tenant export: every row belongs to the exporting tenant by
			// construction (the server's own TenantID filter), even though
			// the row itself carried no tenant_id to read it from.
			tenantID = scope
		}
		var parentKey string
		if r.ParentAllocationID != "" {
			parentKey = keyByID[r.ParentAllocationID]
		}
		allocations = append(allocations, migrate.EvidenceAllocation{
			TenantID:            tenantID,
			AllocationKey:       r.AllocationKey,
			Scope:               r.Scope,
			Environment:         r.Environment,
			Region:              r.Region,
			AccountID:           r.AccountID,
			PrefixLength:        r.PrefixLength,
			ParentAllocationKey: parentKey,
			State:               r.State,
			BindingVerifiedAt:   r.BindingVerifiedAt,
		})
	}
	// Deterministic output: two exports of the same server state produce
	// byte-identical files (aside from read_at), regardless of the order
	// the server happened to page them in.
	sort.Slice(allocations, func(i, j int) bool {
		if allocations[i].TenantID != allocations[j].TenantID {
			return allocations[i].TenantID < allocations[j].TenantID
		}
		return allocations[i].AllocationKey < allocations[j].AllocationKey
	})
	return migrate.EvidenceFile{ReadAt: readAt, Scope: scope, Allocations: allocations}
}

// writeFileAtomic writes data to a temp file beside path and renames it
// into place, so a failure partway through the write (disk full, process
// killed) never leaves path holding a truncated evidence file -- "never a
// partial file on error."
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".evidence-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename below succeeds
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func mustQuote(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return `""`
	}
	return string(encoded)
}

// writeJSON re-encodes the server payload so stdout is stable, indented JSON.
// A pending allocation carries no committed CIDR, and the contract already
// omits it; the client does not synthesize one.
func writeJSON(stdout io.Writer, payload []byte) error {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return nil
	}
	var decoded any
	if err := json.Unmarshal(trimmed, &decoded); err != nil {
		_, err := stdout.Write(append(trimmed, '\n'))
		return err
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(decoded)
}
