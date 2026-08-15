package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/float64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = &alertRuleResource{}
	_ resource.ResourceWithConfigure   = &alertRuleResource{}
	_ resource.ResourceWithImportState = &alertRuleResource{}
)

func newAlertRuleResource() resource.Resource {
	return &alertRuleResource{}
}

// alertRuleResource implements sentry_alert_rule against
// alerting/internal/httpapi's POST/GET/DELETE /rules[/{id}] endpoints.
//
// Deliberately create/destroy only, every attribute RequiresReplace:
// alerting has no PUT /rules/{id} at all -- confirmed down to
// rulestore.Store, which has Create/List/Get/Delete but no Update
// method to even wire one to, a real pre-existing gap in alerting's own
// API. Faking an in-place update via delete-then-recreate inside this
// resource was considered and rejected -- it would silently reset
// alert_state/delivery-log continuity a real operator might care about,
// a behavioral side effect this resource shouldn't paper over. See the
// provider README for the full reasoning and the option (adding a real
// PUT /rules/{id} to alerting) that would remove this constraint.
type alertRuleResource struct {
	client *client
}

type alertRuleResourceModel struct {
	ID                      types.String  `tfsdk:"id"`
	TenantID                types.String  `tfsdk:"tenant_id"`
	Name                    types.String  `tfsdk:"name"`
	Description             types.String  `tfsdk:"description"`
	Query                   types.String  `tfsdk:"query"`
	QueryLanguage           types.String  `tfsdk:"query_language"`
	ConditionType           types.String  `tfsdk:"condition_type"`
	Comparator              types.String  `tfsdk:"comparator"`
	ThresholdValue          types.Float64 `tfsdk:"threshold_value"`
	EvalIntervalSeconds     types.Int64   `tfsdk:"eval_interval_seconds"`
	ForMinutes              types.Int64   `tfsdk:"for_minutes"`
	RenotifyIntervalMinutes types.Int64   `tfsdk:"renotify_interval_minutes"`
	NotificationTargetID    types.String  `tfsdk:"notification_target_id"`
	Enabled                 types.Bool    `tfsdk:"enabled"`
	CreatedBy               types.String  `tfsdk:"created_by"`
}

func (r *alertRuleResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_alert_rule"
}

func (r *alertRuleResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	replace := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		Description: "A Sentry alert rule. Create/destroy only -- alerting has no update endpoint for " +
			"rules today (see this resource's Go doc comment), so every attribute below forces a " +
			"destroy-and-recreate on change, never an in-place update.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
				Description:   "Server-generated rule ID.",
			},
			"tenant_id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:      true,
				PlanModifiers: replace,
				Description:   "Rule name. The API rejects an empty string.",
			},
			"description": schema.StringAttribute{
				Optional:      true,
				Computed:      true,
				Default:       stringdefault.StaticString(""),
				PlanModifiers: replace,
			},
			"query": schema.StringAttribute{
				Required:      true,
				PlanModifiers: replace,
				Description:   "Pipe-syntax or SQL query text (see query_language) -- the same query the evaluator re-runs on every eval_interval_seconds tick. The API rejects an empty string.",
			},
			"query_language": schema.StringAttribute{
				Optional:      true,
				Computed:      true,
				Default:       stringdefault.StaticString(""),
				PlanModifiers: replace,
				Description:   `"" (auto-detect), "sql", or "spl" -- same values the query API itself accepts.`,
			},
			"condition_type": schema.StringAttribute{
				Required:      true,
				PlanModifiers: replace,
				Description:   `"threshold" (requires comparator + threshold_value) or "absence" (fires when the query returns zero rows).`,
			},
			"comparator": schema.StringAttribute{
				Optional:      true,
				PlanModifiers: replace,
				Description:   `Required (and only meaningful) when condition_type = "threshold": one of "gt", "gte", "lt", "lte", "eq", "ne".`,
			},
			"threshold_value": schema.Float64Attribute{
				Optional:      true,
				PlanModifiers: []planmodifier.Float64{float64planmodifier.RequiresReplace()},
				Description:   `Required (and only meaningful) when condition_type = "threshold".`,
			},
			"eval_interval_seconds": schema.Int64Attribute{
				Required:      true,
				PlanModifiers: []planmodifier.Int64{int64planmodifier.RequiresReplace()},
				Description:   "How often the evaluator re-runs this rule's query. The API rejects anything below 30.",
			},
			"for_minutes": schema.Int64Attribute{
				Optional:      true,
				Computed:      true,
				Default:       int64default.StaticInt64(0),
				PlanModifiers: []planmodifier.Int64{int64planmodifier.RequiresReplace()},
				Description:   "How long the condition must stay true before the rule transitions from pending to firing. 0 means fire immediately on the first true evaluation.",
			},
			"renotify_interval_minutes": schema.Int64Attribute{
				Optional:      true,
				PlanModifiers: []planmodifier.Int64{int64planmodifier.RequiresReplace()},
				Description:   "How often to re-send a notification while still firing. Unset means notify once per firing transition only.",
			},
			"notification_target_id": schema.StringAttribute{
				Required:      true,
				PlanModifiers: replace,
				Description:   "ID of a sentry_notification_target-managed (or manually created) notification target. The API rejects an empty string. No sentry_notification_target resource exists yet -- see the provider README -- so this has to be a target created some other way (sentryctl, curl, or the web UI) for now.",
			},
			"enabled": schema.BoolAttribute{
				Optional:      true,
				Computed:      true,
				Default:       booldefault.StaticBool(true),
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.RequiresReplace()},
				Description:   "Whether the evaluator considers this rule at all. Defaults to true, matching POST /rules' own default when the field is omitted.",
			},
			"created_by": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *alertRuleResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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
	r.client = data.alerting
}

func alertRuleModelFromAPI(rl *rule) alertRuleResourceModel {
	m := alertRuleResourceModel{
		ID:                   types.StringValue(rl.ID),
		TenantID:             types.StringValue(rl.TenantID),
		Name:                 types.StringValue(rl.Name),
		Description:          types.StringValue(rl.Description),
		Query:                types.StringValue(rl.Query),
		QueryLanguage:        types.StringValue(rl.QueryLanguage),
		ConditionType:        types.StringValue(rl.ConditionType),
		EvalIntervalSeconds:  types.Int64Value(int64(rl.EvalIntervalSeconds)),
		ForMinutes:           types.Int64Value(int64(rl.ForMinutes)),
		NotificationTargetID: types.StringValue(rl.NotificationTargetID),
		CreatedBy:            types.StringValue(rl.CreatedBy),
	}
	if rl.Comparator != nil {
		m.Comparator = types.StringValue(*rl.Comparator)
	}
	if rl.ThresholdValue != nil {
		m.ThresholdValue = types.Float64Value(*rl.ThresholdValue)
	}
	if rl.RenotifyIntervalMinutes != nil {
		m.RenotifyIntervalMinutes = types.Int64Value(int64(*rl.RenotifyIntervalMinutes))
	}
	// Enabled always comes back set from the API (Rule.Enabled is a
	// plain bool, not a pointer, in rulestore -- see store.go) --
	// unlike Comparator/ThresholdValue/RenotifyIntervalMinutes above,
	// this is never legitimately null in a response.
	enabled := true
	if rl.Enabled != nil {
		enabled = *rl.Enabled
	}
	m.Enabled = types.BoolValue(enabled)
	return m
}

func alertRuleAPIFromModel(m alertRuleResourceModel) *rule {
	rl := &rule{
		Name:                 m.Name.ValueString(),
		Description:          m.Description.ValueString(),
		Query:                m.Query.ValueString(),
		QueryLanguage:        m.QueryLanguage.ValueString(),
		ConditionType:        m.ConditionType.ValueString(),
		EvalIntervalSeconds:  int(m.EvalIntervalSeconds.ValueInt64()),
		ForMinutes:           int(m.ForMinutes.ValueInt64()),
		NotificationTargetID: m.NotificationTargetID.ValueString(),
	}
	if !m.Comparator.IsNull() {
		v := m.Comparator.ValueString()
		rl.Comparator = &v
	}
	if !m.ThresholdValue.IsNull() {
		v := m.ThresholdValue.ValueFloat64()
		rl.ThresholdValue = &v
	}
	if !m.RenotifyIntervalMinutes.IsNull() {
		v := m.RenotifyIntervalMinutes.ValueInt64()
		vInt := int(v)
		rl.RenotifyIntervalMinutes = &vInt
	}
	if !m.Enabled.IsNull() {
		v := m.Enabled.ValueBool()
		rl.Enabled = &v
	}
	return rl
}

func (r *alertRuleResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan alertRuleResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	out, err := r.client.createRule(ctx, alertRuleAPIFromModel(plan))
	if err != nil {
		resp.Diagnostics.AddError("Creating Alert Rule", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, alertRuleModelFromAPI(out))...)
}

func (r *alertRuleResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state alertRuleResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	out, err := r.client.getRule(ctx, state.ID.ValueString())
	if err != nil {
		if isNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Reading Alert Rule", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, alertRuleModelFromAPI(out))...)
}

// Update should be unreachable in practice: every non-Computed
// attribute above carries RequiresReplace, so a real config change
// always plans a destroy-and-recreate instead of an in-place update.
// Still required to satisfy resource.Resource's interface -- implemented
// as a safe passthrough (just re-read the current server state into the
// plan's ID) rather than calling any mutating endpoint, since alerting
// has no PUT /rules/{id} to call in the first place.
func (r *alertRuleResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan alertRuleResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	out, err := r.client.getRule(ctx, plan.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Reading Alert Rule", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, alertRuleModelFromAPI(out))...)
}

func (r *alertRuleResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state alertRuleResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := r.client.deleteRule(ctx, state.ID.ValueString()); err != nil && !isNotFound(err) {
		resp.Diagnostics.AddError("Deleting Alert Rule", err.Error())
	}
}

func (r *alertRuleResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
