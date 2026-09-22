package install

import (
	"context"
	"fmt"

	labcapi "github.com/appmana/labcontainers/pkg/capi"
)

// CAPIVersion is the Cluster API release whose kinds this lab serves.
const CAPIVersion = "v1.11.1"

// labCRDs are the lab's own infrastructure kinds: the machine and
// cluster a claim names, and the template it is created from.
//
// Embedded rather than read from a path, so the harness carries its
// own API and a run cannot half-work because a file was somewhere
// else.
var labCRDs = labcapi.CRDs

// ApplyCRDs installs the lab provider resources. InstallCAPI installs the real
// CAPI controllers and their own CRDs separately.
func (p *Product) ApplyCRDs(ctx context.Context) error {
	if err := p.Kube.Apply(ctx, labCRDs); err != nil {
		return fmt.Errorf("installing the lab's own kinds: %w", err)
	}

	// Cluster API's kinds arrive with Cluster API itself, which is
	// installed in full: the controllers are the dependency, not just
	// the schema.
	return nil
}
