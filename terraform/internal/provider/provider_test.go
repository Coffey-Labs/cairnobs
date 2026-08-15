package provider

import (
	"context"
	"testing"

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
	for _, attr := range []string{"endpoint", "token"} {
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
