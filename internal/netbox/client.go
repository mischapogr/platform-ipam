// Package netbox contains the HTTP boundary for the platform's managed
// NetBox inventory.  NetBox response types intentionally do not escape this
// package; the rest of the service deals only in domain values.
package netbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

// maxResponseBytes bounds a single NetBox response held in memory.
const maxResponseBytes = 16 << 20

const (
	defaultPageSize = 200
	defaultTimeout  = 10 * time.Second
	allocationIDCF  = "platform_allocation_id"
	allocationKeyCF = "platform_allocation_key"
	operationIDCF   = "platform_operation_id"
	stateCF         = "platform_state"
	// parentAllocationIDCF is the nearest thing the inventory has to an
	// allocation's scope, and the onboarding import reads it as one: see
	// domain.Network.ParentAllocationID.
	parentAllocationIDCF = "platform_parent_allocation_id"
)

// Config describes the NetBox endpoint and the domain/pool definitions it is
// permitted to serve. HTTPClient is optional; its transport is retained while
// redirects are disabled by the adapter.
type Config struct {
	BaseURL                   string
	Token                     string
	Domains                   []domain.Domain
	Pools                     []domain.Pool
	HTTPClient                *http.Client
	Timeout                   time.Duration
	ProjectionRefreshInterval time.Duration
}

// Client implements domain.Inventory against a NetBox REST API.
type Client struct {
	base                      *url.URL
	token                     string
	domains                   map[string]domain.Domain
	pools                     map[string]domain.Pool
	http                      *http.Client
	timeout                   time.Duration
	projectionRefreshInterval time.Duration
	now                       func() time.Time
}

// HTTPError intentionally omits response bodies: NetBox validation pages can
// contain implementation details that must not escape the adapter boundary.
type HTTPError struct {
	Method, Path string
	Status       int
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("netbox %s %s: HTTP %d", e.Method, e.Path, e.Status)
}

// New constructs a client from Config.
func New(cfg Config) (*Client, error) {
	base, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/"))
	if err != nil || base.Scheme != "http" && base.Scheme != "https" || base.Host == "" {
		return nil, fmt.Errorf("netbox base URL must be an absolute HTTP URL")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{}
	}
	// A redirect can silently move the bearer token to a different service.
	// Reject it even when the caller supplied a client whose policy follows it.
	transportClient := *hc
	transportClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return errors.New("netbox redirects are not permitted")
	}
	if transportClient.Timeout <= 0 {
		transportClient.Timeout = timeout
	}
	c := &Client{base: base, token: cfg.Token, http: &transportClient, timeout: timeout,
		projectionRefreshInterval: cfg.ProjectionRefreshInterval, now: time.Now,
		domains: make(map[string]domain.Domain), pools: make(map[string]domain.Pool)}
	for _, d := range cfg.Domains {
		c.domains[d.ID] = d
	}
	for _, p := range cfg.Pools {
		c.pools[p.ID] = p
	}
	return c, nil
}

// NewNetBox is the named convenience constructor used by application wiring.
func NewNetBox(cfg Config) (*Client, error) { return New(cfg) }

type apiPage struct {
	Count   int               `json:"count"`
	Next    json.RawMessage   `json:"next"`
	Results []json.RawMessage `json:"results"`
}

// choice decodes a NetBox enumerated field. NetBox 4.x renders these as
// {"value", "label"} objects, while older releases and some brief
// serializations return a bare string. Accepting both keeps a representation
// change from making an entire inventory page unreadable -- which would turn
// a cosmetic upstream difference into "no complete snapshot", and therefore
// into a refusal to allocate.
type choice string

func (c *choice) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		*c = choice(text)
		return nil
	}
	var object struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(data, &object); err != nil {
		return fmt.Errorf("netbox choice must be a string or an object with a value: %w", err)
	}
	*c = choice(object.Value)
	return nil
}

type prefix struct {
	ID           int            `json:"id"`
	Prefix       string         `json:"prefix"`
	VRF          any            `json:"vrf"`
	Status       choice         `json:"status"`
	CustomFields map[string]any `json:"custom_fields"`
	// Tags are decoded here rather than in a second prefix type because the
	// import tag is what separates occupancy from ownership (ADR 0007), and
	// both Snapshot and Adopt have to see it on the same read.
	Tags []awsTag `json:"tags"`
	// Description is decoded only for ADR 0016's removal (package M9b4):
	// onboard remove refuses a prefix whose description is not the one the
	// import would generate from its own contributor list, because that
	// means an operator wrote something there. No other caller in this
	// package reads it -- Snapshot does not carry it into domain.Network,
	// and nothing before this package has ever needed the description back.
	Description string `json:"description"`
}

// imported reports the tag an onboarding import puts on the occupancy it
// writes. It is the only evidence that a prefix is ours to convert.
func (x prefix) imported() bool { return hasTagSlug(x.Tags, ImportedTag) }

// owned reports any of the four ownership custom fields. Two of them reach the
// snapshot as values; a prefix carrying only one of the other two is a
// half-written object rather than free space, so it is owned here too.
func (x prefix) owned() bool {
	for _, field := range []string{allocationIDCF, allocationKeyCF, operationIDCF, stateCF} {
		if stringCF(x.CustomFields, field) != "" {
			return true
		}
	}
	return false
}

type vrf struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}
type ipAddress struct {
	ID      int    `json:"id"`
	Address string `json:"address"`
	VRF     any    `json:"vrf"`
}
type ipRange struct {
	ID           int    `json:"id"`
	StartAddress string `json:"start_address"`
	EndAddress   string `json:"end_address"`
	VRF          any    `json:"vrf"`
}

func (c *Client) request(ctx context.Context, method, path string, body any) (*http.Response, error) {
	return c.requestWithHeaders(ctx, method, path, body, nil)
}

// requestWithHeaders is request with additional request headers. Only the
// conditional write in Adopt sets any; the adapter's own Content-Type and
// Authorization are applied afterwards, so a caller cannot displace them.
func (c *Client) requestWithHeaders(ctx context.Context, method, path string, body any, headers map[string]string) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base.ResolveReference(&url.URL{Path: "/"}).String()+strings.TrimLeft(path, "/"), reader)
	if err != nil {
		return nil, err
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Token "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.Request != nil && (resp.Request.URL.Scheme != c.base.Scheme || !strings.EqualFold(resp.Request.URL.Host, c.base.Host)) {
		resp.Body.Close()
		return nil, errors.New("netbox request crossed origin")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, &HTTPError{Method: method, Path: path, Status: resp.StatusCode}
	}
	// Read the body here rather than handing the caller a streaming one. The
	// deferred cancel above tears down the request context the moment this
	// function returns, so a caller that read afterwards would fail with
	// "context canceled" as soon as a response stopped fitting the
	// transport's read buffer -- that is, silently, once inventory grew.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("read netbox response: %w", err)
	}
	resp.Body = io.NopCloser(bytes.NewReader(data))
	return resp, nil
}

func (c *Client) page(ctx context.Context, path string, decode func(json.RawMessage) error) error {
	parsed, err := url.Parse(path)
	if err != nil {
		return err
	}
	if parsed.Path == "" {
		parsed.Path = "/api/"
	}
	next := c.base.ResolveReference(&url.URL{Path: "/api/" + strings.TrimPrefix(parsed.Path, "/api/"), RawQuery: parsed.RawQuery})
	q := next.Query()
	q.Set("limit", strconv.Itoa(defaultPageSize))
	q.Set("offset", "0")
	next.RawQuery = q.Encode()
	for {
		if next.Scheme != c.base.Scheme || !strings.EqualFold(next.Host, c.base.Host) || !strings.HasPrefix(next.Path, "/api/") {
			return errors.New("netbox pagination crossed origin or API boundary")
		}
		resp, err := c.request(ctx, http.MethodGet, strings.TrimPrefix(next.Path, "/")+"?"+next.RawQuery, nil)
		if err != nil {
			return err
		}
		var p apiPage
		data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()
		if err != nil {
			return err
		}
		if err := json.Unmarshal(data, &p); err != nil {
			return fmt.Errorf("decode netbox page: %w", err)
		}
		for _, raw := range p.Results {
			if err := decode(raw); err != nil {
				return err
			}
		}
		if len(p.Next) == 0 || string(p.Next) == "null" {
			return nil
		}
		var s string
		if err := json.Unmarshal(p.Next, &s); err != nil || s == "" {
			return errors.New("netbox pagination next must be a URL or null")
		}
		n, err := url.Parse(s)
		if err != nil || n.IsAbs() && (n.Scheme != c.base.Scheme || !strings.EqualFold(n.Host, c.base.Host)) {
			return errors.New("netbox pagination URL is not same-origin")
		}
		if !n.IsAbs() {
			n = c.base.ResolveReference(n)
		}
		next = n
	}
}

func intID(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case json.Number:
		i, _ := x.Int64()
		return int(i)
	case map[string]any:
		return intID(x["id"])
	}
	return 0
}
func (p prefix) vrfID() int    { return intID(p.VRF) }
func (a ipAddress) vrfID() int { return intID(a.VRF) }
func (r ipRange) vrfID() int   { return intID(r.VRF) }
func stringCF(fields map[string]any, key string) string {
	// NetBox returns an unset custom field as JSON null, not as a missing key.
	// fmt.Sprint(nil) is the non-empty string "<nil>", which made every
	// unmanaged prefix -- including the pool container -- look as if it
	// carried an allocation id.
	if v, ok := fields[key]; ok && v != nil {
		if s, ok := v.(string); ok {
			return s
		}
		return fmt.Sprint(v)
	}
	return ""
}

// contributorsCF decodes the unowned contributor list ADR 0016 puts on an
// imported prefix, and reports separately whether the field held something
// this version cannot read.
//
// The custom field arrives already decoded into `any` by the page reader, so
// the value is re-marshalled and unmarshalled into the typed slice rather
// than walked by hand: NetBox returns it as structured JSON, not as a string
// (measured against the pinned image, ADR 0016's dated paragraph), and a
// hand-walked decode would have to re-implement every type rule json already
// applies.
//
// Three outcomes, all of which a caller must be able to tell apart:
//
//   - absent, or JSON null: (nil, false). This is every prefix imported
//     before ADR 0016 and every object that is not an imported prefix. An
//     unset custom field is the key present with null, not a missing key --
//     the same shape stringCF was taught in package C5.
//   - a readable array: (the entries, false). An empty array decodes to an
//     empty but NON-nil slice, which ADR 0016 needs kept distinct from
//     absence: "an empty list is not an argument that nothing contributes; it
//     is a list nobody wrote".
//   - anything else: (nil, true). An operator's hand edit, or a shape a later
//     version writes. This is never an error, because a failed Snapshot takes
//     the whole overlap domain offline and one malformed field must not do
//     that, and never silence, because a refresh may populate an absent list
//     while overwriting an unreadable one would destroy it.
func contributorsCF(fields map[string]any, key string) ([]domain.Contributor, bool) {
	raw, present := fields[key]
	if !present || raw == nil {
		return nil, false
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, true
	}
	var out []domain.Contributor
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, true
	}
	if out == nil {
		// Reachable only if the value marshalled to a literal null, which the
		// nil check above already covers; keep the invariant explicit rather
		// than returning a nil slice that would read as "no field".
		return []domain.Contributor{}, false
	}
	return out, false
}

// boolCF decodes a NetBox boolean custom field the same way stringCF decodes
// a text one: an unset field arrives as the key present with JSON null, not a
// missing key, and that reads as false here -- exactly what
// platform_import_contributors_reconstructed means before package M9b3's
// refresh ever sets it true.
func boolCF(fields map[string]any, key string) bool {
	v, ok := fields[key]
	if !ok || v == nil {
		return false
	}
	b, _ := v.(bool)
	return b
}

func parsePrefix(s string) (netip.Prefix, error) { return netip.ParsePrefix(strings.TrimSpace(s)) }

func (c *Client) domainPools(d domain.Domain) ([]domain.Pool, error) {
	var pools []domain.Pool
	for _, p := range c.pools {
		if p.DomainID == d.ID {
			pools = append(pools, p)
		}
	}
	for _, p := range pools {
		if p.CIDR == "" {
			return nil, fmt.Errorf("pool %s has no CIDR", p.ID)
		}
		if _, err := parsePrefix(p.CIDR); err != nil {
			return nil, fmt.Errorf("pool %s CIDR: %w", p.ID, err)
		}
		vrfID := p.Backend.VRFID
		if vrfID == 0 {
			vrfID = d.Backend.VRFID
		}
		if vrfID != d.Backend.VRFID && d.Backend.VRFID != 0 {
			return nil, fmt.Errorf("pool %s VRF %d differs from domain VRF %d", p.ID, vrfID, d.Backend.VRFID)
		}
	}
	return pools, nil
}

func poolVRF(d domain.Domain, p domain.Pool) int {
	if p.Backend.VRFID != 0 {
		return p.Backend.VRFID
	}
	return d.Backend.VRFID
}

// Snapshot reads every relevant NetBox collection. Individual address/range
// records are represented as occupied CIDRs so callers cannot mistake a host
// record for free space.
func (c *Client) Snapshot(ctx context.Context, d domain.Domain) (domain.InventorySnapshot, error) {
	if d.ID == "" {
		return domain.InventorySnapshot{}, errors.New("domain ID is required")
	}
	pools, err := c.domainPools(d)
	if err != nil {
		return domain.InventorySnapshot{}, err
	}
	vrfs := map[int]bool{}
	if err := c.page(ctx, "/api/ipam/vrfs/", func(raw json.RawMessage) error {
		var v vrf
		if err := json.Unmarshal(raw, &v); err != nil {
			return err
		}
		if v.ID == d.Backend.VRFID {
			vrfs[v.ID] = true
		}
		return nil
	}); err != nil {
		return domain.InventorySnapshot{}, err
	}
	if d.Backend.VRFID != 0 && !vrfs[d.Backend.VRFID] {
		return domain.InventorySnapshot{}, fmt.Errorf("configured VRF %d was not found", d.Backend.VRFID)
	}
	var prefixes []prefix
	if err := c.page(ctx, "/api/ipam/prefixes/", func(raw json.RawMessage) error {
		var p prefix
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		if d.Backend.VRFID == 0 || p.vrfID() == d.Backend.VRFID {
			prefixes = append(prefixes, p)
		}
		return nil
	}); err != nil {
		return domain.InventorySnapshot{}, err
	}
	var addresses []ipAddress
	if err := c.page(ctx, "/api/ipam/ip-addresses/", func(raw json.RawMessage) error {
		var a ipAddress
		if err := json.Unmarshal(raw, &a); err != nil {
			return err
		}
		if d.Backend.VRFID == 0 || a.vrfID() == d.Backend.VRFID {
			addresses = append(addresses, a)
		}
		return nil
	}); err != nil {
		return domain.InventorySnapshot{}, err
	}
	var ranges []ipRange
	if err := c.page(ctx, "/api/ipam/ip-ranges/", func(raw json.RawMessage) error {
		var r ipRange
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
		if d.Backend.VRFID == 0 || r.vrfID() == d.Backend.VRFID {
			ranges = append(ranges, r)
		}
		return nil
	}); err != nil {
		return domain.InventorySnapshot{}, err
	}

	seen := map[string]bool{}
	var out []domain.Network
	for _, p := range prefixes {
		cidr, err := parsePrefix(p.Prefix)
		if err != nil {
			return domain.InventorySnapshot{}, fmt.Errorf("prefix %d: %w", p.ID, err)
		}
		if d.Backend.RequireUniquePrefixes {
			key := fmt.Sprintf("%d|%s", p.vrfID(), cidr.String())
			if seen[key] {
				return domain.InventorySnapshot{}, fmt.Errorf("duplicate prefix %s in VRF %d", cidr, p.vrfID())
			}
			seen[key] = true
		}
		pool := false
		ancestor := false
		for _, configured := range pools {
			pc, _ := parsePrefix(configured.CIDR)
			if cidr == pc {
				pool = true
			}
			if pc.Bits() > cidr.Bits() && cidr.Contains(pc.Addr()) {
				ancestor = true
			}
		}
		if ancestor && !pool {
			continue
		}
		contributors, unreadable := contributorsCF(p.CustomFields, ImportContributorsField)
		out = append(out, domain.Network{ID: strconv.Itoa(p.ID), CIDR: cidr.String(), AllocationID: stringCF(p.CustomFields, allocationIDCF), OperationID: stringCF(p.CustomFields, operationIDCF), ParentPool: pool, Imported: p.imported(), Owned: p.owned(), ImportBatch: stringCF(p.CustomFields, ImportBatchField), ParentAllocationID: stringCF(p.CustomFields, parentAllocationIDCF), Status: string(p.Status), AWSAccountID: stringCF(p.CustomFields, awsAccountCF), AWSRegion: stringCF(p.CustomFields, awsRegionCF), Contributors: contributors, ContributorsUnreadable: unreadable, ContributorsReconstructed: boolCF(p.CustomFields, ImportContributorsReconstructedField)})
	}
	for _, configured := range pools {
		configuredCIDR, _ := parsePrefix(configured.CIDR)
		matches := 0
		for _, p := range prefixes {
			if p.vrfID() == poolVRF(d, configured) && p.Prefix == configuredCIDR.String() {
				matches++
			}
		}
		if matches != 1 {
			return domain.InventorySnapshot{}, fmt.Errorf("configured pool %s has %d matching prefixes in VRF %d", configured.ID, matches, poolVRF(d, configured))
		}
	}
	for _, a := range addresses {
		pfx, err := addressCIDR(a.Address)
		if err != nil {
			return domain.InventorySnapshot{}, fmt.Errorf("IP address %d: %w", a.ID, err)
		}
		out = append(out, domain.Network{ID: "ip-" + strconv.Itoa(a.ID), CIDR: pfx, ParentPool: false})
	}
	for _, r := range ranges {
		blocks, err := rangeCIDRs(r.StartAddress, r.EndAddress)
		if err != nil {
			return domain.InventorySnapshot{}, fmt.Errorf("IP range %d: %w", r.ID, err)
		}
		for i, b := range blocks {
			out = append(out, domain.Network{ID: fmt.Sprintf("range-%d-%d", r.ID, i), CIDR: b})
		}
	}
	return domain.InventorySnapshot{Networks: out, Complete: true}, nil
}

func addressCIDR(s string) (string, error) {
	s = strings.Split(strings.TrimSpace(s), "/")[0]
	a, err := netip.ParseAddr(s)
	if err != nil {
		return "", err
	}
	bits := 128
	if a.Is4() {
		bits = 32
	}
	return netip.PrefixFrom(a, bits).String(), nil
}

const maxRangeBlocks = 4096

func rangeCIDRs(start, end string) ([]string, error) {
	start = strings.Split(strings.TrimSpace(start), "/")[0]
	end = strings.Split(strings.TrimSpace(end), "/")[0]
	a, err := netip.ParseAddr(start)
	if err != nil {
		return nil, err
	}
	b, err := netip.ParseAddr(end)
	if err != nil {
		return nil, err
	}
	if a.BitLen() != b.BitLen() || a.Compare(b) > 0 {
		return nil, errors.New("invalid IP range")
	}
	var out []string
	cur := a
	for cur.Compare(b) <= 0 && len(out) < maxRangeBlocks {
		var chosen netip.Prefix
		for bits := 0; bits <= cur.BitLen(); bits++ {
			p := netip.PrefixFrom(cur, bits).Masked()
			if p.Addr().Compare(cur) == 0 && prefixLast(p).Compare(b) <= 0 {
				chosen = p
				break
			}
		}
		if !chosen.IsValid() {
			return nil, errors.New("could not cover IP range")
		}
		out = append(out, chosen.String())
		next := prefixLast(chosen).Next()
		if !next.IsValid() {
			break
		}
		cur = next
	}
	if cur.Compare(b) <= 0 { // bounded conservative cover
		out = append(out, coveringPrefix(cur, b).String())
	}
	return out, nil
}

func prefixLast(p netip.Prefix) netip.Addr {
	a := p.Masked().Addr()
	if a.Is4() {
		v := a.As4()
		host := 32 - p.Bits()
		for i := 0; i < host; i++ {
			v[3-i/8] |= 1 << uint(i%8)
		}
		return netip.AddrFrom4(v)
	}
	v := a.As16()
	host := 128 - p.Bits()
	for i := 0; i < host; i++ {
		v[15-i/8] |= 1 << uint(i%8)
	}
	return netip.AddrFrom16(v)
}

func coveringPrefix(a, b netip.Addr) netip.Prefix {
	for bits := 0; bits <= a.BitLen(); bits++ {
		p := netip.PrefixFrom(a, bits).Masked()
		if p.Contains(b) {
			return p
		}
	}
	return netip.PrefixFrom(a, a.BitLen())
}

func (c *Client) findByMarker(ctx context.Context, allocationID string) ([]prefix, error) {
	var found []prefix
	path := "/api/ipam/prefixes/?cf_" + url.QueryEscape(allocationIDCF) + "=" + url.QueryEscape(allocationID)
	// Some NetBox releases do not filter custom fields unless the query uses
	// cf_<name>. The endpoint may still return all records; filter again locally.
	resp, err := c.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	var p apiPage
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	for _, raw := range p.Results {
		var x prefix
		if err := json.Unmarshal(raw, &x); err != nil {
			return nil, err
		}
		if stringCF(x.CustomFields, allocationIDCF) == allocationID {
			found = append(found, x)
		}
	}
	// Follow any additional pages using the same boundary checks.
	if len(p.Next) > 0 && string(p.Next) != "null" {
		var s string
		if json.Unmarshal(p.Next, &s) != nil || s == "" {
			return nil, errors.New("netbox marker pagination next must be a URL or null")
		}
		{
			u, e := url.Parse(s)
			if e != nil {
				return nil, e
			}
			if !u.IsAbs() {
				u = c.base.ResolveReference(u)
			}
			if u.Host != c.base.Host || u.Scheme != c.base.Scheme {
				return nil, errors.New("netbox marker pagination crossed origin")
			}
			rest := strings.TrimPrefix(u.Path, "/") + "?" + u.RawQuery
			more := []prefix{}
			if e := c.page(ctx, rest, func(raw json.RawMessage) error {
				var x prefix
				if e := json.Unmarshal(raw, &x); e != nil {
					return e
				}
				if stringCF(x.CustomFields, allocationIDCF) == allocationID {
					more = append(more, x)
				}
				return nil
			}); e != nil {
				return nil, e
			}
			found = append(found, more...)
		}
	}
	return found, nil
}

func (c *Client) allocationDomain(a domain.Allocation) (domain.Domain, domain.Pool, error) {
	d, ok := c.domains[a.DomainID]
	if !ok {
		return domain.Domain{}, domain.Pool{}, fmt.Errorf("unknown domain %q", a.DomainID)
	}
	p, ok := c.pools[a.PoolID]
	if !ok {
		return domain.Domain{}, domain.Pool{}, fmt.Errorf("unknown pool %q", a.PoolID)
	}
	if p.DomainID != d.ID {
		return domain.Domain{}, domain.Pool{}, errors.New("allocation pool belongs to another domain")
	}
	return d, p, nil
}
func (c *Client) validateExisting(x prefix, a domain.Allocation, opID string, vrfID int) error {
	if x.CustomFields == nil {
		return errors.New("managed prefix has no identity metadata")
	}
	if stringCF(x.CustomFields, allocationIDCF) != a.ID {
		return errors.New("allocation marker mismatch")
	}
	if op := stringCF(x.CustomFields, operationIDCF); op != opID {
		return fmt.Errorf("operation marker conflict: %s", op)
	}
	if x.Prefix != a.CIDR {
		return fmt.Errorf("CIDR conflict: existing %s, expected %s", x.Prefix, a.CIDR)
	}
	if x.vrfID() != vrfID {
		return fmt.Errorf("VRF conflict: existing %d, expected %d", x.vrfID(), vrfID)
	}
	return nil
}

func ownedFields(a domain.Allocation, operationID string) map[string]any {
	fields := map[string]any{allocationIDCF: a.ID, allocationKeyCF: a.AllocationKey, operationIDCF: operationID, parentAllocationIDCF: a.ParentAllocationID, "platform_tenant_id": a.TenantID, "platform_environment": a.Environment, "platform_pool_id": a.PoolID, "platform_policy_version": a.PolicyVersion, stateCF: a.State, "platform_aws_account_id": a.AccountID, "platform_aws_region": a.Region, "platform_aws_az_id": a.AvailabilityZoneID}
	if a.Binding != nil {
		fields["platform_aws_resource_id"] = a.Binding.ResourceID
	}
	if a.LastObservedAt != nil {
		fields["platform_last_observed_at"] = a.LastObservedAt.UTC().Format(time.RFC3339)
	}
	if a.QuarantineUntil != nil {
		fields["platform_quarantine_until"] = a.QuarantineUntil.UTC().Format(time.RFC3339)
	}
	return fields
}

// Ensure creates exactly the allocation's CIDR, or recovers an already-created
// object. It never invokes NetBox's next-available endpoint.
func (c *Client) Ensure(ctx context.Context, a domain.Allocation, opID string) (string, error) {
	if a.ID == "" || a.CIDR == "" || opID == "" {
		return "", errors.New("allocation ID, CIDR, and operation ID are required")
	}
	d, p, err := c.allocationDomain(a)
	if err != nil {
		return "", err
	}
	if _, err := parsePrefix(a.CIDR); err != nil {
		return "", err
	}
	pc, _ := parsePrefix(p.CIDR)
	ac, _ := parsePrefix(a.CIDR)
	if !pc.Contains(ac.Addr()) || ac.Bits() < pc.Bits() {
		return "", errors.New("allocation CIDR is outside pool")
	}
	found, err := c.findByMarker(ctx, a.ID)
	if err != nil {
		return "", uncertainEnsure("listing prefixes marked for "+a.ID, err)
	}
	if len(found) > 1 {
		return "", errors.New("duplicate allocation marker in NetBox")
	}
	if len(found) == 1 {
		if err := c.validateExisting(found[0], a, opID, poolVRF(d, p)); err != nil {
			return "", err
		}
		return strconv.Itoa(found[0].ID), nil
	}
	// Exact-CIDR lookup catches an unowned or differently-owned prefix before a
	// create; NetBox's uniqueness response is still checked as a final guard.
	var same []prefix
	if err := c.page(ctx, "/api/ipam/prefixes/?prefix="+url.QueryEscape(a.CIDR), func(raw json.RawMessage) error {
		var x prefix
		if e := json.Unmarshal(raw, &x); e != nil {
			return e
		}
		if x.Prefix == a.CIDR && x.vrfID() == poolVRF(d, p) {
			same = append(same, x)
		}
		return nil
	}); err != nil {
		return "", uncertainEnsure("listing prefixes at "+a.CIDR, err)
	}
	if len(same) > 0 {
		return "", errors.New("CIDR already occupied in managed VRF")
	}
	fields := ownedFields(a, opID)
	payload := map[string]any{"prefix": a.CIDR, "vrf": poolVRF(d, p), "status": "reserved", "custom_fields": fields}
	resp, err := c.request(ctx, http.MethodPost, "/api/ipam/prefixes/", payload)
	if err != nil {
		return c.recoverAfterCreateError(ctx, a, opID, poolVRF(d, p), err)
	}
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	resp.Body.Close()
	if readErr != nil {
		return c.recoverAfterCreateError(ctx, a, opID, poolVRF(d, p), readErr)
	}
	var x prefix
	if err := json.Unmarshal(data, &x); err != nil {
		return c.recoverAfterCreateError(ctx, a, opID, poolVRF(d, p), err)
	}
	if err := c.validateExisting(x, a, opID, poolVRF(d, p)); err != nil {
		return "", err
	}
	if x.ID == 0 {
		return "", errors.New("NetBox create returned no ID")
	}
	_ = d
	return strconv.Itoa(x.ID), nil
}
func (c *Client) recoverAfterCreateError(ctx context.Context, a domain.Allocation, opID string, vrfID int, original error) (string, error) {
	found, err := c.findByMarker(ctx, a.ID)
	if err == nil && len(found) == 1 {
		if e := c.validateExisting(found[0], a, opID, vrfID); e == nil {
			return strconv.Itoa(found[0].ID), nil
		} else {
			return "", e
		}
	}
	if len(found) > 1 {
		return "", errors.New("duplicate allocation marker after uncertain NetBox create")
	}
	return "", uncertainEnsure("recovering after a create that did not answer", original)
}

// uncertainEnsure wraps a request error Ensure cannot decide from. Adopt and
// its abandon already mark their own request errors this way (this file's
// sibling internal/netbox/adopt.go, through unanswered/uncertainAdopt);
// Ensure did not, so a NetBox 5xx or a timeout looked exactly like a definite
// refusal to the worker's classifier (uncertainInventoryError in
// internal/service/worker.go), and a stuck reservation could raise
// reservation_stuck on an inventory blip instead of being retried in silence
// -- the same defect the H2b review found and fixed for AbandonAdoption. A
// decision Ensure reaches on evidence it read -- a duplicate marker, a CIDR
// already occupied, a marker mismatch -- is never routed through this
// function, so those stay definite exactly as before.
func uncertainEnsure(what string, err error) error {
	if unanswered(err) {
		return fmt.Errorf("%w: uncertain NetBox reservation (operation remains pending): %s: %w", domain.ErrInventoryUncertain, what, err)
	}
	return fmt.Errorf("NetBox refused the reservation (operation remains pending): %s: %w", what, err)
}

// Sync updates only fields owned by the platform and preserves all unrelated
// NetBox metadata.
func (c *Client) Sync(ctx context.Context, a domain.Allocation) error {
	if a.InventoryID == "" {
		return errors.New("inventory ID is required")
	}
	d, p, err := c.allocationDomain(a)
	if err != nil {
		return err
	}
	_ = d
	resp, err := c.request(ctx, http.MethodGet, "/api/ipam/prefixes/"+url.PathEscape(a.InventoryID)+"/", nil)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	resp.Body.Close()
	if err != nil {
		return err
	}
	var x prefix
	if err := json.Unmarshal(data, &x); err != nil {
		return err
	}
	if x.vrfID() != poolVRF(d, p) || x.Prefix != a.CIDR || stringCF(x.CustomFields, allocationIDCF) != a.ID {
		return errors.New("NetBox prefix identity mismatch")
	}
	fields := ownedFields(a, stringCF(x.CustomFields, operationIDCF))
	// Optional owned fields must be explicitly cleared when the ledger no longer
	// carries them. NetBox renders unset custom fields as null.
	for _, key := range []string{"platform_aws_resource_id", "platform_last_observed_at", "platform_quarantine_until"} {
		if _, ok := fields[key]; !ok {
			fields[key] = nil
		}
	}
	stamp := "platform_last_observed_at"
	refreshStamp := shouldRefreshObservationStamp(fields[stamp], x.CustomFields[stamp], c.now(), c.projectionRefreshInterval)
	if !refreshStamp {
		// A different projection change may still require PATCH; never make it
		// regress or prematurely refresh the observation timestamp.
		fields[stamp] = x.CustomFields[stamp]
	}
	changed := false
	for key, value := range fields {
		if key == stamp {
			continue
		}
		if !sameProjectionValue(key, value, x.CustomFields[key]) {
			changed = true
			break
		}
	}
	for key, value := range x.CustomFields {
		if _, owned := fields[key]; !owned {
			fields[key] = value
		}
	}
	status := "reserved"
	if a.State == domain.Active {
		status = "active"
	}
	if !changed && !refreshStamp && string(x.Status) == status && c.projectionRefreshInterval > 0 {
		return nil
	}
	_, err = c.request(ctx, http.MethodPatch, "/api/ipam/prefixes/"+url.PathEscape(a.InventoryID)+"/", map[string]any{"status": status, "custom_fields": fields})
	return err
}

func sameProjectionValue(key string, desired, current any) bool {
	if desired == nil {
		return current == nil || current == ""
	}
	want, ok := desired.(string)
	if !ok {
		return false
	}
	if want == "" && current == nil {
		return true
	}
	got, ok := current.(string)
	if !ok {
		return false
	}
	if key == "platform_quarantine_until" {
		wt, we := time.Parse(time.RFC3339Nano, want)
		gt, ge := time.Parse(time.RFC3339Nano, got)
		return we == nil && ge == nil && wt.Equal(gt)
	}
	return want == got
}

func shouldRefreshObservationStamp(desired, current any, now time.Time, interval time.Duration) bool {
	if desired == nil {
		return current != nil && current != ""
	}
	want, ok := desired.(string)
	if !ok {
		return true
	}
	wt, err := time.Parse(time.RFC3339Nano, want)
	if err != nil {
		return true
	}
	got, ok := current.(string)
	if !ok || got == "" {
		return true
	}
	gt, err := time.Parse(time.RFC3339Nano, got)
	if err != nil {
		return true
	}
	return wt.After(gt) && now.Sub(gt) >= interval
}

// Delete verifies identity before deleting and confirms the object is absent.
func (c *Client) Delete(ctx context.Context, a domain.Allocation) error {
	if a.InventoryID == "" {
		return errors.New("inventory ID is required")
	}
	d, p, err := c.allocationDomain(a)
	if err != nil {
		return err
	}
	resp, err := c.request(ctx, http.MethodGet, "/api/ipam/prefixes/"+url.PathEscape(a.InventoryID)+"/", nil)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	resp.Body.Close()
	if err != nil {
		return err
	}
	var x prefix
	if err := json.Unmarshal(data, &x); err != nil {
		return err
	}
	if x.vrfID() != poolVRF(d, p) || x.Prefix != a.CIDR || stringCF(x.CustomFields, allocationIDCF) != a.ID {
		return errors.New("refusing to delete a non-owned NetBox prefix")
	}
	if _, err = c.request(ctx, http.MethodDelete, "/api/ipam/prefixes/"+url.PathEscape(a.InventoryID)+"/", nil); err != nil {
		found, e := c.findByMarker(ctx, a.ID)
		if e == nil && len(found) == 0 {
			return nil
		}
		return fmt.Errorf("uncertain NetBox delete: %w", err)
	}
	resp, err = c.request(ctx, http.MethodGet, "/api/ipam/prefixes/"+url.PathEscape(a.InventoryID)+"/", nil)
	if err == nil {
		resp.Body.Close()
		return errors.New("NetBox delete was not confirmed")
	}
	if strings.Contains(err.Error(), "HTTP 404") {
		return nil
	}
	return fmt.Errorf("NetBox delete confirmation failed: %w", err)
}

var _ domain.Inventory = (*Client)(nil)
