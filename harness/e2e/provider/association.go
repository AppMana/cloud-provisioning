package provider

import (
	"context"

	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
)

// WaitAssociation delegates the provider-independent CAPI identity gate.
func (c *Controller) WaitAssociation(ctx context.Context, namespace, machine, node string) error {
	return kube.WaitMachineAssociation(ctx, c.Kube, namespace, machine, node)
}
