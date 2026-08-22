package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
)

var (
	_ datasource.DataSource              = &dashboardDataSource{}
	_ datasource.DataSourceWithConfigure = &dashboardDataSource{}
)

func newDashboardDataSource() datasource.DataSource {
	return &dashboardDataSource{}
}

// dashboardDataSource looks up an existing dashboard by ID -- read-only,
// GET /dashboards/{id} only, no lifecycle of its own. Reuses
// dashboardResourceModel/dashboardModelFromAPI from
// dashboard_resource.go directly rather than defining a parallel type:
// a data source's attribute set here is exactly the resource's (every
// field Computed except id, which the caller supplies), so there's
// nothing a second struct would express that the first doesn't already.
type dashboardDataSource struct {
	client *client
}

func (d *dashboardDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_dashboard"
}

func (d *dashboardDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Looks up an existing Cairn OBS dashboard by ID. See the cairnobs_dashboard resource for how one is created/managed.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Required:    true,
				Description: "Dashboard ID to look up.",
			},
			"tenant_id":        schema.StringAttribute{Computed: true},
			"name":             schema.StringAttribute{Computed: true},
			"description":      schema.StringAttribute{Computed: true},
			"default_earliest": schema.StringAttribute{Computed: true},
			"default_latest":   schema.StringAttribute{Computed: true},
			"created_by":       schema.StringAttribute{Computed: true},
			"created_at":       schema.StringAttribute{Computed: true},
			"updated_at":       schema.StringAttribute{Computed: true},
		},
	}
}

func (d *dashboardDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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
	d.client = data.api
}

func (d *dashboardDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config dashboardResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	out, err := d.client.getDashboard(ctx, config.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Reading Dashboard", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, dashboardModelFromAPI(out))...)
}
