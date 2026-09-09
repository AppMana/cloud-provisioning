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

// WaitForRemoteReach waits for the mesh to carry whatever this
// network needs for a remote, and describes what it found.
//
// The two networks want opposite things, and this is the one place
// that has to know it. On a native network the tunnel sees packets
// addressed to pods, so a remote's accept list has to carry the
// blocks its node owns; until it does, the nodes at home have the
// remote's addresses and no way to reach anything running on it — a
// state in which every component reports healthy. On an encapsulating
// network the packets are addressed to nodes, so the list carries
// host routes and a pod block on it would be wrong: the mesh would
// accept pod-addressed packets that never arrive.
//
// Waiting for blocks either way is what this used to do, and the
// first encapsulating row failed for the network doing exactly the
// right thing.
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
