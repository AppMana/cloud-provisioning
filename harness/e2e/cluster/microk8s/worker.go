package microk8s

import (
	"context"
	"fmt"
	"strings"

	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

func verifySnap(ctx context.Context, node rig.Node) error {
	out, err := node.Exec(ctx, "snap", "list", "microk8s")
	if err != nil {
		return fmt.Errorf("observing MicroK8s snap: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 {
		return fmt.Errorf("cannot verify MicroK8s snap")
	}
	fields := strings.Fields(lines[1])
	if len(fields) < 3 || fields[0] != "microk8s" || fields[1] != Version || fields[2] != Revision {
		return fmt.Errorf("MicroK8s snap version/revision does not match %s/%s", Version, Revision)
	}
	return nil
}

func (Builder) VerifyWorker(ctx context.Context, node rig.Node) error {
	if err := verifySnap(ctx, node); err != nil {
		return err
	}
	_, err := node.Exec(ctx, "test", "-S", strings.TrimPrefix((Builder{}).CRIEndpoint(), "unix://"))
	return err
}
