// Package client is the small, NetBox-independent HTTP client used by the
// Terraform provider. It deliberately models only the v1 consumer contract.
package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultHTTPTimeout = 30 * time.Second
	defaultPollDelay   = 2 * time.Second
	maxRetryDelay      = 30 * time.Second
)

type Config struct {
	Endpoint     string
	Token        string
	HTTPClient   *http.Client
	PollInterval time.Duration
}

type Client struct {
	endpoint     string
	token        string
	http         *http.Client
	pollInterval time.Duration
}

type AllocationRequest struct {
	AllocationKey      string            `json:"allocation_key"`
	Scope              string            `json:"scope"`
	Environment        string            `json:"environment"`
	Region             string            `json:"region"`
	AccountID          string            `json:"account_id"`
	AddressFamily      string            `json:"address_family,omitempty"`
	PrefixLength       int64             `json:"prefix_length"`
	ParentAllocationID string            `json:"parent_allocation_id,omitempty"`
	AvailabilityZoneID string            `json:"availability_zone_id,omitempty"`
	Description        string            `json:"description,omitempty"`
	Labels             map[string]string `json:"labels,omitempty"`
}

type AllocationPatch struct {
	Description *string            `json:"description,omitempty"`
	Labels      *map[string]string `json:"labels,omitempty"`
}

type Allocation struct {
	ID                 string            `json:"id"`
	AllocationKey      string            `json:"allocation_key"`
	Scope              string            `json:"scope"`
	Environment        string            `json:"environment"`
	Region             string            `json:"region"`
	AccountID          string            `json:"account_id"`
	AddressFamily      string            `json:"address_family"`
	PrefixLength       int64             `json:"prefix_length"`
	CIDR               string            `json:"cidr"`
	PoolID             string            `json:"pool_id"`
	ParentAllocationID string            `json:"parent_allocation_id"`
	AvailabilityZoneID string            `json:"availability_zone_id"`
	Description        string            `json:"description"`
	Labels             map[string]string `json:"labels"`
	State              string            `json:"state"`
	Revision           int64             `json:"revision"`
	Binding            *Binding          `json:"binding"`
	Links              Links             `json:"links"`
	ReleaseBlockers    []string          `json:"release_blockers"`
	InventorySync      string            `json:"inventory_sync"`
	ETag               string            `json:"-"`
}

type Binding struct {
	ResourceID   string `json:"resource_id"`
	ResourceType string `json:"resource_type"`
	AccountID    string `json:"account_id"`
	Region       string `json:"region"`
}

type Links struct {
	Inventory string `json:"inventory"`
}

type Operation struct {
	ID           string          `json:"id"`
	Type         string          `json:"type"`
	Status       string          `json:"status"`
	AllocationID string          `json:"allocation_id"`
	Result       json.RawMessage `json:"result"`
	Error        *APIErrorBody   `json:"error"`
}

type Pool struct {
	ID              string   `json:"id"`
	Scopes          []string `json:"scopes"`
	Environment     string   `json:"environment"`
	Region          string   `json:"region"`
	AllowedPrefixes []int64  `json:"allowed_prefix_lengths"`
	PolicyVersion   string   `json:"policy_version"`
}

type APIErrorBody struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	RequestID string         `json:"request_id"`
	Retryable bool           `json:"retryable"`
	Details   map[string]any `json:"details"`
}

type HTTPError struct {
	Status     int
	Code       string
	Message    string
	RequestID  string
	Retryable  bool
	RetryAfter time.Duration
	Details    map[string]any
}

// Error renders the diagnostic. Status is 0 exactly when this HTTPError
// carries a terminal operation's own OperationError rather than a failed
// HTTP round trip -- waitForAllocation's FAILED case below, the mirror of
// internal/cli/cli.go's awaitOperation fix (ADR 0013): the GET that surfaced
// the failure succeeded, so there is no HTTP status to report, and printing
// "HTTP 0" claimed one that never existed. That case is worded without the
// "HTTP %d" phrasing; every real HTTP failure (doOnce below, and the two
// synthetic not-found cases in FindAllocationByKey and GetPool) keeps it.
func (e *HTTPError) Error() string {
	if e.Status == 0 {
		if e.Code == "" {
			return "platform-ipam reservation operation failed: " + e.Message
		}
		return fmt.Sprintf("platform-ipam reservation operation failed (%s): %s", e.Code, e.Message)
	}
	if e.Code == "" {
		return fmt.Sprintf("platform-ipam API returned HTTP %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("platform-ipam API returned HTTP %d (%s): %s", e.Status, e.Code, e.Message)
}

func (e *HTTPError) IsStatus(status int) bool { return e != nil && e.Status == status }

func New(cfg Config) (*Client, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(cfg.Endpoint), "/")
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("endpoint must be an absolute URL")
	}
	if u.User != nil {
		return nil, fmt.Errorf("endpoint must not contain user info")
	}
	if u.Scheme != "https" {
		if u.Scheme != "http" || !allowLocalHTTP() || !isLoopback(u.Hostname()) {
			return nil, fmt.Errorf("endpoint must use HTTPS; HTTP is permitted only for loopback when PLATFORM_IPAM_ALLOW_LOCAL_HTTP=1")
		}
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: defaultHTTPTimeout}
	}
	interval := cfg.PollInterval
	if interval <= 0 {
		interval = defaultPollDelay
	}
	return &Client{endpoint: endpoint, token: cfg.Token, http: hc, pollInterval: interval}, nil
}

func allowLocalHTTP() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("PLATFORM_IPAM_ALLOW_LOCAL_HTTP")))
	return v == "1" || v == "true" || v == "yes"
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c *Client) CreateAllocation(ctx context.Context, request AllocationRequest, key string) (*Allocation, error) {
	var allocation Allocation
	var operation Operation
	status, err := c.doJSON(ctx, http.MethodPost, "/v1/allocations", request, key, &allocation, &operation)
	if err != nil {
		return nil, err
	}
	if status == http.StatusAccepted {
		if operation.ID == "" {
			return nil, fmt.Errorf("POST accepted without an operation ID")
		}
		return c.waitForAllocation(ctx, operation.ID)
	}
	if allocation.ID == "" {
		return nil, fmt.Errorf("POST returned no committed allocation ID")
	}
	return &allocation, nil
}

func (c *Client) waitForAllocation(ctx context.Context, operationID string) (*Allocation, error) {
	for {
		var operation Operation
		status, err := c.doJSON(ctx, http.MethodGet, "/v1/operations/"+url.PathEscape(operationID), nil, "", &operation, nil)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("operation poll returned unexpected HTTP %d", status)
		}
		switch operation.Status {
		case "SUCCEEDED":
			if operation.AllocationID == "" {
				var result struct {
					AllocationID string `json:"allocation_id"`
				}
				_ = json.Unmarshal(operation.Result, &result)
				operation.AllocationID = result.AllocationID
			}
			if operation.AllocationID == "" {
				return nil, fmt.Errorf("successful operation returned no allocation ID")
			}
			return c.GetAllocation(ctx, operation.AllocationID)
		case "FAILED":
			if operation.Error != nil {
				return nil, &HTTPError{Code: operation.Error.Code, Message: operation.Error.Message, RequestID: operation.Error.RequestID, Details: operation.Error.Details}
			}
			return nil, fmt.Errorf("allocation operation %s failed", operationID)
		case "PENDING", "":
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(c.pollInterval):
			}
		default:
			return nil, fmt.Errorf("operation %s has unknown status %q", operationID, operation.Status)
		}
	}
}

func (c *Client) GetAllocation(ctx context.Context, id string) (*Allocation, error) {
	var allocation Allocation
	_, err := c.doJSON(ctx, http.MethodGet, "/v1/allocations/"+url.PathEscape(id), nil, "", &allocation, nil)
	if err != nil {
		return nil, err
	}
	if allocation.ID == "" {
		allocation.ID = id
	}
	return &allocation, nil
}

func (c *Client) FindAllocationByKey(ctx context.Context, key string) (*Allocation, error) {
	path := "/v1/allocations?allocation_key=" + url.QueryEscape(key)
	var response struct {
		Items []Allocation `json:"items"`
	}
	_, err := c.doJSON(ctx, http.MethodGet, path, nil, "", &response, nil)
	if err != nil {
		return nil, err
	}
	if len(response.Items) == 0 {
		return nil, &HTTPError{Status: http.StatusNotFound, Code: "not_found", Message: "allocation key was not found"}
	}
	if len(response.Items) > 1 {
		return nil, fmt.Errorf("allocation key %q returned multiple allocations", key)
	}
	return &response.Items[0], nil
}

func (c *Client) UpdateAllocation(ctx context.Context, id, etag, key string, patch AllocationPatch) (*Allocation, error) {
	var allocation Allocation
	_, err := c.doJSONWithHeaders(ctx, http.MethodPatch, "/v1/allocations/"+url.PathEscape(id), patch, key, map[string]string{"If-Match": etag}, &allocation, nil)
	if err != nil {
		return nil, err
	}
	return &allocation, nil
}

// DeleteAllocation reports whether the API durably accepted quarantine. A
// 204 means the allocation has already reached RELEASED. An ambiguous 404 is
// returned to the caller so Terraform can preserve state and fail closed.
func (c *Client) DeleteAllocation(ctx context.Context, id string) (*Allocation, bool, error) {
	var allocation Allocation
	status, err := c.doJSON(ctx, http.MethodDelete, "/v1/allocations/"+url.PathEscape(id), nil, "", &allocation, nil)
	if err != nil {
		return nil, false, err
	}
	if status == http.StatusNoContent {
		return nil, true, nil
	}
	if status == http.StatusAccepted {
		return &allocation, true, nil
	}
	return &allocation, false, fmt.Errorf("DELETE returned unexpected HTTP %d", status)
}

func (c *Client) GetPool(ctx context.Context, id string) (*Pool, error) {
	var response struct {
		Items []Pool `json:"items"`
	}
	_, err := c.doJSON(ctx, http.MethodGet, "/v1/pools", nil, "", &response, nil)
	if err != nil {
		return nil, err
	}
	for _, pool := range response.Items {
		if pool.ID == id {
			return &pool, nil
		}
	}
	return nil, &HTTPError{Status: http.StatusNotFound, Code: "not_found", Message: "pool was not found"}
}

func (c *Client) doJSON(ctx context.Context, method, path string, body any, idempotency string, success any, accepted any) (int, error) {
	return c.doJSONWithHeaders(ctx, method, path, body, idempotency, nil, success, accepted)
}

func (c *Client) doJSONWithHeaders(ctx context.Context, method, path string, body any, idempotency string, extra map[string]string, success any, accepted any) (int, error) {
	var payload []byte
	var err error
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("encode request: %w", err)
		}
	}
	for attempt := 0; ; attempt++ {
		status, retryAfter, err := c.doOnce(ctx, method, path, payload, idempotency, extra, success, accepted)
		if err == nil {
			return status, nil
		}
		var apiErr *HTTPError
		transient := errors.As(err, &apiErr) && (apiErr.Status == 429 || apiErr.Status == 502 || apiErr.Status == 503 || apiErr.Status == 504)
		if !transient && !isNetworkError(err) {
			return status, err
		}
		if attempt >= 7 {
			return status, err
		}
		delay := retryAfter
		if delay <= 0 {
			delay = time.Duration(math.Min(float64(maxRetryDelay), float64(250*time.Millisecond)*math.Pow(2, float64(attempt))))
		}
		select {
		case <-ctx.Done():
			return status, ctx.Err()
		case <-time.After(delay):
		}
	}
}

func (c *Client) doOnce(ctx context.Context, method, path string, payload []byte, idempotency string, extra map[string]string, success, accepted any) (int, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, bytes.NewReader(payload))
	if err != nil {
		return 0, 0, err
	}
	if len(payload) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if idempotency != "" {
		req.Header.Set("Idempotency-Key", idempotency)
	}
	for key, value := range extra {
		req.Header.Set(key, value)
	}
	response, err := c.http.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if readErr != nil {
		return response.StatusCode, 0, readErr
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return response.StatusCode, parseRetryAfter(response.Header.Get("Retry-After")), parseHTTPError(response.StatusCode, body)
	}
	if response.StatusCode == http.StatusNoContent || len(bytes.TrimSpace(body)) == 0 {
		return response.StatusCode, 0, nil
	}
	var target any = success
	if response.StatusCode == http.StatusAccepted && accepted != nil {
		target = accepted
	}
	if target != nil {
		if err := json.Unmarshal(body, target); err != nil {
			return response.StatusCode, 0, fmt.Errorf("decode API response: %w", err)
		}
		if allocation, ok := target.(*Allocation); ok {
			allocation.ETag = response.Header.Get("ETag")
			if allocation.ETag == "" && allocation.Revision > 0 {
				allocation.ETag = strconv.Quote(strconv.FormatInt(allocation.Revision, 10))
			}
		}
	}
	return response.StatusCode, 0, nil
}

func parseHTTPError(status int, body []byte) error {
	var envelope struct {
		Error APIErrorBody `json:"error"`
	}
	_ = json.Unmarshal(body, &envelope)
	errBody := envelope.Error
	if errBody.Message == "" {
		errBody.Message = strings.TrimSpace(string(body))
		if errBody.Message == "" {
			errBody.Message = http.StatusText(status)
		}
	}
	return &HTTPError{Status: status, Code: errBody.Code, Message: errBody.Message, RequestID: errBody.RequestID, Retryable: errBody.Retryable, Details: errBody.Details}
}

func parseRetryAfter(value string) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	return 0
}

func isNetworkError(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr)
}

func NewIdempotencyKey(prefix string) string {
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return prefix + "-fallback"
	}
	return prefix + "-" + hex.EncodeToString(random[:])
}
