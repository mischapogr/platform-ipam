package main

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/mischapogr/platform-ipam/providers/terraform/internal/client"
)

var _ resource.Resource = (*allocationResource)(nil)
var _ resource.ResourceWithConfigure = (*allocationResource)(nil)
var _ resource.ResourceWithImportState = (*allocationResource)(nil)
var _ resource.ResourceWithModifyPlan = (*allocationResource)(nil)

type allocationResource struct{ api *client.Client }

type allocationModel struct {
	AllocationKey      types.String   `tfsdk:"allocation_key"`
	Scope              types.String   `tfsdk:"scope"`
	Environment        types.String   `tfsdk:"environment"`
	Region             types.String   `tfsdk:"region"`
	AccountID          types.String   `tfsdk:"account_id"`
	AddressFamily      types.String   `tfsdk:"address_family"`
	PrefixLength       types.Int64    `tfsdk:"prefix_length"`
	ParentAllocationID types.String   `tfsdk:"parent_allocation_id"`
	AvailabilityZoneID types.String   `tfsdk:"availability_zone_id"`
	Description        types.String   `tfsdk:"description"`
	Labels             types.Map      `tfsdk:"labels"`
	Timeouts           timeouts.Value `tfsdk:"timeouts"`
	ID                 types.String   `tfsdk:"id"`
	CIDR               types.String   `tfsdk:"cidr"`
	PoolID             types.String   `tfsdk:"pool_id"`
	AllocationState    types.String   `tfsdk:"allocation_state"`
	BoundResourceID    types.String   `tfsdk:"bound_resource_id"`
	InventoryURL       types.String   `tfsdk:"inventory_url"`
	ETag               types.String   `tfsdk:"etag"`
	Revision           types.Int64    `tfsdk:"revision"`
}

func newAllocationResource() resource.Resource { return &allocationResource{} }

func (r *allocationResource) Metadata(_ context.Context, _ resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = "platformipam_allocation"
}

func (r *allocationResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.api = appendClient(&resp.Diagnostics, req.ProviderData)
}

func (r *allocationResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = allocationSchema(ctx)
}

func allocationSchema(ctx context.Context) schema.Schema {
	immutableString := func(description string) schema.StringAttribute {
		return schema.StringAttribute{Required: true, Description: description, PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}}
	}
	return schema.Schema{
		Description: "Reserves a durable address allocation through platform-ipam.",
		Attributes: map[string]schema.Attribute{
			"allocation_key": schema.StringAttribute{
				Required:      true,
				Description:   "Permanent logical identity. Use a new generation for replacement.",
				Validators:    []validator.String{stringvalidator.RegexMatches(allocationKeyPattern, "allocation key must use only ASCII letters, digits, '.', '_', '-' or '/ and start with a letter or digit"), stringvalidator.LengthBetween(1, 128)},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"scope":       immutableString("Allocation scope: vpc or subnet."),
			"environment": immutableString("Authorized environment identifier."),
			"region":      immutableString("Authorized AWS region."),
			"account_id":  immutableString("Authorized 12-digit AWS account ID."),
			"address_family": schema.StringAttribute{
				// Computed as well as Optional: the API applies a default when
				// the caller omits this, and an Optional-only attribute would
				// make Terraform reject that default as an inconsistent result
				// after apply. UseStateForUnknown keeps the committed value
				// from showing as a diff on the next plan.
				Optional: true, Computed: true, Description: "Address family.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"prefix_length": schema.Int64Attribute{
				Required: true, Description: "Requested CIDR prefix length.",
				PlanModifiers: []planmodifier.Int64{int64planmodifier.RequiresReplace()},
			},
			"parent_allocation_id": schema.StringAttribute{
				Optional: true, Description: "Parent VPC allocation for a subnet.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"availability_zone_id": schema.StringAttribute{
				Optional: true, Description: "Stable AWS availability zone ID for a subnet.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"description": schema.StringAttribute{Optional: true, Computed: true, Description: "Mutable operator description."},
			"labels": schema.MapAttribute{
				Optional: true, Computed: true, ElementType: types.StringType, Description: "Mutable labels.",
			},
			"timeouts":          timeouts.Attributes(ctx, timeouts.Opts{Create: true, Update: true, Delete: true}),
			"id":                schema.StringAttribute{Computed: true, Description: "Committed platform allocation ID."},
			"cidr":              schema.StringAttribute{Computed: true, Description: "Committed CIDR; known only after reservation."},
			"pool_id":           schema.StringAttribute{Computed: true},
			"allocation_state":  schema.StringAttribute{Computed: true, Description: "API lifecycle state."},
			"bound_resource_id": schema.StringAttribute{Computed: true, Description: "Verified AWS resource ID, when present."},
			"inventory_url":     schema.StringAttribute{Computed: true, Description: "Opaque inventory link, when authorized."},
			"etag":              schema.StringAttribute{Computed: true, Description: "API revision validator used for metadata updates."},
			"revision":          schema.Int64Attribute{Computed: true},
		},
	}
}

var allocationKeyPattern = mustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)

func (r *allocationResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}
	var prior, planned allocationModel
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	resp.Diagnostics.Append(req.Plan.Get(ctx, &planned)...)
	if resp.Diagnostics.HasError() || !stringKnown(prior.AllocationKey) || !stringKnown(planned.AllocationKey) {
		return
	}
	if prior.AllocationKey.ValueString() == planned.AllocationKey.ValueString() && immutableChanged(prior, planned) {
		resp.Diagnostics.AddAttributeError(path.Root("allocation_key"), "Replacement requires a new allocation key", "An immutable allocation change cannot reuse the retired logical identity. Choose a new allocation_key generation; manual -replace with the same key is unsupported.")
	}
}

func immutableChanged(a, b allocationModel) bool {
	return stringChanged(a.Scope, b.Scope) || stringChanged(a.Environment, b.Environment) || stringChanged(a.Region, b.Region) || stringChanged(a.AccountID, b.AccountID) || stringChanged(a.AddressFamily, b.AddressFamily) || intChanged(a.PrefixLength, b.PrefixLength) || stringChanged(a.ParentAllocationID, b.ParentAllocationID) || stringChanged(a.AvailabilityZoneID, b.AvailabilityZoneID)
}

func stringChanged(a, b types.String) bool {
	return stringKnown(a) && stringKnown(b) && a.ValueString() != b.ValueString()
}
func intChanged(a, b types.Int64) bool {
	return intKnown(a) && intKnown(b) && a.ValueInt64() != b.ValueInt64()
}

func stringKnown(v types.String) bool { return !v.IsNull() && !v.IsUnknown() }
func intKnown(v types.Int64) bool     { return !v.IsNull() && !v.IsUnknown() }

func (r *allocationResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if r.api == nil {
		resp.Diagnostics.AddError("Provider is not configured", "Configure the platformipam provider before creating an allocation.")
		return
	}
	var plan allocationModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	deadline, diags := plan.Timeouts.Create(ctx, 10*time.Minute)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	// This deadline governs CreateAllocation's own polling loop
	// (waitForAllocation) only. On expiry the context simply cancels and the
	// pending reservation is left exactly as it was -- the provider issues no
	// DELETE /v1/operations/{operation_id} here or anywhere else (ADR 0013),
	// deliberately: a Terraform create timeout is routinely shorter than a
	// reconciliation interval, and a provider that cancelled on it would
	// destroy holds that were one worker pass away from committing. Only the
	// hold's own tenant, over the API, ever cancels a reservation.
	operationCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	request := allocationRequest(plan)
	allocation, err := r.api.CreateAllocation(operationCtx, request, reserveKey(plan.AllocationKey.ValueString()))
	if err != nil {
		// A lost response can leave a committed allocation. Recover by the
		// permanent key before returning the diagnostic so operators can import it.
		if recovered, lookupErr := r.api.FindAllocationByKey(operationCtx, request.AllocationKey); lookupErr == nil && recovered.ID != "" {
			state := modelFromAllocation(plan, recovered)
			resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
			resp.Diagnostics.AddError("Allocation committed but create response was not confirmed", diagnosticError("Recover allocation "+recovered.ID+" by import or inspect the existing key", err))
			return
		}
		// If a FAILED reservation reached here because its tenant (or an
		// operator's automation) cancelled it through
		// DELETE /v1/operations/{operation_id} (ADR 0013, error code
		// reservation_cancelled), the allocation row is gone and
		// FindAllocationByKey above found nothing to recover: this apply is
		// safe to retry unmodified, and it reserves fresh under the same
		// derived key rather than replaying the cancelled attempt.
		resp.Diagnostics.AddError("Unable to reserve allocation", fmt.Sprintf("The request used stable allocation_key %q and can be retried safely after checking the operation or key -- including whether the reservation was itself cancelled (error code reservation_cancelled), in which case retrying reserves fresh under the same key. %s", request.AllocationKey, err))
		return
	}
	if allocation.State != "RESERVED" && allocation.State != "ACTIVE" {
		resp.Diagnostics.AddError("Allocation is not usable", fmt.Sprintf("API committed allocation %s in state %q; only RESERVED or ACTIVE allocations can be provisioned.", allocation.ID, allocation.State))
		return
	}
	state := modelFromAllocation(plan, allocation)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !metadataEqual(plan, allocation) {
		updated, updateErr := r.updateMetadata(operationCtx, allocation, plan)
		if updateErr != nil {
			resp.Diagnostics.AddError("Allocation committed but metadata update failed", fmt.Sprintf("Allocation %s is in Terraform state and may be tainted. Stop applies, verify this allocation ID and cloud identity, then explicitly clear taint or re-import without releasing it. Retry the metadata update after recovery: %s", allocation.ID, updateErr))
			return
		}
		state = modelFromAllocation(plan, updated)
		resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
	}
}

func (r *allocationResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if r.api == nil {
		resp.Diagnostics.AddError("Provider is not configured", "Configure the platformipam provider before reading an allocation.")
		return
	}
	var state allocationModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	allocation, err := r.api.GetAllocation(ctx, state.ID.ValueString())
	if err != nil {
		// E3 (docs/API_V1.md section 5): a 409 allocation_pending means the
		// allocation still exists and is being worked on -- the opposite of
		// absent. It must never be confused with a 404, which is already
		// handled below by never calling resp.State.RemoveResource at all: no
		// branch of this function does, on any error, so state is retained
		// here exactly as it was before this package. What changes is only the
		// diagnostic, which names the operation still pending so an operator
		// reading `terraform plan` output knows what to poll instead of
		// guessing from a bare HTTP error.
		if opID, pending := pendingOperationID(err); pending {
			resp.Diagnostics.AddError("Allocation reservation is still pending", fmt.Sprintf("Allocation %q is not yet committed: reservation operation %s is still being worked on. State is retained; this is not a failure to retry with a new key, and the resource will refresh cleanly once the operation reaches a terminal state (poll GET /v1/operations/%s or platform-ipam client get --id %s). %s", state.ID.ValueString(), opID, opID, state.ID.ValueString(), err))
			return
		}
		resp.Diagnostics.AddError("Unable to refresh allocation", fmt.Sprintf("The allocation ID %q was not proven absent. State is retained to prevent unsafe recreation; recover authorization or inspect the API before retrying. %s", state.ID.ValueString(), err))
		return
	}
	state = modelFromAllocation(state, allocation)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// pendingOperationID recognises a 409 allocation_pending error (E3,
// docs/API_V1.md section 5) and reports the operation id from its
// details.operation_id, when present. The second return value is true for
// any allocation_pending error even when the id could not be extracted, so a
// caller can still choose the more specific diagnostic wording.
func pendingOperationID(err error) (string, bool) {
	apiErr, ok := err.(*client.HTTPError)
	if !ok || apiErr.Code != "allocation_pending" {
		return "", false
	}
	if id, ok := apiErr.Details["operation_id"].(string); ok {
		return id, true
	}
	return "", true
}

func (r *allocationResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	if r.api == nil {
		resp.Diagnostics.AddError("Provider is not configured", "Configure the platformipam provider before updating an allocation.")
		return
	}
	var plan, state allocationModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	deadline, diags := plan.Timeouts.Update(ctx, 5*time.Minute)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	operationCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	if stringKnown(state.AllocationState) && (state.AllocationState.ValueString() == "QUARANTINED" || state.AllocationState.ValueString() == "RELEASED") {
		resp.Diagnostics.AddError("Allocation is retired", "Metadata cannot be changed after release intent. Use a new allocation key for provisioning.")
		return
	}
	allocation := allocationFromState(state)
	updated, err := r.updateMetadata(operationCtx, allocation, plan)
	if err != nil {
		resp.Diagnostics.AddError("Unable to update allocation metadata", err.Error())
		return
	}
	result := modelFromAllocation(plan, updated)
	resp.Diagnostics.Append(resp.State.Set(ctx, &result)...)
}

func (r *allocationResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if r.api == nil {
		resp.Diagnostics.AddError("Provider is not configured", "Configure the platformipam provider before deleting an allocation.")
		return
	}
	var state allocationModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	deadline, diags := state.Timeouts.Delete(ctx, 5*time.Minute)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	operationCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	_, durable, err := r.api.DeleteAllocation(operationCtx, state.ID.ValueString())
	if err != nil {
		if apiErr, ok := err.(*client.HTTPError); ok && apiErr.IsStatus(404) && stringKnown(state.AllocationState) && state.AllocationState.ValueString() == "RELEASED" {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Unable to release allocation", fmt.Sprintf("Release was not durably acknowledged. Terraform state is retained; resolve the API error and retry after AWS resources and child allocations are handled. %s", err))
		return
	}
	if durable {
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.AddError("Release was not durable", "The platform API did not acknowledge quarantine or release; Terraform state is retained.")
}

func (r *allocationResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *allocationResource) updateMetadata(ctx context.Context, current *client.Allocation, desired allocationModel) (*client.Allocation, error) {
	patch := client.AllocationPatch{}
	if stringKnown(desired.Description) {
		description := desired.Description.ValueString()
		patch.Description = &description
	}
	if !desired.Labels.IsNull() && !desired.Labels.IsUnknown() {
		labels := mapValue(desired.Labels)
		patch.Labels = &labels
	}
	if patch.Description == nil && patch.Labels == nil {
		return current, nil
	}
	etag := current.ETag
	for attempt := 0; attempt < 4; attempt++ {
		updated, err := r.api.UpdateAllocation(ctx, current.ID, etag, client.NewIdempotencyKey("patch-"+current.ID), patch)
		if err == nil {
			return updated, nil
		}
		apiErr, ok := err.(*client.HTTPError)
		if !ok || !apiErr.IsStatus(412) {
			return nil, err
		}
		fresh, readErr := r.api.GetAllocation(ctx, current.ID)
		if readErr != nil {
			return nil, fmt.Errorf("revision conflict; re-read failed: %w", readErr)
		}
		if metadataEqual(desired, fresh) {
			return fresh, nil
		}
		current, etag = fresh, fresh.ETag
	}
	return nil, fmt.Errorf("metadata update exceeded revision-conflict retry limit")
}

func allocationRequest(m allocationModel) client.AllocationRequest {
	return client.AllocationRequest{AllocationKey: m.AllocationKey.ValueString(), Scope: m.Scope.ValueString(), Environment: m.Environment.ValueString(), Region: m.Region.ValueString(), AccountID: m.AccountID.ValueString(), AddressFamily: valueOr(m.AddressFamily, "ipv4"), PrefixLength: m.PrefixLength.ValueInt64(), ParentAllocationID: m.ParentAllocationID.ValueString(), AvailabilityZoneID: m.AvailabilityZoneID.ValueString(), Description: m.Description.ValueString(), Labels: mapValue(m.Labels)}
}

func modelFromAllocation(base allocationModel, a *client.Allocation) allocationModel {
	base.AllocationKey = types.StringValue(a.AllocationKey)
	base.Scope, base.Environment, base.Region, base.AccountID = types.StringValue(a.Scope), types.StringValue(a.Environment), types.StringValue(a.Region), types.StringValue(a.AccountID)
	base.AddressFamily, base.PrefixLength = types.StringValue(valueOrString(a.AddressFamily, "ipv4")), types.Int64Value(a.PrefixLength)
	base.ParentAllocationID, base.AvailabilityZoneID = optionalString(a.ParentAllocationID), optionalString(a.AvailabilityZoneID)
	base.Description, base.Labels = types.StringValue(a.Description), types.MapValueMust(types.StringType, stringMapValues(a.Labels))
	base.ID, base.CIDR, base.PoolID, base.AllocationState = types.StringValue(a.ID), types.StringValue(a.CIDR), types.StringValue(a.PoolID), types.StringValue(a.State)
	if a.Binding != nil {
		base.BoundResourceID = types.StringValue(a.Binding.ResourceID)
	} else {
		base.BoundResourceID = types.StringNull()
	}
	if a.Links.Inventory != "" {
		base.InventoryURL = types.StringValue(a.Links.Inventory)
	} else {
		base.InventoryURL = types.StringNull()
	}
	base.ETag, base.Revision = types.StringValue(a.ETag), types.Int64Value(a.Revision)
	return base
}

func allocationFromState(m allocationModel) *client.Allocation {
	return &client.Allocation{ID: m.ID.ValueString(), AllocationKey: m.AllocationKey.ValueString(), ETag: m.ETag.ValueString(), Description: m.Description.ValueString(), Labels: mapValue(m.Labels), State: m.AllocationState.ValueString(), Revision: m.Revision.ValueInt64()}
}
func metadataEqual(m allocationModel, a *client.Allocation) bool {
	descriptionEqual := !stringKnown(m.Description) || m.Description.ValueString() == a.Description
	labelsEqual := m.Labels.IsNull() || m.Labels.IsUnknown() || mapsEqual(mapValue(m.Labels), a.Labels)
	return descriptionEqual && labelsEqual
}
func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
func mapValue(v types.Map) map[string]string {
	out := map[string]string{}
	if v.IsNull() || v.IsUnknown() {
		return out
	}
	for k, item := range v.Elements() {
		if s, ok := item.(types.String); ok && stringKnown(s) {
			out[k] = s.ValueString()
		}
	}
	return out
}
func stringMapValues(v map[string]string) map[string]attr.Value {
	out := map[string]attr.Value{}
	for k, value := range v {
		out[k] = types.StringValue(value)
	}
	return out
}
func optionalString(v string) types.String {
	if v == "" {
		return types.StringNull()
	}
	return types.StringValue(v)
}
func valueOr(v types.String, fallback string) string {
	if v.IsNull() || v.IsUnknown() || v.ValueString() == "" {
		return fallback
	}
	return v.ValueString()
}
func valueOrString(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
func reserveKey(key string) string { return "reserve-" + key }

func mustCompile(pattern string) *regexp.Regexp { return regexp.MustCompile(pattern) }
