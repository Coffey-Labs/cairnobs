package provider

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = &notificationTargetResource{}
	_ resource.ResourceWithConfigure   = &notificationTargetResource{}
	_ resource.ResourceWithImportState = &notificationTargetResource{}
)

func newNotificationTargetResource() resource.Resource {
	return &notificationTargetResource{}
}

// notificationTargetResource implements cairnobs_notification_target
// against alerting/internal/httpapi's POST/GET/DELETE
// /targets[/{id}] endpoints.
//
// Deliberately create/destroy only, every attribute RequiresReplace --
// same reasoning as alertRuleResource (see that file's doc comment):
// alerting has no PUT /targets/{id} at all, confirmed down to
// notifystore.Store, which has Create/List/Get/Delete but no Update.
type notificationTargetResource struct {
	client *client
}

type notificationTargetResourceModel struct {
	ID              types.String `tfsdk:"id"`
	TenantID        types.String `tfsdk:"tenant_id"`
	Name            types.String `tfsdk:"name"`
	Kind            types.String `tfsdk:"kind"`
	WebhookURL      types.String `tfsdk:"webhook_url"`
	PayloadTemplate types.String `tfsdk:"payload_template"`
	Headers         types.String `tfsdk:"headers"`
	Secret          types.String `tfsdk:"secret"`
	CreatedBy       types.String `tfsdk:"created_by"`
}

func (r *notificationTargetResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_notification_target"
}

func (r *notificationTargetResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	replace := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		Description: "A Cairn OBS alert notification target. Create/destroy only -- alerting has no update " +
			"endpoint for targets today (see this resource's Go doc comment), so every attribute below " +
			"forces a destroy-and-recreate on change, never an in-place update.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
				Description:   "Server-generated target ID -- reference this from a cairnobs_alert_rule's notification_target_id.",
			},
			"tenant_id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:      true,
				PlanModifiers: replace,
				Description:   "Target name. The API rejects an empty string.",
			},
			"kind": schema.StringAttribute{
				Required:      true,
				PlanModifiers: replace,
				Description:   `One of "webhook", "slack", "pagerduty".`,
			},
			"webhook_url": schema.StringAttribute{
				Required:      true,
				PlanModifiers: replace,
				Description:   "Destination URL. The API rejects an empty string, regardless of kind.",
			},
			"payload_template": schema.StringAttribute{
				Optional:      true,
				PlanModifiers: replace,
				Description:   "Optional Go text/template string overriding the default payload shape for this target's kind.",
			},
			"headers": schema.StringAttribute{
				Optional:      true,
				PlanModifiers: replace,
				Description:   `Optional extra HTTP headers, as a JSON object string -- e.g. jsonencode({"X-Custom" = "value"}). Stored and returned as opaque JSON; this provider does not interpret it.`,
			},
			"secret": schema.StringAttribute{
				Optional:      true,
				Sensitive:     true,
				PlanModifiers: replace,
				Description: "Optional shared secret (e.g. for HMAC-signing outgoing webhook payloads). " +
					"alerting's GET /targets/{id} returns this back unredacted (confirmed in " +
					"notifystore/store.go -- no redaction at the store or handler layer), so it is " +
					"necessarily present in this resource's Terraform state in plaintext, the standard " +
					"caveat for any Sensitive Terraform attribute: treat state files as sensitive, " +
					"encrypt the backend, restrict who can read them.",
			},
			"created_by": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *notificationTargetResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func notificationTargetModelFromAPI(t *notificationTarget) notificationTargetResourceModel {
	m := notificationTargetResourceModel{
		ID:         types.StringValue(t.ID),
		TenantID:   types.StringValue(t.TenantID),
		Name:       types.StringValue(t.Name),
		Kind:       types.StringValue(t.Kind),
		WebhookURL: types.StringValue(t.WebhookURL),
		CreatedBy:  types.StringValue(t.CreatedBy),
	}
	if t.PayloadTemplate != nil {
		m.PayloadTemplate = types.StringValue(*t.PayloadTemplate)
	}
	if len(t.Headers) > 0 {
		m.Headers = types.StringValue(string(t.Headers))
	}
	if t.Secret != nil {
		m.Secret = types.StringValue(*t.Secret)
	}
	return m
}

func notificationTargetAPIFromModel(m notificationTargetResourceModel) (*notificationTarget, error) {
	t := &notificationTarget{
		Name:       m.Name.ValueString(),
		Kind:       m.Kind.ValueString(),
		WebhookURL: m.WebhookURL.ValueString(),
	}
	if !m.PayloadTemplate.IsNull() {
		v := m.PayloadTemplate.ValueString()
		t.PayloadTemplate = &v
	}
	if !m.Headers.IsNull() {
		raw := m.Headers.ValueString()
		if !json.Valid([]byte(raw)) {
			return nil, fmt.Errorf("headers must be valid JSON (use jsonencode(...) in the resource config), got: %s", raw)
		}
		t.Headers = json.RawMessage(raw)
	}
	if !m.Secret.IsNull() {
		v := m.Secret.ValueString()
		t.Secret = &v
	}
	return t, nil
}

func (r *notificationTargetResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan notificationTargetResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	in, err := notificationTargetAPIFromModel(plan)
	if err != nil {
		resp.Diagnostics.AddError("Invalid Configuration", err.Error())
		return
	}
	out, err := r.client.createNotificationTarget(ctx, in)
	if err != nil {
		resp.Diagnostics.AddError("Creating Notification Target", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, notificationTargetModelFromAPI(out))...)
}

func (r *notificationTargetResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state notificationTargetResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	out, err := r.client.getNotificationTarget(ctx, state.ID.ValueString())
	if err != nil {
		if isNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Reading Notification Target", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, notificationTargetModelFromAPI(out))...)
}

// Update should be unreachable in practice -- see alertRuleResource's
// Update doc comment for why this is a safe read-only passthrough
// rather than calling any mutating endpoint (alerting has none to call).
func (r *notificationTargetResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan notificationTargetResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	out, err := r.client.getNotificationTarget(ctx, plan.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Reading Notification Target", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, notificationTargetModelFromAPI(out))...)
}

func (r *notificationTargetResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state notificationTargetResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := r.client.deleteNotificationTarget(ctx, state.ID.ValueString()); err != nil && !isNotFound(err) {
		resp.Diagnostics.AddError("Deleting Notification Target", err.Error())
	}
}

func (r *notificationTargetResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
