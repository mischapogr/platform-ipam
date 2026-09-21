package main

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/mischapogr/platform-ipam/providers/terraform/internal/client"
)

var _ datasource.DataSource = (*allocationDataSource)(nil)
var _ datasource.DataSourceWithConfigure = (*allocationDataSource)(nil)

type allocationDataSource struct{ api *client.Client }

type allocationDataSourceModel struct {
	ID                 types.String `tfsdk:"id"`
	AllocationKey      types.String `tfsdk:"allocation_key"`
	Scope              types.String `tfsdk:"scope"`
	Environment        types.String `tfsdk:"environment"`
	Region             types.String `tfsdk:"region"`
	AccountID          types.String `tfsdk:"account_id"`
	AddressFamily      types.String `tfsdk:"address_family"`
	PrefixLength       types.Int64  `tfsdk:"prefix_length"`
	ParentAllocationID types.String `tfsdk:"parent_allocation_id"`
	AvailabilityZoneID types.String `tfsdk:"availability_zone_id"`
	Description        types.String `tfsdk:"description"`
	Labels             types.Map    `tfsdk:"labels"`
	CIDR               types.String `tfsdk:"cidr"`
	PoolID             types.String `tfsdk:"pool_id"`
	AllocationState    types.String `tfsdk:"allocation_state"`
	BoundResourceID    types.String `tfsdk:"bound_resource_id"`
	InventoryURL       types.String `tfsdk:"inventory_url"`
}

func newAllocationDataSource() datasource.DataSource { return &allocationDataSource{} }
func (d *allocationDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = "platformipam_allocation"
}
func (d *allocationDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.api = appendClient(&resp.Diagnostics, req.ProviderData)
}

func (d *allocationDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{Description: "Reads one committed usable platform-ipam allocation without reserving space.", Attributes: map[string]schema.Attribute{
		"id":             schema.StringAttribute{Optional: true, Computed: true, Description: "Allocation ID; exactly one of id or allocation_key is required."},
		"allocation_key": schema.StringAttribute{Optional: true, Computed: true, Description: "Permanent allocation key; exactly one of allocation_key or id is required."},
		"scope":          schema.StringAttribute{Computed: true}, "environment": schema.StringAttribute{Computed: true}, "region": schema.StringAttribute{Computed: true}, "account_id": schema.StringAttribute{Computed: true}, "address_family": schema.StringAttribute{Computed: true}, "prefix_length": schema.Int64Attribute{Computed: true}, "parent_allocation_id": schema.StringAttribute{Computed: true}, "availability_zone_id": schema.StringAttribute{Computed: true}, "description": schema.StringAttribute{Computed: true}, "labels": schema.MapAttribute{Computed: true, ElementType: types.StringType}, "cidr": schema.StringAttribute{Computed: true}, "pool_id": schema.StringAttribute{Computed: true}, "allocation_state": schema.StringAttribute{Computed: true}, "bound_resource_id": schema.StringAttribute{Computed: true}, "inventory_url": schema.StringAttribute{Computed: true},
	}}
}

func (d *allocationDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	if d.api == nil {
		resp.Diagnostics.AddError("Provider is not configured", "Configure the platformipam provider before reading an allocation.")
		return
	}
	var data allocationDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if data.ID.IsNull() == data.AllocationKey.IsNull() || data.ID.IsUnknown() || data.AllocationKey.IsUnknown() {
		resp.Diagnostics.AddError("Choose exactly one allocation lookup", "Set exactly one of id or allocation_key. A data source read never reserves an allocation.")
		return
	}
	var allocation *client.Allocation
	var err error
	if !data.ID.IsUnknown() && !data.ID.IsNull() {
		allocation, err = d.api.GetAllocation(ctx, data.ID.ValueString())
	} else {
		allocation, err = d.api.FindAllocationByKey(ctx, data.AllocationKey.ValueString())
	}
	if err != nil {
		// E3 (docs/API_V1.md section 5): mirror the resource's Read -- a 409
		// allocation_pending means the allocation exists and is not yet
		// committed, not that it is absent, so name the operation still
		// working on it rather than the generic "not proven absent" wording.
		// A data source holds no Terraform-managed state to remove on any
		// error, by ID or by key, so there is nothing to preserve beyond the
		// diagnostic itself.
		if opID, pending := pendingOperationID(err); pending {
			resp.Diagnostics.AddError("Allocation reservation is still pending", fmt.Sprintf("The allocation is not yet committed: reservation operation %s is still being worked on. This data source only reads a committed RESERVED or ACTIVE allocation; retry once the operation reaches a terminal state. %s", opID, err))
			return
		}
		resp.Diagnostics.AddError("Unable to read allocation", fmt.Sprintf("The allocation was not proven absent and will not be replaced or reserved by this data source: %s", err))
		return
	}
	if allocation.State != "RESERVED" && allocation.State != "ACTIVE" {
		resp.Diagnostics.AddError("Allocation is not usable", fmt.Sprintf("Allocation %s is in state %q; data sources accept only RESERVED or ACTIVE allocations.", allocation.ID, allocation.State))
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, dataSourceFromAllocation(allocation))...)
}

func dataSourceFromAllocation(a *client.Allocation) allocationDataSourceModel {
	return allocationDataSourceModel{ID: types.StringValue(a.ID), AllocationKey: types.StringValue(a.AllocationKey), Scope: types.StringValue(a.Scope), Environment: types.StringValue(a.Environment), Region: types.StringValue(a.Region), AccountID: types.StringValue(a.AccountID), AddressFamily: types.StringValue(valueOrString(a.AddressFamily, "ipv4")), PrefixLength: types.Int64Value(a.PrefixLength), ParentAllocationID: optionalString(a.ParentAllocationID), AvailabilityZoneID: optionalString(a.AvailabilityZoneID), Description: types.StringValue(a.Description), Labels: types.MapValueMust(types.StringType, stringMapValues(a.Labels)), CIDR: types.StringValue(a.CIDR), PoolID: types.StringValue(a.PoolID), AllocationState: types.StringValue(a.State), BoundResourceID: bindingID(a.Binding), InventoryURL: optionalString(a.Links.Inventory)}
}

func bindingID(binding *client.Binding) types.String {
	if binding == nil || binding.ResourceID == "" {
		return types.StringNull()
	}
	return types.StringValue(binding.ResourceID)
}
