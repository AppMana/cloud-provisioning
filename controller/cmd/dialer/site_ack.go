package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// Called only after successful kernel application. Re-read before the CAS patch
// so an old render cannot acknowledge changed content or a replacement Node.
func acknowledgeSite(ctx context.Context, k kubernetes.Interface, namespace, name, nodeName, nodeUID, publicKey, secretUID, hash string, appliedTransit ...*tunnel.TransitSpec) error {
	node, err := k.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if string(node.UID) != nodeUID || node.DeletionTimestamp != nil {
		return fmt.Errorf("site Node identity changed")
	}
	secret, err := k.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if string(secret.UID) != secretUID || string(secret.Data[tunnel.NodePublicKeyPrefix+nodeName]) != publicKey {
		return fmt.Errorf("site Secret or key identity changed")
	}
	current, err := tunnel.SitePeerHash(secret.Data)
	if err != nil {
		return err
	}
	if current != hash {
		return fmt.Errorf("site peer render changed before acknowledgement")
	}
	receipt := tunnel.SiteApplied{NodeUID: nodeUID, PublicKey: publicKey, Hash: hash, Role: "direct"}
	nodes, err := k.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	dead := map[string]bool{}
	for _, n := range nodes.Items {
		ready := false
		for _, condition := range n.Status.Conditions {
			if condition.Type == "Ready" && condition.Status == "True" {
				ready = true
			}
		}
		dead[n.Name] = !ready
	}
	role, expectedTransit, err := tunnel.SiteForwardingRole(secret.Data, nodeName, dead)
	if err != nil {
		return err
	}
	if len(appliedTransit) > 0 && appliedTransit[0] != nil {
		if role != "relayed" && role != "transit" {
			return fmt.Errorf("site forwarding role changed")
		}
		actual, err := tunnel.SiteTransitHash(secret.Data, appliedTransit[0])
		if err != nil {
			return err
		}
		expected, err := tunnel.SiteTransitHash(secret.Data, expectedTransit)
		if err != nil {
			return err
		}
		if actual != expected {
			return fmt.Errorf("site transit changed before acknowledgement")
		}
		receipt.Role = role
		receipt.TransitHash = actual
	} else if role != "direct" {
		return fmt.Errorf("direct receipt cannot acknowledge relayed forwarding")
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	key := tunnel.SiteAppliedPrefix + nodeName
	if string(secret.Data[key]) == string(raw) {
		return nil
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"uid": secret.UID, "resourceVersion": secret.ResourceVersion}, "data": map[string][]byte{key: raw}})
	if err != nil {
		return err
	}
	_, err = k.CoreV1().Secrets(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}
