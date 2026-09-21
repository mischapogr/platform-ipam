package netbox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

// awsPluginStack is a minimal, stateful fake for netbox-aws-vpc-plugin plus
// the slice of core NetBox (prefixes, dcim regions) awsplugin.go reads, in
// the httptest style of client_test.go and occupancy_test.go: GET list
// endpoints answer from in-memory state filtered the same way the real
// plugin's filtersets do (exact match on the unique key), POST appends and
// echoes the created object, PATCH merges into the stored object. FK fields
// sent as plain integers (this file's own write convention) are normalized
// into the nested-object shape real NetBox echoes back, so decoding the
// fake's responses exercises the exact same code path a live NetBox would.
type awsPluginStack struct {
	mu           sync.Mutex
	notInstalled bool // when true every /api/plugins/ path 404s (ProbeAWSPlugin)
	prefixes     []map[string]any
	accounts     []map[string]any
	vpcs         []map[string]any
	subnets      []map[string]any
	regions      []map[string]any
	tagIDs       map[string]int
	nextID       int
	requests     []string
	bodies       []map[string]any
}

func (s *awsPluginStack) writes() []string {
	var out []string
	for _, r := range s.requests {
		if !strings.HasPrefix(r, http.MethodGet+" ") {
			out = append(out, r)
		}
	}
	return out
}

func awsToAny(m []map[string]any) []any {
	out := make([]any, len(m))
	for i, v := range m {
		out[i] = v
	}
	return out
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}

// normalizeAWSObject mirrors what real NetBox does between accepting a
// bare-PK write and rendering its nested-serializer read representation
// (this file's package comment): every single-object FK becomes {"id": n},
// every M2M entry becomes the same, and every tag payload entry gets an
// assigned ID. Must be called with s.mu held.
func (s *awsPluginStack) normalizeAWSObject(obj map[string]any) {
	for _, key := range []string{"owner_account", "region", "vpc_cidr", "subnet_cidr", "vpc"} {
		if v, ok := obj[key]; ok && v != nil {
			if n, ok := toInt(v); ok {
				obj[key] = map[string]any{"id": n}
			}
		}
	}
	if v, ok := obj["vpc_secondary_ipv4_cidrs"]; ok {
		if list, ok := v.([]any); ok {
			out := make([]any, 0, len(list))
			for _, item := range list {
				if n, ok := toInt(item); ok {
					out = append(out, map[string]any{"id": n})
				} else {
					out = append(out, item)
				}
			}
			obj["vpc_secondary_ipv4_cidrs"] = out
		}
	}
	if v, ok := obj["tags"]; ok {
		if list, ok := v.([]any); ok {
			out := make([]any, 0, len(list))
			for _, item := range list {
				m, _ := item.(map[string]any)
				slug, _ := m["slug"].(string)
				if slug == "" {
					continue
				}
				if s.tagIDs == nil {
					s.tagIDs = map[string]int{}
				}
				id, ok := s.tagIDs[slug]
				if !ok {
					s.nextID++
					id = s.nextID
					s.tagIDs[slug] = id
				}
				out = append(out, map[string]any{"id": id, "slug": slug})
			}
			obj["tags"] = out
		}
	}
}

func idOf(m map[string]any) int {
	n, _ := toInt(m["id"])
	return n
}

func (s *awsPluginStack) collection(w http.ResponseWriter, r *http.Request, items *[]map[string]any, keyField string) {
	if r.Method == http.MethodPost {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.bodies = append(s.bodies, body)
		s.nextID++
		obj := map[string]any{"id": s.nextID}
		for k, v := range body {
			obj[k] = v
		}
		s.normalizeAWSObject(obj)
		*items = append(*items, obj)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(obj)
		return
	}
	q := r.URL.Query().Get(keyField)
	var out []any
	for _, it := range *items {
		if q == "" || it[keyField] == q {
			out = append(out, it)
		}
	}
	w.Write(page(out, ""))
}

func (s *awsPluginStack) patch(w http.ResponseWriter, r *http.Request, items *[]map[string]any, idStr string) {
	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.bodies = append(s.bodies, body)
	for i, it := range *items {
		if idOf(it) != id {
			continue
		}
		for k, v := range body {
			(*items)[i][k] = v
		}
		s.normalizeAWSObject((*items)[i])
		b, _ := json.Marshal((*items)[i])
		w.Write(b)
		return
	}
	http.NotFound(w, r)
}

func (s *awsPluginStack) server(t *testing.T) *httptest.Server {
	t.Helper()
	if s.nextID == 0 {
		s.nextID = 500
	}
	return localServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		s.requests = append(s.requests, r.Method+" "+r.URL.Path)

		if s.notInstalled && strings.HasPrefix(r.URL.Path, "/api/plugins/") {
			http.NotFound(w, r)
			return
		}

		switch {
		case r.URL.Path == "/api/ipam/prefixes/" && r.Method == http.MethodGet:
			w.Write(page(awsToAny(s.prefixes), ""))
			return
		case r.URL.Path == awsPluginAccountsPath:
			s.collection(w, r, &s.accounts, "account_id")
			return
		case r.URL.Path == awsPluginVPCsPath:
			s.collection(w, r, &s.vpcs, "vpc_id")
			return
		case r.URL.Path == awsPluginSubnetsPath:
			s.collection(w, r, &s.subnets, "subnet_id")
			return
		case r.URL.Path == dcimRegionsPath:
			s.collection(w, r, &s.regions, "slug")
			return
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, awsPluginAccountsPath):
			s.patch(w, r, &s.accounts, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, awsPluginAccountsPath), "/"))
			return
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, awsPluginVPCsPath):
			s.patch(w, r, &s.vpcs, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, awsPluginVPCsPath), "/"))
			return
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, awsPluginSubnetsPath):
			s.patch(w, r, &s.subnets, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, awsPluginSubnetsPath), "/"))
			return
		}
		http.NotFound(w, r)
	}))
}

func awsPluginClient(t *testing.T, stack *awsPluginStack) (*Client, domain.Domain, func()) {
	t.Helper()
	d, p := testDomain()
	s := stack.server(t)
	c, err := New(Config{BaseURL: s.URL, Domains: []domain.Domain{d}, Pools: []domain.Pool{p}})
	if err != nil {
		s.Close()
		t.Fatal(err)
	}
	return c, d, s.Close
}

func seedPrefix(id int, cidr string, vrfID int) map[string]any {
	return map[string]any{"id": id, "prefix": cidr, "vrf": map[string]any{"id": vrfID}, "status": "active"}
}

// --- ProbeAWSPlugin ---

func TestProbeAWSPluginDetectsNotInstalled(t *testing.T) {
	stack := &awsPluginStack{notInstalled: true}
	c, _, closer := awsPluginClient(t, stack)
	defer closer()
	err := c.ProbeAWSPlugin(context.Background())
	if !errors.Is(err, ErrAWSPluginAccountsUnavailable) {
		t.Fatalf("got %v, want ErrAWSPluginAccountsUnavailable", err)
	}
}

func TestProbeAWSPluginOK(t *testing.T) {
	stack := &awsPluginStack{}
	c, _, closer := awsPluginClient(t, stack)
	defer closer()
	if err := c.ProbeAWSPlugin(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// --- EnsureAWSAccount ---

func TestEnsureAWSAccountCreatesThenRepeatsUnchanged(t *testing.T) {
	stack := &awsPluginStack{}
	c, _, closer := awsPluginClient(t, stack)
	defer closer()

	got, err := c.EnsureAWSAccount(context.Background(), AWSAccountSpec{AccountID: "123456789012", Name: "Team A"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Action != AWSObjectCreated || got.ID == "" {
		t.Fatalf("unexpected result %+v", got)
	}
	writes := stack.writes()
	if len(writes) != 1 || writes[0] != "POST "+awsPluginAccountsPath {
		t.Fatalf("unexpected writes %v", writes)
	}
	body := stack.bodies[0]
	if body["account_id"] != "123456789012" || body["name"] != "Team A" {
		t.Fatalf("unexpected create body: %#v", body)
	}
	tags, _ := body["tags"].([]any)
	if len(tags) != 1 {
		t.Fatalf("expected exactly the import tag: %#v", body["tags"])
	}
	if tag, _ := tags[0].(map[string]any); tag["slug"] != ImportedTag {
		t.Fatalf("import tag missing: %#v", tags[0])
	}

	stack.requests, stack.bodies = nil, nil
	got2, err := c.EnsureAWSAccount(context.Background(), AWSAccountSpec{AccountID: "123456789012", Name: "Team A"})
	if err != nil {
		t.Fatal(err)
	}
	if got2 != (AWSObjectResult{ID: got.ID, Action: AWSObjectUnchanged}) {
		t.Fatalf("unexpected repeat result %+v", got2)
	}
	if writes := stack.writes(); len(writes) != 0 {
		t.Fatalf("a repeated ensure wrote %v", writes)
	}
}

func TestEnsureAWSAccountUpdatesOwnedFieldsOnly(t *testing.T) {
	stack := &awsPluginStack{accounts: []map[string]any{
		{"id": 900, "account_id": "123456789012", "name": "Old Name", "tags": []any{}},
	}}
	c, _, closer := awsPluginClient(t, stack)
	defer closer()

	got, err := c.EnsureAWSAccount(context.Background(), AWSAccountSpec{AccountID: "123456789012", Name: "New Name"})
	if err != nil {
		t.Fatal(err)
	}
	if got != (AWSObjectResult{ID: "900", Action: AWSObjectUpdated}) {
		t.Fatalf("unexpected result %+v", got)
	}
	writes := stack.writes()
	if len(writes) != 1 || writes[0] != "PATCH "+awsPluginAccountsPath+"900/" {
		t.Fatalf("unexpected writes %v", writes)
	}
	body := stack.bodies[0]
	if body["name"] != "New Name" {
		t.Fatalf("name was not updated: %#v", body)
	}
	tags, _ := body["tags"].([]any)
	if len(tags) != 1 {
		t.Fatalf("expected the import tag to be added: %#v", body["tags"])
	}

	// A second Ensure with the same name and the tag now present writes
	// nothing: every owned field already matches.
	stack.requests, stack.bodies = nil, nil
	if _, err := c.EnsureAWSAccount(context.Background(), AWSAccountSpec{AccountID: "123456789012", Name: "New Name"}); err != nil {
		t.Fatal(err)
	}
	if writes := stack.writes(); len(writes) != 0 {
		t.Fatalf("unchanged ensure wrote %v", writes)
	}

	// An empty Name never blanks an existing one out.
	stack.requests, stack.bodies = nil, nil
	if _, err := c.EnsureAWSAccount(context.Background(), AWSAccountSpec{AccountID: "123456789012"}); err != nil {
		t.Fatal(err)
	}
	if writes := stack.writes(); len(writes) != 0 {
		t.Fatalf("an empty Name should leave the account untouched, wrote %v", writes)
	}
}

// --- EnsureAWSRegion ---

func TestEnsureAWSRegionCreatesThenRepeatsUnchanged(t *testing.T) {
	stack := &awsPluginStack{}
	c, _, closer := awsPluginClient(t, stack)
	defer closer()

	got, err := c.EnsureAWSRegion(context.Background(), "eu-central-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Action != AWSObjectCreated {
		t.Fatalf("unexpected result %+v", got)
	}
	body := stack.bodies[0]
	if body["name"] != "eu-central-1" || body["slug"] != "eu-central-1" {
		t.Fatalf("unexpected create body: %#v", body)
	}

	stack.requests, stack.bodies = nil, nil
	got2, err := c.EnsureAWSRegion(context.Background(), "eu-central-1")
	if err != nil {
		t.Fatal(err)
	}
	if got2 != (AWSObjectResult{ID: got.ID, Action: AWSObjectUnchanged}) {
		t.Fatalf("unexpected repeat result %+v", got2)
	}
	if writes := stack.writes(); len(writes) != 0 {
		t.Fatalf("a repeated ensure wrote %v", writes)
	}
}

// --- EnsureAWSVPC ---

func TestEnsureAWSVPCRequiresAnExistingPrefix(t *testing.T) {
	stack := &awsPluginStack{}
	c, d, closer := awsPluginClient(t, stack)
	defer closer()

	_, err := c.EnsureAWSVPC(context.Background(), d, AWSVPCSpec{
		VPCID: "vpc-0abc", OwnerAccountNetBoxID: 1, PrimaryCIDR: "10.1.0.0/16",
	})
	if !errors.Is(err, ErrAWSObjectInvalid) {
		t.Fatalf("got %v, want ErrAWSObjectInvalid", err)
	}
	if !strings.Contains(err.Error(), "10.1.0.0/16") {
		t.Fatalf("error does not name the CIDR: %v", err)
	}
	if writes := stack.writes(); len(writes) != 0 {
		t.Fatalf("refusal still wrote %v", writes)
	}
}

func TestEnsureAWSVPCCreatesThenRepeatsUnchanged(t *testing.T) {
	stack := &awsPluginStack{prefixes: []map[string]any{seedPrefix(501, "10.1.0.0/16", 7)}}
	c, d, closer := awsPluginClient(t, stack)
	defer closer()

	accRes, err := c.EnsureAWSAccount(context.Background(), AWSAccountSpec{AccountID: "123456789012"})
	if err != nil {
		t.Fatal(err)
	}
	accID, _ := strconv.Atoi(accRes.ID)

	regRes, err := c.EnsureAWSRegion(context.Background(), "eu-central-1")
	if err != nil {
		t.Fatal(err)
	}
	regID, _ := strconv.Atoi(regRes.ID)

	stack.requests, stack.bodies = nil, nil
	got, err := c.EnsureAWSVPC(context.Background(), d, AWSVPCSpec{
		VPCID: "vpc-0abc", Name: "legacy-vpc", OwnerAccountNetBoxID: accID, RegionNetBoxID: regID, PrimaryCIDR: "10.1.0.0/16",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Action != AWSObjectCreated {
		t.Fatalf("unexpected result %+v", got)
	}
	var createBody map[string]any
	for _, b := range stack.bodies {
		if b["vpc_id"] == "vpc-0abc" {
			createBody = b
		}
	}
	if createBody == nil {
		t.Fatalf("no create body found among %#v", stack.bodies)
	}
	if createBody["vpc_cidr"] != float64(501) || createBody["owner_account"] != float64(accID) || createBody["region"] != float64(regID) {
		t.Fatalf("unexpected create body: %#v", createBody)
	}

	stack.requests, stack.bodies = nil, nil
	got2, err := c.EnsureAWSVPC(context.Background(), d, AWSVPCSpec{
		VPCID: "vpc-0abc", Name: "legacy-vpc", OwnerAccountNetBoxID: accID, RegionNetBoxID: regID, PrimaryCIDR: "10.1.0.0/16",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got2 != (AWSObjectResult{ID: got.ID, Action: AWSObjectUnchanged}) {
		t.Fatalf("unexpected repeat result %+v", got2)
	}
	if writes := stack.writes(); len(writes) != 0 {
		t.Fatalf("a repeated ensure wrote %v", writes)
	}
}

// TestEnsureAWSVPCTwoVPCsShareOnePrefix is package N3's central scenario
// (ADR 0009): vpc_cidr is a plain ForeignKey, not one-to-one, so the same
// CIDR collapsed to one prefix by onboarding import (the duplicate-CIDR
// rule, docs/ONBOARDING_IMPORT.md section 5) can carry two AWSVPC objects,
// each with its own owner_account, restoring the per-account picture the
// collapse would otherwise have destroyed.
func TestEnsureAWSVPCTwoVPCsShareOnePrefix(t *testing.T) {
	stack := &awsPluginStack{prefixes: []map[string]any{seedPrefix(501, "10.1.0.0/16", 7)}}
	c, d, closer := awsPluginClient(t, stack)
	defer closer()

	acc1, err := c.EnsureAWSAccount(context.Background(), AWSAccountSpec{AccountID: "111111111111"})
	if err != nil {
		t.Fatal(err)
	}
	acc2, err := c.EnsureAWSAccount(context.Background(), AWSAccountSpec{AccountID: "222222222222"})
	if err != nil {
		t.Fatal(err)
	}
	acc1ID, _ := strconv.Atoi(acc1.ID)
	acc2ID, _ := strconv.Atoi(acc2.ID)

	vpc1, err := c.EnsureAWSVPC(context.Background(), d, AWSVPCSpec{VPCID: "vpc-account1", OwnerAccountNetBoxID: acc1ID, PrimaryCIDR: "10.1.0.0/16"})
	if err != nil {
		t.Fatal(err)
	}
	vpc2, err := c.EnsureAWSVPC(context.Background(), d, AWSVPCSpec{VPCID: "vpc-account2", OwnerAccountNetBoxID: acc2ID, PrimaryCIDR: "10.1.0.0/16"})
	if err != nil {
		t.Fatal(err)
	}
	if vpc1.ID == vpc2.ID {
		t.Fatalf("expected two distinct AWSVPC objects, got the same ID %s twice", vpc1.ID)
	}
	if len(stack.vpcs) != 2 {
		t.Fatalf("expected 2 stored AWS VPCs, got %d: %#v", len(stack.vpcs), stack.vpcs)
	}
	for _, v := range stack.vpcs {
		cidrRef, _ := v["vpc_cidr"].(map[string]any)
		if idOf(cidrRef) != 501 {
			t.Fatalf("VPC %v does not point at the shared prefix: %#v", v["vpc_id"], v["vpc_cidr"])
		}
	}
}

// --- EnsureAWSSubnet ---

func TestEnsureAWSSubnetRequiresVPCAndOwnerAccount(t *testing.T) {
	stack := &awsPluginStack{}
	c, d, closer := awsPluginClient(t, stack)
	defer closer()

	if _, err := c.EnsureAWSSubnet(context.Background(), d, AWSSubnetSpec{SubnetID: "subnet-0abc", OwnerAccountNetBoxID: 1}); !errors.Is(err, ErrAWSObjectInvalid) {
		t.Fatalf("missing VPC: got %v, want ErrAWSObjectInvalid", err)
	}
	if _, err := c.EnsureAWSSubnet(context.Background(), d, AWSSubnetSpec{SubnetID: "subnet-0abc", VPCNetBoxID: 1}); !errors.Is(err, ErrAWSObjectInvalid) {
		t.Fatalf("missing owner account: got %v, want ErrAWSObjectInvalid", err)
	}
	if writes := stack.writes(); len(writes) != 0 {
		t.Fatalf("refusals still wrote %v", writes)
	}
}

func TestEnsureAWSSubnetCreatesThenRepeatsUnchangedAndNeverWritesAZ(t *testing.T) {
	stack := &awsPluginStack{prefixes: []map[string]any{
		seedPrefix(501, "10.1.0.0/16", 7),
		seedPrefix(502, "10.1.1.0/24", 7),
	}}
	c, d, closer := awsPluginClient(t, stack)
	defer closer()

	accRes, err := c.EnsureAWSAccount(context.Background(), AWSAccountSpec{AccountID: "123456789012"})
	if err != nil {
		t.Fatal(err)
	}
	accID, _ := strconv.Atoi(accRes.ID)
	vpcRes, err := c.EnsureAWSVPC(context.Background(), d, AWSVPCSpec{VPCID: "vpc-0abc", OwnerAccountNetBoxID: accID, PrimaryCIDR: "10.1.0.0/16"})
	if err != nil {
		t.Fatal(err)
	}
	vpcID, _ := strconv.Atoi(vpcRes.ID)

	stack.requests, stack.bodies = nil, nil
	got, err := c.EnsureAWSSubnet(context.Background(), d, AWSSubnetSpec{
		SubnetID: "subnet-0xyz", Name: "legacy-subnet", VPCNetBoxID: vpcID, OwnerAccountNetBoxID: accID,
		CIDR: "10.1.1.0/24", AvailabilityZone: "eu-central-1a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Action != AWSObjectCreated {
		t.Fatalf("unexpected result %+v", got)
	}
	body := stack.bodies[len(stack.bodies)-1]
	if body["subnet_id"] != "subnet-0xyz" || body["vpc"] != float64(vpcID) || body["owner_account"] != float64(accID) || body["subnet_cidr"] != float64(502) {
		t.Fatalf("unexpected create body: %#v", body)
	}

	stack.requests, stack.bodies = nil, nil
	got2, err := c.EnsureAWSSubnet(context.Background(), d, AWSSubnetSpec{
		SubnetID: "subnet-0xyz", Name: "legacy-subnet", VPCNetBoxID: vpcID, OwnerAccountNetBoxID: accID,
		CIDR: "10.1.1.0/24", AvailabilityZone: "eu-central-1a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got2 != (AWSObjectResult{ID: got.ID, Action: AWSObjectUnchanged}) {
		t.Fatalf("unexpected repeat result %+v", got2)
	}
	if writes := stack.writes(); len(writes) != 0 {
		t.Fatalf("a repeated ensure wrote %v", writes)
	}

	// AWSSubnet has no availability-zone field in this plugin version: no
	// request this test made may mention it, under any key spelling.
	for _, b := range stack.bodies {
		raw, _ := json.Marshal(b)
		if strings.Contains(strings.ToLower(string(raw)), "availab") || strings.Contains(strings.ToLower(string(raw)), "\"az\"") {
			t.Fatalf("a request body mentions an availability zone, which the plugin cannot store: %s", raw)
		}
	}
}

func TestLookupAWSVPCFindsAnExistingVPCWithoutWriting(t *testing.T) {
	stack := &awsPluginStack{vpcs: []map[string]any{
		{"id": 700, "vpc_id": "vpc-existing", "owner_account": map[string]any{"id": 1}, "tags": []any{}},
	}}
	c, _, closer := awsPluginClient(t, stack)
	defer closer()

	res, found, err := c.LookupAWSVPC(context.Background(), "vpc-existing")
	if err != nil {
		t.Fatal(err)
	}
	if !found || res.ID != "700" {
		t.Fatalf("unexpected result %+v found=%v", res, found)
	}
	if writes := stack.writes(); len(writes) != 0 {
		t.Fatalf("a pure lookup wrote %v", writes)
	}

	_, found, err = c.LookupAWSVPC(context.Background(), "vpc-missing")
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatalf("expected vpc-missing not to be found")
	}
}
