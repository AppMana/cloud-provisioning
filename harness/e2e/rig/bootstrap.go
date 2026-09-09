package rig

import (
	"context"
	"fmt"

	"github.com/appmana/cloud-provisioning/controller/pkg/bootstrap"
)

// BootstrapData is the CAPI bootstrap Secret contract. Format is independent
// of the distribution and infrastructure names: Ubuntu consumes cloud-config,
// SCOS consumes Ignition, and CAPA forwards the same contract to AWS.
type BootstrapData = bootstrap.Data

const (
	CloudConfig = bootstrap.CloudConfig
	Ignition    = bootstrap.Ignition
	PowerShell  = bootstrap.PowerShell
)

// BootstrapConsumer accepts native first-boot data. Keeping format negotiation
// on the machine implementation avoids OS or cloud switches in lifecycle code.
type BootstrapConsumer interface {
	Bootstrap(context.Context, BootstrapData) error
}

// Bootstrap preserves compatibility with cloud-config-only rigs while refusing
// to reinterpret an unsupported format. Native machines implement the explicit
// capability and validate before replacing a disk or changing guest state.
func Bootstrap(ctx context.Context, node Node, data BootstrapData) error {
	if err := data.Validate(); err != nil {
		return fmt.Errorf("%s: %w", node.Name(), err)
	}
	if native, ok := node.(BootstrapConsumer); ok {
		return native.Bootstrap(ctx, data)
	}
	if data.Format != CloudConfig {
		return fmt.Errorf("%s: machine does not support bootstrap format %q", node.Name(), data.Format)
	}
	return node.Userdata(ctx, data.Value)
}
