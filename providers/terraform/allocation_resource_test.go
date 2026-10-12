package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/mischapogr/platform-ipam/providers/terraform/internal/client"
)

func TestAllocationSchemaHasReplacementModifiersAndTimeouts(t *testing.T) {
	allocationSchemaValue := allocationSchema(context.Background())
	for _, name := range []string{"allocation_key", "scope", "prefix_length", "parent_allocation_id"} {
		attribute, ok := allocationSchemaValue.Attributes[name]
		if !ok {
			t.Fatalf("missing %s", name)
		}
		switch value := attribute.(type) {
		case schema.StringAttribute:
			if len(value.PlanModifiers) == 0 {
				t.Fatalf("%s must require replacement", name)
			}
		case schema.Int64Attribute:
			if len(value.PlanModifiers) == 0 {
				t.Fatalf("%s must require replacement", name)
			}
		default:
			t.Fatalf("unexpected schema type for %s: %T", name, attribute)
		}
	}
	if _, ok := allocationSchemaValue.Attributes["timeouts"]; !ok {
		t.Fatal("timeouts attribute missing")
	}
}

func TestModifyPlanRejectsImmutableChangeWithSameAllocationKey(t *testing.T) {
	ctx := context.Background()
	s := allocationSchema(ctx)
	state := tfsdk.State{Schema: s}
	prior := allocationModel{AllocationKey: types.StringValue("orders-v1"), Scope: types.StringValue("vpc"), Environment: types.StringValue("prod"), Region: types.StringValue("eu-central-1"), AccountID: types.StringValue("123456789012"), AddressFamily: types.StringValue("ipv4"), PrefixLength: types.Int64Value(20)}
	prior.Labels = types.MapNull(types.StringType)
	prior.Timeouts = timeouts.Value{Object: types.ObjectNull(map[string]attr.Type{"create": types.StringType, "update": types.StringType, "delete": types.StringType})}
	if diags := state.Set(ctx, &prior); diags.HasError() {
		t.Fatal(diags)
	}
	plan := tfsdk.Plan{Schema: s}
	planned := prior
	planned.PrefixLength = types.Int64Value(21)
	if diags := plan.Set(ctx, &planned); diags.HasError() {
		t.Fatal(diags)
	}
	resp := &resource.ModifyPlanResponse{}
	(&allocationResource{}).ModifyPlan(ctx, resource.ModifyPlanRequest{State: state, Plan: plan}, resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected same-key replacement diagnostic")
	}
}

func TestModifyPlanRejectsOptionalImmutableChangesWithSameKey(t *testing.T) {
	for _, name := range []string{"add parent", "remove parent", "remove availability zone"} {
		t.Run(name, func(t *testing.T) {
			prior := testAllocationModel()
			planned := prior
			switch name {
			case "add parent":
				planned.ParentAllocationID = types.StringValue("alloc_parent")
			case "remove parent":
				prior.ParentAllocationID = types.StringValue("alloc_parent")
			case "remove availability zone":
				prior.AvailabilityZoneID = types.StringValue("euc1-az1")
			}
			response := modifyAllocationPlan(t, prior, &planned)
			if !response.Diagnostics.HasError() {
				t.Fatal("optional immutable change reused its key")
			}
		})
	}
}

func TestModifyPlanRetiredAllocationsAllowDestroyOrNewGeneration(t *testing.T) {
	for _, lifecycle := range []string{"QUARANTINED", "RELEASED"} {
		prior := testAllocationModel()
		prior.AllocationState = types.StringValue(lifecycle)
		planned := prior
		if response := modifyAllocationPlan(t, prior, &planned); !response.Diagnostics.HasError() {
			t.Fatalf("%s allocation allowed provisioning", lifecycle)
		}
		if response := modifyAllocationPlan(t, prior, nil); response.Diagnostics.HasError() {
			t.Fatalf("%s allocation cannot be destroyed: %v", lifecycle, response.Diagnostics)
		}
		planned.AllocationKey = types.StringValue("orders-v2")
		if response := modifyAllocationPlan(t, prior, &planned); response.Diagnostics.HasError() {
			t.Fatalf("new generation refused: %v", response.Diagnostics)
		}
	}
}

func TestModifyPlanPreservesStableIdentityOnlyForMetadataUpdate(t *testing.T) {
	prior := testAllocationModel()
	prior.ID, prior.CIDR, prior.PoolID = types.StringValue("alloc_01"), types.StringValue("10.0.0.0/20"), types.StringValue("pool_01")
	planned := prior
	planned.Description = types.StringValue("updated")
	planned.ID, planned.CIDR, planned.PoolID = types.StringUnknown(), types.StringUnknown(), types.StringUnknown()
	planned.AllocationState, planned.BoundResourceID = types.StringUnknown(), types.StringUnknown()
	response := modifyAllocationPlan(t, prior, &planned)
	if response.Diagnostics.HasError() {
		t.Fatal(response.Diagnostics)
	}
	var result allocationModel
	if diags := response.Plan.Get(context.Background(), &result); diags.HasError() {
		t.Fatal(diags)
	}
	if !result.ID.Equal(prior.ID) || !result.CIDR.Equal(prior.CIDR) || !result.PoolID.Equal(prior.PoolID) || !result.AllocationState.IsUnknown() || !result.BoundResourceID.IsUnknown() {
		t.Fatalf("unexpected metadata update plan: %+v", result)
	}
	for _, replacement := range []string{"new key", "unknown placement"} {
		if replacement == "new key" {
			planned.AllocationKey = types.StringValue("orders-v2")
		} else {
			planned.AllocationKey = prior.AllocationKey
			planned.ParentAllocationID = types.StringUnknown()
		}
		response = modifyAllocationPlan(t, prior, &planned)
		if diags := response.Plan.Get(context.Background(), &result); diags.HasError() {
			t.Fatal(diags)
		}
		if !result.ID.IsUnknown() || !result.CIDR.IsUnknown() {
			t.Fatalf("%s copied old identity into a new or uncertain allocation", replacement)
		}
	}
}

func TestValidateConfigChecksScopeAndMetadataWithoutConfiguredClient(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*allocationModel)
		invalid bool
	}{
		{"vpc", func(*allocationModel) {}, false},
		{"vpc parent", func(m *allocationModel) { m.ParentAllocationID = types.StringValue("alloc_parent") }, true},
		{"subnet missing parent", func(m *allocationModel) { m.Scope = types.StringValue("subnet") }, true},
		{"subnet unknown parent", func(m *allocationModel) {
			m.Scope = types.StringValue("subnet")
			m.ParentAllocationID = types.StringUnknown()
		}, false},
		{"long description", func(m *allocationModel) { m.Description = types.StringValue(strings.Repeat("a", 513)) }, true},
		{"reserved label", func(m *allocationModel) {
			m.Labels = types.MapValueMust(types.StringType, map[string]attr.Value{"platform-ipam/owner": types.StringValue("orders")})
		}, true},
		{"null label", func(m *allocationModel) {
			m.Labels = types.MapValueMust(types.StringType, map[string]attr.Value{"service": types.StringNull()})
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			state := tfsdk.State{Schema: allocationSchema(ctx)}
			model := testAllocationModel()
			tc.change(&model)
			if diags := state.Set(ctx, &model); diags.HasError() {
				t.Fatal(diags)
			}
			response := resource.ValidateConfigResponse{}
			(&allocationResource{}).ValidateConfig(ctx, resource.ValidateConfigRequest{Config: tfsdk.Config{Schema: state.Schema, Raw: state.Raw}}, &response)
			if response.Diagnostics.HasError() != tc.invalid {
				t.Fatalf("diagnostics=%v expected-invalid=%v", response.Diagnostics, tc.invalid)
			}
		})
	}
}

func TestReserveKeyFitsAPIForMaximumLengthAllocationKey(t *testing.T) {
	key := strings.Repeat("a", 128)
	got := reserveKey(key)
	if len(got) > 128 || got != reserveKey(key) || got == reserveKey(strings.Repeat("b", 128)) {
		t.Fatalf("maximum-length key is not stable, bounded, and distinct: %q", got)
	}
	if reserveKey("orders-v1") != "reserve-orders-v1" {
		t.Fatal("existing short request identity changed")
	}
}

func modifyAllocationPlan(t *testing.T, prior allocationModel, planned *allocationModel) *resource.ModifyPlanResponse {
	t.Helper()
	ctx := context.Background()
	s := allocationSchema(ctx)
	state := tfsdk.State{Schema: s}
	if diags := state.Set(ctx, &prior); diags.HasError() {
		t.Fatal(diags)
	}
	plan := tfsdk.Plan{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	if planned != nil {
		if diags := plan.Set(ctx, planned); diags.HasError() {
			t.Fatal(diags)
		}
	}
	response := &resource.ModifyPlanResponse{Plan: plan}
	(&allocationResource{}).ModifyPlan(ctx, resource.ModifyPlanRequest{State: state, Plan: plan}, response)
	return response
}

func TestReadRetainsStateOnAmbiguous404(t *testing.T) {
	t.Setenv("PLATFORM_IPAM_ALLOW_LOCAL_HTTP", "1")
	server := newResourceTestServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) }))
	defer server.Close()
	api, err := client.New(client.Config{Endpoint: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s := allocationSchema(ctx)
	state := tfsdk.State{Schema: s}
	model := testAllocationModel()
	if diags := state.Set(ctx, &model); diags.HasError() {
		t.Fatal(diags)
	}
	response := resource.ReadResponse{State: state}
	(&allocationResource{api: api}).Read(ctx, resource.ReadRequest{State: state}, &response)
	if !response.Diagnostics.HasError() || response.State.Raw.IsNull() {
		t.Fatalf("diagnostics=%v state removed=%v", response.Diagnostics, response.State.Raw.IsNull())
	}
}

// TestReadRetainsStateOnAllocationPendingAndNamesTheOperation is package E3's
// provider test, written to demonstrate what Read already did before this
// package for ANY GetAllocation error (retain state; add a diagnostic; never
// call resp.State.RemoveResource) still holds for the new 409
// allocation_pending code, and that the diagnostic additionally names the
// pending operation rather than the generic "not proven absent" wording a
// 409 would otherwise get. A 409 must never be treated like the 404 case
// above in the one way that would matter -- removing the resource from
// state -- and this proves neither case ever does that.
func TestReadRetainsStateOnAllocationPendingAndNamesTheOperation(t *testing.T) {
	t.Setenv("PLATFORM_IPAM_ALLOW_LOCAL_HTTP", "1")
	server := newResourceTestServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"allocation_pending","message":"still pending","retryable":true,"details":{"operation_id":"op_still_pending_01"}}}`))
	}))
	defer server.Close()
	api, err := client.New(client.Config{Endpoint: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s := allocationSchema(ctx)
	state := tfsdk.State{Schema: s}
	model := testAllocationModel()
	if diags := state.Set(ctx, &model); diags.HasError() {
		t.Fatal(diags)
	}
	response := resource.ReadResponse{State: state}
	(&allocationResource{api: api}).Read(ctx, resource.ReadRequest{State: state}, &response)
	if !response.Diagnostics.HasError() || response.State.Raw.IsNull() {
		t.Fatalf("diagnostics=%v state removed=%v", response.Diagnostics, response.State.Raw.IsNull())
	}
	if !strings.Contains(response.Diagnostics[0].Detail(), "op_still_pending_01") {
		t.Fatalf("diagnostic detail=%q, want it to name the pending operation", response.Diagnostics[0].Detail())
	}
	if !strings.Contains(response.Diagnostics[0].Summary(), "pending") {
		t.Fatalf("diagnostic summary=%q, want it to distinguish this from an ordinary refresh failure", response.Diagnostics[0].Summary())
	}
}

func TestCreateMetadataFailureKeepsCommittedIdentityAndTaintRecoveryGuidance(t *testing.T) {
	t.Setenv("PLATFORM_IPAM_ALLOW_LOCAL_HTTP", "1")
	server := newResourceTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			w.Header().Set("ETag", `"1"`)
			_, _ = w.Write([]byte(`{"id":"alloc_01","allocation_key":"orders-v1","scope":"vpc","environment":"prod","region":"eu-central-1","account_id":"123456789012","address_family":"ipv4","prefix_length":20,"cidr":"10.0.0.0/20","state":"RESERVED","revision":1,"description":"old","labels":{}}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"invalid_request","message":"patch rejected"}}`))
	}))
	defer server.Close()
	api, err := client.New(client.Config{Endpoint: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s := allocationSchema(ctx)
	plan := tfsdk.Plan{Schema: s}
	model := testAllocationModel()
	model.Description = types.StringValue("desired")
	if diags := plan.Set(ctx, &model); diags.HasError() {
		t.Fatal(diags)
	}
	response := resource.CreateResponse{State: tfsdk.State{Schema: s}}
	(&allocationResource{api: api}).Create(ctx, resource.CreateRequest{Plan: plan}, &response)
	if !response.Diagnostics.HasError() || !strings.Contains(response.Diagnostics[0].Summary(), "metadata") {
		t.Fatalf("diagnostics=%v", response.Diagnostics)
	}
	if response.State.Raw.IsNull() {
		t.Fatal("committed allocation identity was lost")
	}
	if !strings.Contains(response.Diagnostics[0].Detail(), "tainted") {
		t.Fatalf("missing taint recovery guidance: %v", response.Diagnostics)
	}
}

func TestCreateDoesNotRecoverAnAllocationWithDifferentImmutableIdentity(t *testing.T) {
	t.Setenv("PLATFORM_IPAM_ALLOW_LOCAL_HTTP", "1")
	server := newResourceTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"allocation_key_conflict","message":"immutable request differs"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []client.Allocation{{ID: "alloc_other", AllocationKey: "orders-v1", Scope: "vpc", Environment: "prod", Region: "eu-central-1", AccountID: "123456789012", AddressFamily: "ipv4", PrefixLength: 21, CIDR: "10.0.0.0/21", State: "RESERVED"}}})
	}))
	defer server.Close()
	api, err := client.New(client.Config{Endpoint: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s := allocationSchema(ctx)
	plan := tfsdk.Plan{Schema: s}
	model := testAllocationModel()
	if diags := plan.Set(ctx, &model); diags.HasError() {
		t.Fatal(diags)
	}
	response := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}}
	(&allocationResource{api: api}).Create(ctx, resource.CreateRequest{Plan: plan}, &response)
	if !response.Diagnostics.HasError() || !response.State.Raw.IsNull() {
		t.Fatalf("mismatched existing allocation entered state: %v", response.Diagnostics)
	}
}

func TestMetadataRevisionConflictAndAmbiguousRetryKeepCorrectKeys(t *testing.T) {
	t.Setenv("PLATFORM_IPAM_ALLOW_LOCAL_HTTP", "1")
	var keys, etags []string
	server := newResourceTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPatch {
			keys = append(keys, r.Header.Get("Idempotency-Key"))
			etags = append(etags, r.Header.Get("If-Match"))
			switch len(keys) {
			case 1:
				w.WriteHeader(http.StatusPreconditionFailed)
				_, _ = w.Write([]byte(`{"error":{"code":"revision_conflict","message":"stale revision"}}`))
				return
			case 2:
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"error":{"code":"dependency_unavailable","message":"response uncertain"}}`))
				return
			}
		}
		allocation := client.Allocation{ID: "alloc_01", AllocationKey: "orders-v1", State: "RESERVED", CIDR: "10.0.0.0/20", Revision: 2, Description: "old"}
		if r.Method == http.MethodPatch {
			allocation.Description = "desired"
			allocation.Revision = 3
		}
		_ = json.NewEncoder(w).Encode(allocation)
	}))
	defer server.Close()
	api, err := client.New(client.Config{Endpoint: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	desired := testAllocationModel()
	desired.Description = types.StringValue("desired")
	updated, err := (&allocationResource{api: api}).updateMetadata(context.Background(), &client.Allocation{ID: "alloc_01", ETag: `"1"`}, desired)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Description != "desired" || len(keys) != 3 || keys[0] == keys[1] || keys[1] != keys[2] || etags[0] != `"1"` || etags[1] != `"2"` || etags[1] != etags[2] {
		t.Fatalf("updated=%+v keys=%v etags=%v", updated, keys, etags)
	}
}

func TestImportUsesPlatformIDAndDoesNotReserve(t *testing.T) {
	ctx := context.Background()
	s := allocationSchema(ctx)
	state := tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)}
	response := resource.ImportStateResponse{State: state}
	(&allocationResource{}).ImportState(ctx, resource.ImportStateRequest{ID: "alloc_01"}, &response)
	if response.Diagnostics.HasError() || response.State.Raw.IsNull() {
		t.Fatalf("import diagnostics=%v state-null=%v", response.Diagnostics, response.State.Raw.IsNull())
	}
	var imported allocationModel
	if diags := response.State.Get(ctx, &imported); diags.HasError() {
		t.Fatal(diags)
	}
	if imported.ID.ValueString() != "alloc_01" {
		t.Fatalf("imported ID=%q", imported.ID.ValueString())
	}
}

func TestDeleteRemovesStateAfterDurableQuarantineAcceptance(t *testing.T) {
	t.Setenv("PLATFORM_IPAM_ALLOW_LOCAL_HTTP", "1")
	server := newResourceTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"alloc_01","state":"QUARANTINED"}`))
	}))
	defer server.Close()
	api, err := client.New(client.Config{Endpoint: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s := allocationSchema(ctx)
	state := tfsdk.State{Schema: s}
	model := testAllocationModel()
	model.ID = types.StringValue("alloc_01")
	model.AllocationState = types.StringValue("RESERVED")
	if diags := state.Set(ctx, &model); diags.HasError() {
		t.Fatal(diags)
	}
	response := resource.DeleteResponse{State: state}
	(&allocationResource{api: api}).Delete(ctx, resource.DeleteRequest{State: state}, &response)
	if response.Diagnostics.HasError() || !response.State.Raw.IsNull() {
		t.Fatalf("delete diagnostics=%v state-removed=%v", response.Diagnostics, response.State.Raw.IsNull())
	}
}

func TestDeleteRetainsStateWithoutMatchingQuarantineReceipt(t *testing.T) {
	t.Setenv("PLATFORM_IPAM_ALLOW_LOCAL_HTTP", "1")
	server := newResourceTestServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"alloc_01","state":"RESERVED"}`))
	}))
	defer server.Close()
	api, err := client.New(client.Config{Endpoint: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	state := tfsdk.State{Schema: allocationSchema(ctx)}
	model := testAllocationModel()
	model.ID = types.StringValue("alloc_01")
	if diags := state.Set(ctx, &model); diags.HasError() {
		t.Fatal(diags)
	}
	response := resource.DeleteResponse{State: state}
	(&allocationResource{api: api}).Delete(ctx, resource.DeleteRequest{State: state}, &response)
	if !response.Diagnostics.HasError() || !response.State.Raw.Equal(state.Raw) {
		t.Fatalf("state changed without durable release: %v", response.Diagnostics)
	}
}

func testAllocationModel() allocationModel {
	return allocationModel{AllocationKey: types.StringValue("orders-v1"), Scope: types.StringValue("vpc"), Environment: types.StringValue("prod"), Region: types.StringValue("eu-central-1"), AccountID: types.StringValue("123456789012"), AddressFamily: types.StringValue("ipv4"), PrefixLength: types.Int64Value(20), Description: types.StringValue(""), Labels: types.MapNull(types.StringType), Timeouts: timeouts.Value{Object: types.ObjectNull(map[string]attr.Type{"create": types.StringType, "update": types.StringType, "delete": types.StringType})}}
}

func newResourceTestServer(handler http.Handler) *httptest.Server {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	server := &httptest.Server{Listener: listener, Config: &http.Server{Handler: handler}}
	server.Start()
	return server
}
