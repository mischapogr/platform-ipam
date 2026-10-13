package main

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
)

var version = "dev"
var providerAddress = "registry.example.com/platform/platformipam"

func main() {
	providerserver.Serve(context.Background(), New, providerserver.ServeOpts{
		Address: providerAddress,
		Debug:   false,
	})
}
