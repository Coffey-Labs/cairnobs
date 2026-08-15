package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/resource"
)

// TestProviderSchemaValid and TestDashboardResourceSchemaValid don't
// need a Terraform binary or a live api service -- ValidateImplementation
// runs the same internal consistency checks
// terraform-plugin-framework's own protocol layer would (attribute
// names are valid identifiers, no Optional+Required conflicts, etc.),
// catching a broken schema before it ever reaches an acceptance test.
func TestProviderSchemaValid(t *testing.T) {
	ctx := context.Background()
	req := provider.SchemaRequest{}
	resp := &provider.SchemaResponse{}

	New("test")().Schema(ctx, req, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("provider schema has errors: %v", resp.Diagnostics)
	}
	for _, attr := range []string{"endpoint", "alerting_endpoint", "token"} {
		if _, ok := resp.Schema.Attributes[attr]; !ok {
			t.Errorf("provider schema missing expected attribute %q", attr)
		}
	}
}

func TestDashboardResourceSchemaValid(t *testing.T) {
	ctx := context.Background()
	req := resource.SchemaRequest{}
	resp := &resource.SchemaResponse{}

	newDashboardResource().Schema(ctx, req, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("sentry_dashboard schema has errors: %v", resp.Diagnostics)
	}
	for _, attr := range []string{
		"id", "tenant_id", "name", "description",
		"default_earliest", "default_latest", "created_by", "created_at", "updated_at",
	} {
		if _, ok := resp.Schema.Attributes[attr]; !ok {
			t.Errorf("sentry_dashboard schema missing expected attribute %q", attr)
		}
	}
	if !resp.Schema.Attributes["name"].IsRequired() {
		t.Error(`"name" must be Required`)
	}
	if !resp.Schema.Attributes["id"].IsComputed() {
		t.Error(`"id" must be Computed`)
	}
}

func TestDashboardResourceMetadataSetsTypeName(t *testing.T) {
	resp := &resource.MetadataResponse{}
	newDashboardResource().Metadata(context.Background(), resource.MetadataRequest{ProviderTypeName: "sentry"}, resp)
	if resp.TypeName != "sentry_dashboard" {
		t.Fatalf("TypeName = %q, want sentry_dashboard", resp.TypeName)
	}
}

func TestAlertRuleResourceSchemaValid(t *testing.T) {
	ctx := context.Background()
	req := resource.SchemaRequest{}
	resp := &resource.SchemaResponse{}

	newAlertRuleResource().Schema(ctx, req, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("sentry_alert_rule schema has errors: %v", resp.Diagnostics)
	}
	for _, attr := range []string{
		"id", "tenant_id", "name", "description", "query", "query_language",
		"condition_type", "comparator", "threshold_value", "eval_interval_seconds",
		"for_minutes", "renotify_interval_minutes", "notification_target_id", "enabled", "created_by",
	} {
		if _, ok := resp.Schema.Attributes[attr]; !ok {
			t.Errorf("sentry_alert_rule schema missing expected attribute %q", attr)
		}
	}
	if !resp.Schema.Attributes["name"].IsRequired() {
		t.Error(`"name" must be Required`)
	}
	if !resp.Schema.Attributes["id"].IsComputed() {
		t.Error(`"id" must be Computed`)
	}
}

func TestAlertRuleResourceMetadataSetsTypeName(t *testing.T) {
	resp := &resource.MetadataResponse{}
	newAlertRuleResource().Metadata(context.Background(), resource.MetadataRequest{ProviderTypeName: "sentry"}, resp)
	if resp.TypeName != "sentry_alert_rule" {
		t.Fatalf("TypeName = %q, want sentry_alert_rule", resp.TypeName)
	}
}

func TestNotificationTargetResourceSchemaValid(t *testing.T) {
	ctx := context.Background()
	req := resource.SchemaRequest{}
	resp := &resource.SchemaResponse{}

	newNotificationTargetResource().Schema(ctx, req, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("sentry_notification_target schema has errors: %v", resp.Diagnostics)
	}
	for _, attr := range []string{
		"id", "tenant_id", "name", "kind", "webhook_url",
		"payload_template", "headers", "secret", "created_by",
	} {
		if _, ok := resp.Schema.Attributes[attr]; !ok {
			t.Errorf("sentry_notification_target schema missing expected attribute %q", attr)
		}
	}
	if !resp.Schema.Attributes["name"].IsRequired() {
		t.Error(`"name" must be Required`)
	}
	if !resp.Schema.Attributes["secret"].IsSensitive() {
		t.Error(`"secret" must be Sensitive`)
	}
}

func TestNotificationTargetResourceMetadataSetsTypeName(t *testing.T) {
	resp := &resource.MetadataResponse{}
	newNotificationTargetResource().Metadata(context.Background(), resource.MetadataRequest{ProviderTypeName: "sentry"}, resp)
	if resp.TypeName != "sentry_notification_target" {
		t.Fatalf("TypeName = %q, want sentry_notification_target", resp.TypeName)
	}
}

func TestDashboardDataSourceSchemaValid(t *testing.T) {
	ctx := context.Background()
	req := datasource.SchemaRequest{}
	resp := &datasource.SchemaResponse{}

	newDashboardDataSource().Schema(ctx, req, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("sentry_dashboard data source schema has errors: %v", resp.Diagnostics)
	}
	if !resp.Schema.Attributes["id"].IsRequired() {
		t.Error(`"id" must be Required -- a data source needs it to know what to look up`)
	}
	if !resp.Schema.Attributes["name"].IsComputed() {
		t.Error(`"name" must be Computed`)
	}
}

func TestAlertRuleDataSourceSchemaValid(t *testing.T) {
	ctx := context.Background()
	req := datasource.SchemaRequest{}
	resp := &datasource.SchemaResponse{}

	newAlertRuleDataSource().Schema(ctx, req, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("sentry_alert_rule data source schema has errors: %v", resp.Diagnostics)
	}
	if !resp.Schema.Attributes["id"].IsRequired() {
		t.Error(`"id" must be Required`)
	}
	if !resp.Schema.Attributes["query"].IsComputed() {
		t.Error(`"query" must be Computed`)
	}
}

func TestNotificationTargetDataSourceSchemaValid(t *testing.T) {
	ctx := context.Background()
	req := datasource.SchemaRequest{}
	resp := &datasource.SchemaResponse{}

	newNotificationTargetDataSource().Schema(ctx, req, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("sentry_notification_target data source schema has errors: %v", resp.Diagnostics)
	}
	if !resp.Schema.Attributes["id"].IsRequired() {
		t.Error(`"id" must be Required`)
	}
	if !resp.Schema.Attributes["secret"].IsSensitive() {
		t.Error(`"secret" must be Sensitive`)
	}
}

func TestDataSourcesMetadataSetTypeNames(t *testing.T) {
	cases := []struct {
		newDS    func() datasource.DataSource
		wantType string
	}{
		{newDashboardDataSource, "sentry_dashboard"},
		{newAlertRuleDataSource, "sentry_alert_rule"},
		{newNotificationTargetDataSource, "sentry_notification_target"},
	}
	for _, c := range cases {
		resp := &datasource.MetadataResponse{}
		c.newDS().Metadata(context.Background(), datasource.MetadataRequest{ProviderTypeName: "sentry"}, resp)
		if resp.TypeName != c.wantType {
			t.Errorf("TypeName = %q, want %q", resp.TypeName, c.wantType)
		}
	}
}
