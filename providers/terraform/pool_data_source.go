package main

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/mischapogr/platform-ipam/providers/terraform/internal/client"
)

var _ datasource.DataSource = (*poolDataSource)(nil)
var _ datasource.DataSourceWithConfigure = (*poolDataSource)(nil)

type poolDataSource struct{ api *client.Client }
type poolDataSourceModel struct {
	ID              types.String `tfsdk:"id"`
	Scopes          types.Set    `tfsdk:"scopes"`
	Environment     types.String `tfsdk:"environment"`
	Region          types.String `tfsdk:"region"`
	AllowedPrefixes types.Set    `tfsdk:"allowed_prefix_lengths"`
	PolicyVersion   types.String `tfsdk:"policy_version"`
}

func newPoolDataSource() datasource.DataSource { return &poolDataSource{} }
func (d *poolDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = "platformipam_pool"
}
func (d *poolDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.api = appendClient(&resp.Diagnostics, req.ProviderData)
}
func (d *poolDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{Description: "Reads an authorized platform-ipam pool summary without allocating space.", Attributes: map[string]schema.Attribute{"id": schema.StringAttribute{Required: true}, "scopes": schema.SetAttribute{Computed: true, ElementType: types.StringType}, "environment": schema.StringAttribute{Computed: true}, "region": schema.StringAttribute{Computed: true}, "allowed_prefix_lengths": schema.SetAttribute{Computed: true, ElementType: types.Int64Type}, "policy_version": schema.StringAttribute{Computed: true}}}
}
func (d *poolDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	if d.api == nil {
		resp.Diagnostics.AddError("Provider is not configured", "Configure the platformipam provider before reading a pool.")
		return
	}
	var data poolDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	pool, err := d.api.GetPool(ctx, data.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Unable to read pool", fmt.Sprintf("Pool state was not proven absent: %s", err))
		return
	}
	values := make([]attr.Value, 0, len(pool.AllowedPrefixes))
	for _, prefix := range pool.AllowedPrefixes {
		values = append(values, types.Int64Value(prefix))
	}
	scopeValues := make([]attr.Value, 0, len(pool.Scopes))
	for _, scope := range pool.Scopes {
		scopeValues = append(scopeValues, types.StringValue(scope))
	}
	data.Scopes = types.SetValueMust(types.StringType, scopeValues)
	data.Environment, data.Region, data.PolicyVersion = types.StringValue(pool.Environment), types.StringValue(pool.Region), types.StringValue(pool.PolicyVersion)
	data.AllowedPrefixes = types.SetValueMust(types.Int64Type, values)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
