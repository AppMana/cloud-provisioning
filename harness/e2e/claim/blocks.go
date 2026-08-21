package claim

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
)

// WaitForPodBlocks waits for the mesh to carry a remote's own pod
// blocks on its peer entry, and returns them.
//
// This is the product's own evidence that it resolved the machine to
// a node: the blocks are allocated after the node joins, so the
// controller has to come back for them, find which node the machine
// turned out to be, and publish what that node owns. Until they are
// there, the nodes at home have the remote's addresses and no way to
// reach anything running on it — a state in which every component
// reports healthy.
//
// The tunnel address and the node address are always on the entry.
// What this waits for is something wider than a host route, which is
// a pod block and nothing else.
func WaitForPodBlocks(ctx context.Context, k *kube.Client, machine string, within time.Duration) (string, error) {
	deadline := time.Now().Add(within)
	for {
		encoded, err := k.Get(ctx, Namespace, "secret", Namespace+"-peers",
			"{.data.peer-allowed-ips-"+machine+"}")
		if err == nil && encoded != "" {
			decoded, err := base64.StdEncoding.DecodeString(encoded)
			if err == nil {
				if blocks := wider(string(decoded)); blocks != "" {
					return blocks, nil
				}
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf(
				"%s's peer entry never carried a pod block, so nothing at the site can reach a pod on it", machine)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// wider returns the entries that are not host routes.
func wider(list string) string {
	var out []string
	for _, entry := range strings.Split(list, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" || strings.HasSuffix(entry, "/32") || strings.HasSuffix(entry, "/128") {
			continue
		}
		out = append(out, entry)
	}
	return strings.Join(out, ",")
}
