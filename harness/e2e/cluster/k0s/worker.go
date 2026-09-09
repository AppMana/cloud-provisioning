package k0s

import (
	"context"
	"fmt"
	"strings"

	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

func (Builder) VerifyWorker(ctx context.Context, node rig.Node) error {
	version, err := node.Exec(ctx, "k0s", "version")
	if err != nil {
		return fmt.Errorf("observing k0s worker release: %w", err)
	}
	if strings.TrimSpace(string(version)) != Version {
		return fmt.Errorf("k0s worker release does not match %s", Version)
	}
	_, err = node.Exec(ctx, "test", "-S", strings.TrimPrefix((Builder{}).CRIEndpoint(), "unix://"))
	return err
}
