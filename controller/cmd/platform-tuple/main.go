// Command platform-tuple renders k0s/Calico Windows BGP resources and evaluates
// platform tuple health and environment checks (e.g. "k0s + calico,bgp + aws").
package main

import (
	"context"
	"io"
	"os"

	tuplecli "github.com/appmana/cloud-provisioning/controller/pkg/tuple/cli"
)

func run(ctx context.Context, args []string, out, diagnostics io.Writer) error {
	return tuplecli.Run(ctx, args, out, diagnostics)
}

func main() {
	os.Exit(tuplecli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
