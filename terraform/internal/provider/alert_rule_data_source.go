package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
)

var (
	_ datasource.DataSource              = &alertRuleDataSource{}
	_ datasource.DataSourceWithConfigure = &alertRuleDataSource{}
)

func newAlertRuleDataSource() datasource.DataSource {
	return &alertRuleDataSource{}
}

// alertRuleDataSource looks up an existing alert rule by ID -- see
// dashboardDataSource's doc comment for why this reuses
// alertRuleResourceModel/alertRuleModelFromAPI rather than a parallel
// type.
type alertRuleDataSource struct {
	client *client
}

func (d *alertRuleDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_alert_rule"
}

func (d *alertRuleDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Looks up an existing Sentry alert rule by ID. See the sentry_alert_rule resource for how one is created/managed.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Required:    true,
				Description: "Rule ID to look up.",
			},
			"tenant_id":                 schema.StringAttribute{Computed: true},
			"name":                      schema.StringAttribute{Computed: true},
			"description":               schema.StringAttribute{Computed: true},
			"query":                     schema.StringAttribute{Computed: true},
			"query_language":            schema.StringAttribute{Computed: true},
			"condition_type":            schema.StringAttribute{Computed: true},
			"comparator":                schema.StringAttribute{Computed: true},
			"threshold_value":           schema.Float64Attribute{Computed: true},
			"eval_interval_seconds":     schema.Int64Attribute{Computed: true},
			"for_minutes":               schema.Int64Attribute{Computed: true},
			"renotify_interval_minutes": schema.Int64Attribute{Computed: true},
			"notification_target_id":    schema.StringAttribute{Computed: true},
			"enabled":                   schema.BoolAttribute{Computed: true},
			"created_by":                schema.StringAttribute{Computed: true},
		},
	}
}

func (d *alertRuleDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	data, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Data Source Configure Type",
			fmt.Sprintf("Expected *provider.providerData, got: %T. This is a provider bug -- please report it.", req.ProviderData),
		)
		return
	}
	d.client = data.alerting
}

func (d *alertRuleDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config alertRuleResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	out, err := d.client.getRule(ctx, config.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Reading Alert Rule", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, alertRuleModelFromAPI(out))...)
}
