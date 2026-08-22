package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
)

var (
	_ datasource.DataSource              = &dashboardPanelDataSource{}
	_ datasource.DataSourceWithConfigure = &dashboardPanelDataSource{}
)

func newDashboardPanelDataSource() datasource.DataSource {
	return &dashboardPanelDataSource{}
}

// dashboardPanelDataSource looks up an existing panel by
// (dashboard_id, id) -- both Required, unlike the other three data
// sources' single Required id, because getPanel itself needs both
// (there's no standalone GET for a panel, only
// GET /dashboards/{id}, see that method's doc comment in client.go).
type dashboardPanelDataSource struct {
	client *client
}

func (d *dashboardPanelDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_dashboard_panel"
}

func (d *dashboardPanelDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Looks up an existing Cairn OBS dashboard panel by (dashboard_id, id). See the cairnobs_dashboard_panel resource for how one is created/managed.",
		Attributes: map[string]schema.Attribute{
			"dashboard_id": schema.StringAttribute{
				Required:    true,
				Description: "ID of the parent cairnobs_dashboard.",
			},
			"id": schema.StringAttribute{
				Required:    true,
				Description: "Panel ID to look up.",
			},
			"title":             schema.StringAttribute{Computed: true},
			"query":             schema.StringAttribute{Computed: true},
			"query_language":    schema.StringAttribute{Computed: true},
			"viz_type":          schema.StringAttribute{Computed: true},
			"viz_config":        schema.StringAttribute{Computed: true},
			"position_x":        schema.Int64Attribute{Computed: true},
			"position_y":        schema.Int64Attribute{Computed: true},
			"width":             schema.Int64Attribute{Computed: true},
			"height":            schema.Int64Attribute{Computed: true},
			"earliest_override": schema.StringAttribute{Computed: true},
			"latest_override":   schema.StringAttribute{Computed: true},
			"sort_order":        schema.Int64Attribute{Computed: true},
			"created_at":        schema.StringAttribute{Computed: true},
			"updated_at":        schema.StringAttribute{Computed: true},
		},
	}
}

func (d *dashboardPanelDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

func (d *dashboardPanelDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config dashboardPanelResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	dashboardID := config.DashboardID.ValueString()
	out, err := d.client.getPanel(ctx, dashboardID, config.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Reading Dashboard Panel", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, dashboardPanelModelFromAPI(dashboardID, out))...)
}
