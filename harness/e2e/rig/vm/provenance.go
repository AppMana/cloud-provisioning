package vm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

// LabcontainersRevision is the source of the SDK, guest helper and launcher.
// Keep it synchronized with go.mod; the contract test rejects drift.
const LabcontainersRevision = rig.LabcontainersRevision

// verifiedImage resolves a mutable local tag once, before changing any lab.
func (r *Rig) verifiedImage(ctx context.Context) (string, error) {
	raw, stderr, code, err := r.runner()(ctx, nil, "docker", "image", "inspect", lab.VMImage)
	if err != nil || code != 0 {
		return "", fmt.Errorf("inspect VM image: %v (exit %d): %s", err, code, stderr)
	}
	var images []struct {
		ID     string                             `json:"Id"`
		Config struct{ Labels map[string]string } `json:"Config"`
	}
	if err := json.Unmarshal(raw, &images); err != nil {
		return "", fmt.Errorf("decode VM image provenance: %w", err)
	}
	if len(images) != 1 || !strings.HasPrefix(images[0].ID, "sha256:") || len(images[0].ID) != 71 {
		return "", fmt.Errorf("VM image has no immutable identity")
	}
	if got := images[0].Config.Labels["appmana.labcontainers.revision"]; got != LabcontainersRevision {
		return "", fmt.Errorf("VM wrapper labcontainers revision %q does not match SDK %s; rebuild with rig/vm/image/build.sh", got, LabcontainersRevision)
	}
	return images[0].ID, nil
}
