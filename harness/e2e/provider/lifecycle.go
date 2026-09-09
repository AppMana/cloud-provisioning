package provider

import (
	"context"
	"encoding/base64"
	"fmt"
	"slices"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

const instanceFinalizer = "lab.cloud-provisioning.appmana.com/instance"
const instanceAnnotation = "lab.cloud-provisioning.appmana.com/bootstrapped-uid"

// provision consumes the bootstrap reference CAPI carries. The infrastructure
// provider owns launching and terminating the VM; the claim driver never edits
// product status or supplies a join command.
func (c *Controller) provision(ctx context.Context, namespace, name, node, uid string, annotations map[string]string, finalizers []string, deleting bool) (bool, error) {
	if uid == "" {
		return false, fmt.Errorf("infrastructure machine has no UID")
	}
	if deleting {
		if !slices.Contains(finalizers, instanceFinalizer) {
			return false, nil
		}
		stop := func() error { return c.Rig.Node(node).Kill(ctx) }
		if c.Slots != nil {
			if err := c.Slots.Stop(ctx, node, SlotOwner{Namespace: namespace, Name: name, UID: uid}, stop); err != nil {
				return false, err
			}
		} else if err := stop(); err != nil {
			return false, err
		}
		kept := slices.DeleteFunc(finalizers, func(f string) bool { return f == instanceFinalizer })
		return false, c.patchMetadata(ctx, namespace, name, map[string]any{"finalizers": kept})
	}
	if !slices.Contains(finalizers, instanceFinalizer) {
		if err := c.patchMetadata(ctx, namespace, name, map[string]any{"finalizers": append(finalizers, instanceFinalizer)}); err != nil {
			return false, err
		}
	}
	if annotations[instanceAnnotation] == uid {
		return true, nil
	}
	secret, err := c.Kube.Get(ctx, namespace, "machine", name, "{.spec.bootstrap.dataSecretName}")
	if err != nil {
		return false, err
	}
	if secret == "" {
		return false, nil
	}
	raw, err := c.Kube.Run(ctx, "-n", namespace, "get", "secret", secret, "--ignore-not-found", "-o", "jsonpath={.data.value}")
	encoded := string(raw)
	if err != nil {
		return false, err
	}
	if encoded == "" {
		return false, nil
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return false, fmt.Errorf("bootstrap secret %s: %w", secret, err)
	}
	format, err := c.Kube.Get(ctx, namespace, "secret", secret, "{.data.format}")
	if err != nil {
		return false, err
	}
	decodedFormat, err := base64.StdEncoding.DecodeString(format)
	if err != nil {
		return false, fmt.Errorf("invalid bootstrap format encoding")
	}
	if err := rig.BootstrapInstance(ctx, c.Rig.Node(node), uid, rig.BootstrapData{Format: string(decodedFormat), Value: data}); err != nil {
		return false, err
	}
	return true, c.patchMetadata(ctx, namespace, name, map[string]any{"annotations": map[string]string{instanceAnnotation: uid}})
}

// WaitForAddress reserves an address before bootstrap can be rendered. Ready
// stays false until the provider has actually launched the instance.
func (c *Controller) WaitForAddress(ctx context.Context, namespace, name string) error {
	for {
		observed, err := c.Reconcile(ctx, namespace)
		if err != nil {
			return err
		}
		for _, m := range observed {
			if m.Name == name && m.Address != "" {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}
