// Package provider is Sentry's Terraform provider implementation,
// built on HashiCorp's terraform-plugin-framework (not the legacy
// SDKv2 -- the framework is the actively-developed, currently-
// recommended library for a provider started from scratch, matching
// CLAUDE.md's "prefer boring, well-understood dependencies" read
// forward rather than backward).
package provider

import (
	"context"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ provider.Provider = &sentryProvider{}

// New matches providerserver.Serve's expected constructor shape --
// version is threaded through from main.go's -ldflags-injected build
// version.
func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &sentryProvider{version: version}
	}
}

type sentryProvider struct {
	version string
}

type sentryProviderModel struct {
	Endpoint         types.String `tfsdk:"endpoint"`
	AlertingEndpoint types.String `tfsdk:"alerting_endpoint"`
	Token            types.String `tfsdk:"token"`
}

// providerData is what Configure hands resources/data sources via
// req.ProviderData -- two separate clients, not one, because `alerting`
// is a genuinely separate service with its own base URL (its own
// REST API, its own port, sometimes its own deployment) -- same split
// web/src/lib/api.ts's apiBase/alertingBase and cli/cmd/sentryctl's
// --api/--alerting-api already draw, not something invented for this
// provider.
type providerData struct {
	api      *client
	alerting *client
}

func (p *sentryProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "sentry"
	resp.Version = p.version
}

func (p *sentryProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages Sentry log-aggregation-platform resources. Dashboards, alert rules, and notification targets for now -- tenant/RBAC resources are real, disclosed future work, not built in this pass; see the provider README.",
		Attributes: map[string]schema.Attribute{
			"endpoint": schema.StringAttribute{
				Optional: true,
				Description: "Base URL of the api service, e.g. \"http://localhost:8080\". Defaults to " +
					"$SENTRY_API_ENDPOINT, or \"http://localhost:8080\" if that's unset too -- same " +
					"default sentryctl's --api/$SENTRYCTL_API_URL uses (cli/cmd/sentryctl/main.go).",
			},
			"alerting_endpoint": schema.StringAttribute{
				Optional: true,
				Description: "Base URL of the alerting service, e.g. \"http://localhost:8081\" -- a " +
					"separate service from api, not a path under endpoint above (see " +
					"/docs/phase-3-alerting-design.md's component boundary). Defaults to " +
					"$SENTRY_ALERTING_API_ENDPOINT, or \"http://localhost:8081\" if that's unset too -- " +
					"same default sentryctl's --alerting-api/$SENTRYCTL_ALERTING_API_URL uses.",
			},
			"token": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				Description: "Bearer credential sent as \"Authorization: Bearer <token>\" on every request " +
					"-- required once a deployment configures enterprise-auth (see " +
					"/docs/phase-4-rbac-design.md), same as sentryctl's $SENTRYCTL_TOKEN. Defaults to " +
					"$SENTRY_API_TOKEN if unset. Set via a variable or environment, never a literal in a " +
					".tf file committed to version control.",
			},
		},
	}
}

// Configure resolves endpoint/token the same precedence order
// sentryctl's resolveAPIURL/resolveToken use (explicit config value,
// then an environment variable, then a hardcoded default) so behavior
// stays predictable across both of this project's Sentry API clients.
func (p *sentryProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var config sentryProviderModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	endpoint := config.Endpoint.ValueString()
	if endpoint == "" {
		endpoint = os.Getenv("SENTRY_API_ENDPOINT")
	}
	if endpoint == "" {
		endpoint = "http://localhost:8080"
	}

	alertingEndpoint := config.AlertingEndpoint.ValueString()
	if alertingEndpoint == "" {
		alertingEndpoint = os.Getenv("SENTRY_ALERTING_API_ENDPOINT")
	}
	if alertingEndpoint == "" {
		alertingEndpoint = "http://localhost:8081"
	}

	token := config.Token.ValueString()
	if token == "" {
		token = os.Getenv("SENTRY_API_TOKEN")
	}

	data := &providerData{
		api:      newClient(endpoint, token),
		alerting: newClient(alertingEndpoint, token),
	}
	resp.DataSourceData = data
	resp.ResourceData = data
}

func (p *sentryProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		newDashboardResource,
		newDashboardPanelResource,
		newAlertRuleResource,
		newNotificationTargetResource,
	}
}

func (p *sentryProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		newDashboardDataSource,
		newDashboardPanelDataSource,
		newAlertRuleDataSource,
		newNotificationTargetDataSource,
	}
}
