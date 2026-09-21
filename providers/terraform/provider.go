package main

import (
	"context"
	"fmt"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/mischapogr/platform-ipam/providers/terraform/internal/client"
)

var _ provider.Provider = (*ipamProvider)(nil)

type providerData struct {
	Endpoint types.String `tfsdk:"endpoint"`
	Token    types.String `tfsdk:"token"`
}

type ipamProvider struct {
	version string
}

func New() provider.Provider { return &ipamProvider{version: version} }

func (p *ipamProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "platformipam"
	resp.Version = p.version
}

func (p *ipamProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Reserves address space through the platform-ipam API.",
		Attributes: map[string]schema.Attribute{
			"endpoint": schema.StringAttribute{
				Required:            true,
				Description:         "Platform API origin. HTTPS is required unless local loopback HTTP is explicitly enabled.",
				Validators:          nil,
				MarkdownDescription: "Platform API origin. HTTPS is required unless local loopback HTTP is explicitly enabled.",
			},
			"token": schema.StringAttribute{
				Optional:            true,
				Sensitive:           true,
				Description:         "Bearer token. Defaults to PLATFORM_IPAM_TOKEN.",
				MarkdownDescription: "Bearer token. Defaults to `PLATFORM_IPAM_TOKEN`.",
			},
		},
	}
}

func (p *ipamProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var data providerData
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if data.Endpoint.IsUnknown() || data.Endpoint.IsNull() || data.Endpoint.ValueString() == "" {
		resp.Diagnostics.AddError("Missing platform-ipam endpoint", "Set endpoint in the provider configuration.")
		return
	}
	endpoint := data.Endpoint.ValueString()

	token := os.Getenv("PLATFORM_IPAM_TOKEN")
	if !data.Token.IsNull() && !data.Token.IsUnknown() {
		token = data.Token.ValueString()
	}
	api, err := client.New(client.Config{Endpoint: endpoint, Token: token})
	if err != nil {
		resp.Diagnostics.AddError("Invalid platform-ipam endpoint", err.Error())
		return
	}
	resp.DataSourceData = api
	resp.ResourceData = api
}

func (p *ipamProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{newAllocationResource}
}

func (p *ipamProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{newAllocationDataSource, newPoolDataSource}
}

// appendClient resolves the configured API client for a resource or data
// source.
//
// Terraform calls Configure on every resource and data source before the
// provider itself is configured -- during validation, and again for a
// provider whose own configuration is not yet known. ProviderData is nil in
// those calls, and the framework's contract is to return quietly: the call is
// repeated once the provider is configured. Treating that first nil as a
// failure makes every plan fail with "Provider is not configured", which no
// consumer configuration can fix.
func appendClient(respDiags *diag.Diagnostics, data any) *client.Client {
	if data == nil {
		return nil
	}
	api, ok := data.(*client.Client)
	if !ok || api == nil {
		respDiags.AddError("Provider is not configured", "Configure the platformipam provider before using its resources or data sources.")
		return nil
	}
	return api
}

func diagnosticError(prefix string, err error) string {
	if err == nil {
		return prefix
	}
	return fmt.Sprintf("%s: %s", prefix, err)
}
