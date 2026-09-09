package main

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// pruneDeletedMachines repairs both deletion events and orphaned state found
// after a controller restart. A failed or cached Machine read must never revoke
// a live node's peer. The caller patches the Secret with optimistic locking.
func (r *meshReconciler) pruneDeletedMachines(ctx context.Context, secret *corev1.Secret) (bool, error) {
	machines := &unstructured.UnstructuredList{}
	machines.SetGroupVersionKind(machineGVK.GroupVersion().WithKind("MachineList"))
	if err := r.reader.List(ctx, machines); err != nil {
		return false, fmt.Errorf("checking machines before peer cleanup: %w", err)
	}
	live := map[string]bool{}
	for _, machine := range machines.Items {
		live[machine.GetName()] = true
	}
	prefixes := []string{tunnel.PeerPublicKeyPrefix, tunnel.PeerEndpointPrefix, tunnel.PeerAllowedIPsPrefix, tunnel.PeerRouteHostsPrefix, tunnel.PeerRouteHostPrefix}
	changed := false
	_, subnet, _ := net.ParseCIDR(r.tunnelSubnet)
	for _, name := range publishedNames(secret.Data, prefixes...) {
		if live[name] {
			continue
		}
		adoption := &corev1.Secret{}
		key := types.NamespacedName{Namespace: r.secretNamespace, Name: tunnel.AdoptionSecretName(name)}
		if err := r.reader.Get(ctx, key, adoption); err == nil {
			if err := r.Delete(ctx, adoption, client.Preconditions{UID: &adoption.UID, ResourceVersion: &adoption.ResourceVersion}); err != nil && !apierrors.IsNotFound(err) {
				return false, err
			}
		} else if !apierrors.IsNotFound(err) {
			return false, err
		}
		for _, addr := range tunnel.SplitList(string(secret.Data[tunnel.PeerRouteHostsPrefix+name]), string(secret.Data[tunnel.PeerRouteHostPrefix+name])) {
			ip := net.ParseIP(strings.SplitN(addr, "/", 2)[0])
			if subnet != nil && ip != nil && subnet.Contains(ip) {
				retireTunnelAddress(secret.Data, ip.String())
			}
		}
		for _, prefix := range prefixes {
			delete(secret.Data, prefix+name)
		}
		changed = true
	}
	return changed, nil
}
