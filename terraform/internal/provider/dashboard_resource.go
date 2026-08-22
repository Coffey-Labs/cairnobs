package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = &dashboardResource{}
	_ resource.ResourceWithConfigure   = &dashboardResource{}
	_ resource.ResourceWithImportState = &dashboardResource{}
)

func newDashboardResource() resource.Resource {
	return &dashboardResource{}
}

// dashboardResource implements cairnobs_dashboard against
// api/dashboards.Handler's POST/GET/PUT/DELETE /dashboards[/{id}]
// endpoints -- the exact same JSON contract cli/cmd/cairnobsctl's
// "dashboards apply" and web's Export JSON button already use (see
// cli/README.md's "one JSON contract, multiple callers" framing; this
// is that third caller). Panels are a separate CRUD surface
// (POST/PUT/DELETE /dashboards/{id}/panels[/{panelId}]) not modeled by
// this resource yet -- see the provider README for why that's scoped
// out of this first pass rather than an oversight.
type dashboardResource struct {
	client *client
}

type dashboardResourceModel struct {
	ID              types.String `tfsdk:"id"`
	TenantID        types.String `tfsdk:"tenant_id"`
	Name            types.String `tfsdk:"name"`
	Description     types.String `tfsdk:"description"`
	DefaultEarliest types.String `tfsdk:"default_earliest"`
	DefaultLatest   types.String `tfsdk:"default_latest"`
	CreatedBy       types.String `tfsdk:"created_by"`
	CreatedAt       types.String `tfsdk:"created_at"`
	UpdatedAt       types.String `tfsdk:"updated_at"`
}

func (r *dashboardResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_dashboard"
}

func (r *dashboardResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A Cairn OBS dashboard. Panels aren't managed by this resource yet -- see the provider README.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
				Description:   "Server-generated dashboard ID.",
			},
			"tenant_id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
				Description: "Resolved server-side from the caller's identity -- never settable here, " +
					"matching api/dashboards.Handler's tenantID() doc comment (a client-supplied " +
					"tenant_id in the request body is always overridden).",
			},
			"name": schema.StringAttribute{
				Required:    true,
				Description: "Dashboard name. The API rejects an empty string.",
			},
			"description": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(""),
			},
			"default_earliest": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Description: "Default earliest time bound for panels that don't set their own override " +
					"(a query-language relative offset like \"-1h\" or an absolute timestamp -- see " +
					"/docs/query-language-reference.md). Left unset, the server defaults this to " +
					"\"-1h\" -- deliberately not hardcoded as a Terraform-side default too, so the API " +
					"stays the one source of truth for what \"unset\" means.",
			},
			"default_latest": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Default latest time bound. Left unset, the server defaults this to \"now\".",
			},
			"created_by": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"created_at": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"updated_at": schema.StringAttribute{
				Computed:    true,
				Description: "Changes on every update -- deliberately not given UseStateForUnknown, unlike created_at.",
			},
		},
	}
}

func (r *dashboardResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	data, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Resource Configure Type",
			fmt.Sprintf("Expected *provider.providerData, got: %T. This is a provider bug -- please report it.", req.ProviderData),
		)
		return
	}
	r.client = data.api
}

func dashboardModelFromAPI(d *dashboard) dashboardResourceModel {
	return dashboardResourceModel{
		ID:              types.StringValue(d.ID),
		TenantID:        types.StringValue(d.TenantID),
		Name:            types.StringValue(d.Name),
		Description:     types.StringValue(d.Description),
		DefaultEarliest: types.StringValue(d.DefaultEarliest),
		DefaultLatest:   types.StringValue(d.DefaultLatest),
		CreatedBy:       types.StringValue(d.CreatedBy),
		CreatedAt:       types.StringValue(d.CreatedAt),
		UpdatedAt:       types.StringValue(d.UpdatedAt),
	}
}

func (r *dashboardResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan dashboardResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	out, err := r.client.createDashboard(ctx, &dashboard{
		Name:            plan.Name.ValueString(),
		Description:     plan.Description.ValueString(),
		DefaultEarliest: plan.DefaultEarliest.ValueString(),
		DefaultLatest:   plan.DefaultLatest.ValueString(),
	})
	if err != nil {
		resp.Diagnostics.AddError("Creating Dashboard", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, dashboardModelFromAPI(out))...)
}

func (r *dashboardResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state dashboardResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	out, err := r.client.getDashboard(ctx, state.ID.ValueString())
	if err != nil {
		if isNotFound(err) {
			// Deleted out-of-band (e.g. via web or cairnobsctl) --
			// dropping it from state lets the next plan offer to
			// recreate it, the standard Terraform convention, rather
			// than failing every subsequent plan/apply until someone
			// manually edits state.
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Reading Dashboard", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, dashboardModelFromAPI(out))...)
}

func (r *dashboardResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan dashboardResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	out, err := r.client.updateDashboard(ctx, plan.ID.ValueString(), &dashboard{
		Name:            plan.Name.ValueString(),
		Description:     plan.Description.ValueString(),
		DefaultEarliest: plan.DefaultEarliest.ValueString(),
		DefaultLatest:   plan.DefaultLatest.ValueString(),
	})
	if err != nil {
		resp.Diagnostics.AddError("Updating Dashboard", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, dashboardModelFromAPI(out))...)
}

func (r *dashboardResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state dashboardResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := r.client.deleteDashboard(ctx, state.ID.ValueString()); err != nil && !isNotFound(err) {
		resp.Diagnostics.AddError("Deleting Dashboard", err.Error())
	}
}

func (r *dashboardResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
