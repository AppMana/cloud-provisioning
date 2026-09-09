package vm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
)

// Load carries an image from this host into a machine's runtime.
//
// The stream goes through the wrapper and into the guest, because
// that is the only path to it: a machine in this lab has no route to
// this host and no route to a registry, exactly as a real one in a
// cloud has none to whatever built its images.
//
// Which command takes it is the distribution's business, the same as
// on containers — a distribution that brings its own containerd does
// not share the one on the node's PATH, and an image imported into
// the wrong one is invisible to the kubelet that needs it.
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
	// Export once and import into one guest at a time. During concurrent
	// transfers, the eight-VM runner hit its memory limit and lost QEMU.
	// Keeping the single export avoids repeating the costly docker save step.
	saved, stderr, code, err := r.runner()(ctx, nil, "docker", "save", image)
	if err != nil || code != 0 {
		return fmt.Errorf("saving %s: %v %s", image, err, stderr)
	}
	results := make([]error, len(nodes))
	for index, name := range nodes {
		if err := ctx.Err(); err != nil {
			results[index] = err
			continue
		}
		if _, err := r.Node(name).Pipe(ctx, bytes.NewReader(saved), importArgs...); err != nil {
			results[index] = fmt.Errorf("importing %s into %s: %w", image, name, err)
		}
	}
	return errors.Join(results...)
}
