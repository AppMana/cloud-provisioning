package claim

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/cni"
	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
)

// WaitForRemoteReach waits for the address permissions required by the
// network: pod allocations for native routing, node addresses for an
// encapsulating network. Requiring pod blocks for both would reject valid
// encapsulated connectivity.
func WaitForRemoteReach(ctx context.Context, k *kube.Client, machine string,
	encapsulation cni.Encapsulation, within time.Duration) (string, error) {

	deadline := time.Now().Add(within)
	for {
		entry, ok := peerEntry(ctx, k, machine)
		if ok {
			blocks := wider(entry)
			switch encapsulation {
			case cni.Native:
				if blocks != "" {
					return "pod blocks: " + blocks, nil
				}
			case cni.Encapsulated:
				if blocks != "" {
					return "", fmt.Errorf(
						"%s's peer entry carries %s on a network that encapsulates: "+
							"the mesh would accept pod-addressed packets that never arrive",
						machine, blocks)
				}
				if hosts(entry) != "" {
					return "node addresses: " + hosts(entry), nil
				}
			default:
				return "", fmt.Errorf(
					"the network's encapsulation is unknown, so what %s's peer entry should "+
						"carry cannot be decided", machine)
			}
		}
		if time.Now().After(deadline) {
			if encapsulation == cni.Encapsulated {
				return "", fmt.Errorf(
					"%s's peer entry never carried its node's address, so nothing at the site "+
						"can reach it", machine)
			}
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

// peerEntry reads a machine's accept list.
func peerEntry(ctx context.Context, k *kube.Client, machine string) (string, bool) {
	encoded, err := k.Get(ctx, Namespace, "secret", Namespace+"-peers",
		"{.data.peer-allowed-ips-"+machine+"}")
	if err != nil || encoded == "" {
		return "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", false
	}
	return string(decoded), true
}

// hosts returns the entries that are host routes.
func hosts(list string) string {
	var out []string
	for _, entry := range strings.Split(list, ",") {
		entry = strings.TrimSpace(entry)
		if strings.HasSuffix(entry, "/32") || strings.HasSuffix(entry, "/128") {
			out = append(out, entry)
		}
	}
	return strings.Join(out, ",")
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
