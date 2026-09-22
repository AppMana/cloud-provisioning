package rig

import (
	"context"

	"github.com/appmana/labcontainers/pkg/bootstrap"
	labrig "github.com/appmana/labcontainers/pkg/rig"
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
type BootstrapConsumer = labrig.BootstrapConsumer

// InstanceBootstrapConsumer binds a launch to the infrastructure object's UID.
// Implementations retain launch state across retries and observer restarts.
type InstanceBootstrapConsumer = labrig.InstanceBootstrapConsumer

func BootstrapInstance(ctx context.Context, node Node, uid string, data BootstrapData) error {
	return labrig.BootstrapInstance(ctx, node, uid, data)
}

// Bootstrap preserves compatibility with cloud-config-only rigs while refusing
// to reinterpret an unsupported format. Native machines implement the explicit
// capability and validate before replacing a disk or changing guest state.
func Bootstrap(ctx context.Context, node Node, data BootstrapData) error {
	return labrig.Bootstrap(ctx, node, data)
}
