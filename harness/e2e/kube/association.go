package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/appmana/cloud-provisioning/harness/e2e/wait"
	"time"
)

// WaitMachineAssociation requires CAPI's own Node association and Running phase.
// CAPI v1beta2 references Nodes by name; the lifecycle driver separately asserts
// that removal/recreation produces a new Node UID.
func (c *Client) WaitMachineAssociation(ctx context.Context, namespace, machine, node string) error {
	return wait.Until(ctx, 5*time.Minute, "CAPI did not associate "+machine+" with its current Node", func(ctx context.Context) error {
		var m struct {
			Spec   struct{ ProviderID string } `json:"spec"`
			Status struct {
				NodeRef struct{ Name string }
				Phase   string
			} `json:"status"`
		}
		raw, err := c.Run(ctx, "-n", namespace, "get", "machine", machine, "-o", "json")
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
		var n struct {
			Metadata struct{ UID string }        `json:"metadata"`
			Spec     struct{ ProviderID string } `json:"spec"`
		}
		raw, err = c.Run(ctx, "get", "node", node, "-o", "json")
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &n); err != nil {
			return err
		}
		if m.Status.NodeRef.Name != node || m.Status.Phase != "Running" || n.Metadata.UID == "" || m.Spec.ProviderID == "" || n.Spec.ProviderID != m.Spec.ProviderID {
			return fmt.Errorf("Machine/Node identity has not converged")
		}
		return nil
	})
}
