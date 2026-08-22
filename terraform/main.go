// Command terraform-provider-cairnobs is Cairn OBS's Terraform provider --
// CLAUDE.md names it a first-class deliverable alongside cairnobsctl
// ("CLI and Terraform provider are first-class, not afterthoughts"),
// but this is the first phase to actually build any of it.
//
// Scoped deliberately narrow to start: one resource
// (internal/provider.dashboardResource, cairnobs_dashboard), reusing the
// exact same JSON contract cli/cmd/cairnobsctl's "apply" subcommand and
// web's dashboard export button already use against
// api/dashboards.Handler -- "one JSON contract, multiple callers" is a
// design decision made back in Phase 3 (see cli/README.md), this
// provider is just a third caller of it, not a new contract. Alert
// rules, notification targets, and tenant/RBAC resources are real,
// disclosed future work, not attempted in this pass -- see README.md.
package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/cairnobs/cairnobs/terraform/internal/provider"
)

// version is overridden at build time via -ldflags, same convention
// HashiCorp's own scaffold and every published provider use --
// Terraform's registry protocol reports this to users running
// `terraform version`. "dev" is deliberately obvious in output if
// someone runs a local build without setting it.
var version = "dev"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "run the provider with support for debuggers like delve")
	flag.Parse()

	err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		// Matches this provider's eventual Terraform Registry address --
		// required by the protocol even before actual registry
		// publication, since local dev overrides
		// (~/.terraformrc dev_overrides) key on this same address.
		Address: "registry.terraform.io/cairnobs/cairnobs",
		Debug:   debug,
	})
	if err != nil {
		log.Fatal(err.Error())
	}
}
