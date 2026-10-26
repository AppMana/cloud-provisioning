package vm

import (
	"bytes"
	"context"
	"fmt"

	"github.com/appmana/labcontainers/pkg/containerd"
	"github.com/appmana/labcontainers/pkg/rig"
)

// Load carries an image from this host into a machine's runtime.
//
// A machine in this lab has no route to this host or to a registry,
// exactly as a real one in a cloud has none to whatever built its
// images. The image is exported once and carried to each guest by
// labcontainers; which command imports it is the distribution's.
func (r *Rig) Load(ctx context.Context, image string, nodes []string, importArgs []string) error {
	if len(importArgs) == 0 {
		importArgs = []string{"ctr", "-n", "k8s.io", "images", "import", "-"}
	}
	if _, _, code, err := r.runner()(ctx, nil, "docker", "image", "inspect", image); err != nil || code != 0 {
		if _, errb, code, err := r.runner()(ctx, nil, "docker", "pull", "-q", image); err != nil || code != 0 {
			return fmt.Errorf("pulling %s: %v %s", image, err, errb)
		}
	}
	if len(nodes) == 0 {
		return nil
	}
	saved, stderr, code, err := r.runner()(ctx, nil, "docker", "save", image)
	if err != nil || code != 0 {
		return fmt.Errorf("saving %s: %v %s", image, err, stderr)
	}
	guests := make([]rig.Commands, len(nodes))
	for i, name := range nodes {
		guests[i] = r.Node(name)
	}
	if err := containerd.CarryArchive(ctx, bytes.NewReader(saved), importArgs, guests...); err != nil {
		return fmt.Errorf("loading %s: %w", image, err)
	}
	return nil
}
