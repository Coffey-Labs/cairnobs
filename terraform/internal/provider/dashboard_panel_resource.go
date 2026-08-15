package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = &dashboardPanelResource{}
	_ resource.ResourceWithConfigure   = &dashboardPanelResource{}
	_ resource.ResourceWithImportState = &dashboardPanelResource{}
)

func newDashboardPanelResource() resource.Resource {
	return &dashboardPanelResource{}
}

// dashboardPanelResource implements sentry_dashboard_panel against
// api/dashboards.Handler's POST/PUT/DELETE
// /dashboards/{id}/panels[/{panelId}] endpoints -- a genuinely separate
// resource from sentry_dashboard (own id, own lifecycle, own endpoints),
// not a nested block on the dashboard resource. That split matches the
// API's own shape (a panel is created/updated/deleted independently of
// its parent dashboard, never by rewriting the dashboard's whole panel
// list) and is the more idiomatic Terraform pattern for independently-
// lifecycled child resources: a nested list block would force every
// panel to be rewritten on any single panel's change, hiding
// fine-grained diffs a separate resource shows naturally.
//
// Unlike sentry_alert_rule/sentry_notification_target, this resource
// supports a real in-place Update -- api/dashboards.Handler actually has
// a PUT /dashboards/{id}/panels/{panelId}. Only dashboard_id forces a
// replace: UpdatePanel's SQL matches WHERE id = $panelID AND
// dashboard_id = $dashboardID (store.go), so sending a changed
// dashboard_id through the existing panel's URL wouldn't move it, it
// would just fail to match -- there's no API operation for "move a
// panel to a different dashboard," so Terraform has to destroy and
// recreate instead of attempting an update that can't work.
type dashboardPanelResource struct {
	client *client
}

type dashboardPanelResourceModel struct {
	ID               types.String `tfsdk:"id"`
	DashboardID      types.String `tfsdk:"dashboard_id"`
	Title            types.String `tfsdk:"title"`
	Query            types.String `tfsdk:"query"`
	QueryLanguage    types.String `tfsdk:"query_language"`
	VizType          types.String `tfsdk:"viz_type"`
	VizConfig        types.String `tfsdk:"viz_config"`
	PositionX        types.Int64  `tfsdk:"position_x"`
	PositionY        types.Int64  `tfsdk:"position_y"`
	Width            types.Int64  `tfsdk:"width"`
	Height           types.Int64  `tfsdk:"height"`
	EarliestOverride types.String `tfsdk:"earliest_override"`
	LatestOverride   types.String `tfsdk:"latest_override"`
	SortOrder        types.Int64  `tfsdk:"sort_order"`
	CreatedAt        types.String `tfsdk:"created_at"`
	UpdatedAt        types.String `tfsdk:"updated_at"`
}

func (r *dashboardPanelResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_dashboard_panel"
}

func (r *dashboardPanelResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A panel on a Sentry dashboard, managed independently of the sentry_dashboard it belongs to.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
				Description:   "Server-generated panel ID.",
			},
			"dashboard_id": schema.StringAttribute{
				Required:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Description:   "ID of the sentry_dashboard this panel belongs to. Forces replacement on change -- there is no API operation to move a panel between dashboards, see this resource's Go doc comment.",
			},
			"title": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(""),
			},
			"query": schema.StringAttribute{
				Required:    true,
				Description: "Pipe-syntax query text. The API rejects an empty string, and rejects query_language = \"sql\" outright -- see this resource's Go doc comment.",
			},
			"query_language": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Default:     stringdefault.StaticString(""),
				Description: `"" (auto-detect) or "spl" -- never "sql", the API rejects that for panels specifically (unlike sentry_alert_rule's query_language, which accepts it).`,
			},
			"viz_type": schema.StringAttribute{
				Required:    true,
				Description: `One of "table", "line", "bar", "single_stat", "top_n".`,
			},
			"viz_config": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Default:     stringdefault.StaticString("{}"),
				Description: `Visualization-specific config, as a JSON object string -- e.g. jsonencode({...}). Left unset, the API defaults this to "{}".`,
			},
			"position_x": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				Default:  int64default.StaticInt64(0),
			},
			"position_y": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				Default:  int64default.StaticInt64(0),
			},
			"width": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				Default:  int64default.StaticInt64(0),
			},
			"height": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				Default:  int64default.StaticInt64(0),
			},
			"earliest_override": schema.StringAttribute{
				Optional:    true,
				Description: "Overrides the parent dashboard's default_earliest for this panel only. Unset means inherit the dashboard's default.",
			},
			"latest_override": schema.StringAttribute{
				Optional:    true,
				Description: "Overrides the parent dashboard's default_latest for this panel only. Unset means inherit the dashboard's default.",
			},
			"sort_order": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				Default:  int64default.StaticInt64(0),
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

func (r *dashboardPanelResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func dashboardPanelModelFromAPI(dashboardID string, p *panel) dashboardPanelResourceModel {
	m := dashboardPanelResourceModel{
		ID:            types.StringValue(p.ID),
		DashboardID:   types.StringValue(dashboardID),
		Title:         types.StringValue(p.Title),
		Query:         types.StringValue(p.Query),
		QueryLanguage: types.StringValue(p.QueryLanguage),
		VizType:       types.StringValue(p.VizType),
		PositionX:     types.Int64Value(int64(p.PositionX)),
		PositionY:     types.Int64Value(int64(p.PositionY)),
		Width:         types.Int64Value(int64(p.Width)),
		Height:        types.Int64Value(int64(p.Height)),
		SortOrder:     types.Int64Value(int64(p.SortOrder)),
		CreatedAt:     types.StringValue(p.CreatedAt),
		UpdatedAt:     types.StringValue(p.UpdatedAt),
	}
	if len(p.VizConfig) > 0 {
		m.VizConfig = types.StringValue(string(p.VizConfig))
	} else {
		m.VizConfig = types.StringValue("{}")
	}
	if p.EarliestOverride != nil {
		m.EarliestOverride = types.StringValue(*p.EarliestOverride)
	}
	if p.LatestOverride != nil {
		m.LatestOverride = types.StringValue(*p.LatestOverride)
	}
	return m
}

func dashboardPanelAPIFromModel(m dashboardPanelResourceModel) (*panel, error) {
	p := &panel{
		Title:         m.Title.ValueString(),
		Query:         m.Query.ValueString(),
		QueryLanguage: m.QueryLanguage.ValueString(),
		VizType:       m.VizType.ValueString(),
		PositionX:     int(m.PositionX.ValueInt64()),
		PositionY:     int(m.PositionY.ValueInt64()),
		Width:         int(m.Width.ValueInt64()),
		Height:        int(m.Height.ValueInt64()),
		SortOrder:     int(m.SortOrder.ValueInt64()),
	}
	if !m.VizConfig.IsNull() {
		raw := m.VizConfig.ValueString()
		if !json.Valid([]byte(raw)) {
			return nil, fmt.Errorf("viz_config must be valid JSON (use jsonencode(...) in the resource config), got: %s", raw)
		}
		p.VizConfig = json.RawMessage(raw)
	}
	if !m.EarliestOverride.IsNull() {
		v := m.EarliestOverride.ValueString()
		p.EarliestOverride = &v
	}
	if !m.LatestOverride.IsNull() {
		v := m.LatestOverride.ValueString()
		p.LatestOverride = &v
	}
	return p, nil
}

func (r *dashboardPanelResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan dashboardPanelResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	in, err := dashboardPanelAPIFromModel(plan)
	if err != nil {
		resp.Diagnostics.AddError("Invalid Configuration", err.Error())
		return
	}
	dashboardID := plan.DashboardID.ValueString()
	out, err := r.client.createPanel(ctx, dashboardID, in)
	if err != nil {
		resp.Diagnostics.AddError("Creating Dashboard Panel", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, dashboardPanelModelFromAPI(dashboardID, out))...)
}

func (r *dashboardPanelResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state dashboardPanelResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	dashboardID := state.DashboardID.ValueString()
	out, err := r.client.getPanel(ctx, dashboardID, state.ID.ValueString())
	if err != nil {
		if isNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Reading Dashboard Panel", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, dashboardPanelModelFromAPI(dashboardID, out))...)
}

func (r *dashboardPanelResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan dashboardPanelResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	in, err := dashboardPanelAPIFromModel(plan)
	if err != nil {
		resp.Diagnostics.AddError("Invalid Configuration", err.Error())
		return
	}
	dashboardID := plan.DashboardID.ValueString()
	out, err := r.client.updatePanel(ctx, dashboardID, plan.ID.ValueString(), in)
	if err != nil {
		resp.Diagnostics.AddError("Updating Dashboard Panel", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, dashboardPanelModelFromAPI(dashboardID, out))...)
}

func (r *dashboardPanelResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state dashboardPanelResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := r.client.deletePanel(ctx, state.DashboardID.ValueString(), state.ID.ValueString()); err != nil && !isNotFound(err) {
		resp.Diagnostics.AddError("Deleting Dashboard Panel", err.Error())
	}
}

// ImportState takes "dashboard_id/panel_id" -- unlike the other
// resources, a panel's identity in the API isn't self-sufficient (Read
// needs the parent dashboard_id to know where to look, since there's no
// standalone GET for a panel -- see getPanel's doc comment), so a bare
// panel ID isn't enough to import from.
func (r *dashboardPanelResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	dashboardID, panelID, found := splitImportID(req.ID)
	if !found {
		resp.Diagnostics.AddError(
			"Unexpected Import Identifier",
			fmt.Sprintf("Expected import identifier of the form \"dashboard_id/panel_id\", got: %q", req.ID),
		)
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("dashboard_id"), dashboardID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), panelID)...)
}

// splitImportID splits "dashboard_id/panel_id" on the last "/" --
// dashboard IDs are server-generated UUIDs with no "/" in them today,
// but splitting on the *last* separator rather than the first is
// defensive against that changing, since the panel ID is what actually
// needs to be unambiguous here.
func splitImportID(id string) (dashboardID, panelID string, found bool) {
	i := strings.LastIndex(id, "/")
	if i < 0 {
		return "", "", false
	}
	return id[:i], id[i+1:], true
}
