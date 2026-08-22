package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
)

var (
	_ datasource.DataSource              = &notificationTargetDataSource{}
	_ datasource.DataSourceWithConfigure = &notificationTargetDataSource{}
)

func newNotificationTargetDataSource() datasource.DataSource {
	return &notificationTargetDataSource{}
}

// notificationTargetDataSource looks up an existing notification target
// by ID -- see dashboardDataSource's doc comment for why this reuses
// notificationTargetResourceModel/notificationTargetModelFromAPI rather
// than a parallel type.
type notificationTargetDataSource struct {
	client *client
}

func (d *notificationTargetDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_notification_target"
}

func (d *notificationTargetDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Looks up an existing Cairn OBS notification target by ID. See the cairnobs_notification_target resource for how one is created/managed.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Required:    true,
				Description: "Target ID to look up.",
			},
			"tenant_id":   schema.StringAttribute{Computed: true},
			"name":        schema.StringAttribute{Computed: true},
			"kind":        schema.StringAttribute{Computed: true},
			"webhook_url": schema.StringAttribute{Computed: true},
			"payload_template": schema.StringAttribute{
				Computed: true,
			},
			"headers": schema.StringAttribute{Computed: true},
			"secret": schema.StringAttribute{
				Computed:  true,
				Sensitive: true,
				Description: "alerting's own GET /targets/{id} returns this unredacted (see the " +
					"cairnobs_notification_target resource's schema doc comment) -- Sensitive here for the " +
					"same reason, and the same state-file caveat applies.",
			},
			"created_by": schema.StringAttribute{Computed: true},
		},
	}
}

func (d *notificationTargetDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

func (d *notificationTargetDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config notificationTargetResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	out, err := d.client.getNotificationTarget(ctx, config.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Reading Notification Target", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, notificationTargetModelFromAPI(out))...)
}
