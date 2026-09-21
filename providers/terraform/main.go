package main

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
)

var version = "dev"

func main() {
	providerserver.Serve(context.Background(), New, providerserver.ServeOpts{
		Address: "registry.example.com/platform/platformipam",
		Debug:   false,
	})
}
