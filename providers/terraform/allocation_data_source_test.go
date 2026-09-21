package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/mischapogr/platform-ipam/providers/terraform/internal/client"
)

// TestDataSourceReadNamesThePendingOperationRatherThanReportingAbsence is
// package E3's data source test: like the resource's own Read, the data
// source holds no Terraform-managed state to remove on any error, so the
// only thing this package changes here is the diagnostic wording for a 409
// allocation_pending -- it names the operation still working on the
// allocation instead of the generic "not proven absent" phrasing a plain
// error would get.
func TestDataSourceReadNamesThePendingOperationRatherThanReportingAbsence(t *testing.T) {
	t.Setenv("PLATFORM_IPAM_ALLOW_LOCAL_HTTP", "1")
	server := newResourceTestServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"allocation_pending","message":"still pending","retryable":true,"details":{"operation_id":"op_ds_pending_01"}}}`))
	}))
	defer server.Close()
	api, err := client.New(client.Config{Endpoint: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	ds := &allocationDataSource{api: api}
	ctx := context.Background()
	schemaResp := &datasource.SchemaResponse{}
	ds.Schema(ctx, datasource.SchemaRequest{}, schemaResp)
	// tfsdk.Config in this SDK version has no Set helper (only State and
	// Plan do); build the raw value through a State with the same schema
	// and reuse its Raw, which is exactly what Set would have produced.
	state := tfsdk.State{Schema: schemaResp.Schema}
	model := allocationDataSourceModel{
		ID: types.StringValue("alloc_01"), AllocationKey: types.StringNull(),
		Scope: types.StringNull(), Environment: types.StringNull(), Region: types.StringNull(),
		AccountID: types.StringNull(), AddressFamily: types.StringNull(), PrefixLength: types.Int64Null(),
		ParentAllocationID: types.StringNull(), AvailabilityZoneID: types.StringNull(),
		Description: types.StringNull(), Labels: types.MapNull(types.StringType),
		CIDR: types.StringNull(), PoolID: types.StringNull(), AllocationState: types.StringNull(),
		BoundResourceID: types.StringNull(), InventoryURL: types.StringNull(),
	}
	if diags := state.Set(ctx, &model); diags.HasError() {
		t.Fatal(diags)
	}
	config := tfsdk.Config{Schema: schemaResp.Schema, Raw: state.Raw}
	resp := &datasource.ReadResponse{}
	ds.Read(ctx, datasource.ReadRequest{Config: config}, resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected a diagnostic")
	}
	if !strings.Contains(resp.Diagnostics[0].Detail(), "op_ds_pending_01") {
		t.Fatalf("detail=%q, want it to name the pending operation", resp.Diagnostics[0].Detail())
	}
	if !strings.Contains(resp.Diagnostics[0].Summary(), "pending") {
		t.Fatalf("summary=%q, want it to distinguish this from an ordinary read failure", resp.Diagnostics[0].Summary())
	}
}
