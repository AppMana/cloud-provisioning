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
	// Imported from a file carried into the guest, not from stdin: a guest
	// agent message carries at most ~47 MiB of stdin, and image archives
	// are routinely larger. File transfer is chunked.
	args := append([]string(nil), importArgs...)
	if len(args) == 0 || args[len(args)-1] != "-" {
		return fmt.Errorf("image import command %v does not read its archive from \"-\"", importArgs)
	}
	args[len(args)-1] = GuestImageArchive
	results := make([]error, len(nodes))
	for index, name := range nodes {
		if err := ctx.Err(); err != nil {
			results[index] = err
			continue
		}
		node := r.Node(name)
		if err := node.Put(ctx, bytes.NewReader(saved), GuestImageArchive, 0o600); err != nil {
			results[index] = fmt.Errorf("carrying %s into %s: %w", image, name, err)
			continue
		}
		_, err := node.Exec(ctx, args...)
		if _, rmErr := node.Exec(ctx, "rm", "-f", GuestImageArchive); rmErr != nil && err == nil {
			err = rmErr
		}
		if err != nil {
			results[index] = fmt.Errorf("importing %s into %s: %w", image, name, err)
		}
	}
	return errors.Join(results...)
}

// GuestImageArchive is where an image archive is carried before import.
const GuestImageArchive = "/var/tmp/cldt-image-import.tar"
