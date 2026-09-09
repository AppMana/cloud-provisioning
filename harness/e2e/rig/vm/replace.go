package vm

import (
	"context"
	"fmt"
	"strings"
)

// DestroyDeployed removes the lab by its deployed identity, before a new
// topology is installed. Reconfigure with the new topology leaves behind old
// nodes absent from it (observed when switching five-node k0s to compact OKD).
func (r *Rig) DestroyDeployed(ctx context.Context) error {
	out, stderr, code, err := r.runner()(ctx, nil, "docker", "ps", "-aq", "--filter", "label=containerlab="+r.Topology.Name)
	if err != nil || code != 0 {
		return fmt.Errorf("finding deployed lab: %v, exit %d: %s", err, code, stderr)
	}
	if strings.TrimSpace(string(out)) == "" {
		return nil
	}
	_, stderr, code, err = r.runner()(ctx, nil, "sudo", "containerlab", "destroy", "--name", r.Topology.Name)
	if err != nil || code != 0 {
		return fmt.Errorf("destroying deployed lab: %v, exit %d: %s", err, code, stderr)
	}
	return nil
}
