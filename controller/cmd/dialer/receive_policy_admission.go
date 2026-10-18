package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"reflect"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// The phase executor already holds the host lock. Shared rule maintenance has
// independent live-mesh authority; terminal phase verification stays read-only.
func reconcileReceivePolicy(ctx context.Context, api kubernetes.Interface, devices receiverDevices, cfg config, key wgtypes.Key, plan tunnel.ReceiveTransitionPlan, view handover.View, nodeUID string, intent tunnel.ReceiveJournalRoutes) error {
	if err := verifyReceivePolicyRules(cfg); err == nil {
		return nil
	}
	return admitReceivePolicyRepair(ctx, api, cfg, key, plan, view, nodeUID, intent, func(groups []receiveAdvertisementGroup, check func() error) error {
		return restoreReceivePolicyRules(ctx, devices, cfg, key, groups, check)
	})
}

func admitReceivePolicyRepair(ctx context.Context, api kubernetes.Interface, cfg config, key wgtypes.Key, plan tunnel.ReceiveTransitionPlan, view handover.View, nodeUID string, intent tunnel.ReceiveJournalRoutes, native func([]receiveAdvertisementGroup, func() error) error) error {
	if api == nil || native == nil {
		return fmt.Errorf("policy recovery requires live admission and native verification")
	}
	namespace, meshName := cfg.secretNamespace, cfg.secretName
	cloud := meshName == ""
	if cloud {
		namespace, meshName = cfg.peersSecretNamespace, cfg.receiptMeshName
	}
	mesh, err := api.CoreV1().Secrets(namespace).Get(ctx, meshName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if string(mesh.UID) != plan.MeshUID || mesh.ResourceVersion == "" || mesh.DeletionTimestamp != nil || mesh.Name != plan.MeshName || mesh.Namespace != plan.Namespace {
		return fmt.Errorf("policy recovery mesh differs from admitted journal")
	}
	if !cloud {
		role, _, err := tunnel.SiteForwardingRole(mesh.Data, cfg.nodeName, nil)
		if err != nil || role != "direct" {
			return fmt.Errorf("policy recovery requires a direct site projection: %v", err)
		}
	}
	store := handover.Store{ConfigMaps: api.CoreV1().ConfigMaps(plan.Namespace), ClusterUID: view.Round.ClusterUID, Name: tunnel.ReceiveTransitionName(plan.MachineUID)}
	scope := receiveJournalScope{Namespace: plan.Namespace, MeshName: plan.MeshName, MeshUID: plan.MeshUID, MachineUID: plan.MachineUID}
	set, err := loadReceiveSet(ctx, api, store, scope, cfg.nodeName, key)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(set.Routes, intent) || set.Local[plan.MachineUID].NodeUID != nodeUID {
		return fmt.Errorf("policy recovery route authority changed")
	}
	agent := func() (string, error) {
		pod, err := api.CoreV1().Pods(namespace).Get(ctx, cfg.receiptPodName, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		if cfg.receiptPodUID == "" || string(pod.UID) != cfg.receiptPodUID || pod.Spec.NodeName != cfg.nodeName || pod.DeletionTimestamp != nil {
			return "", fmt.Errorf("policy recovery agent identity differs")
		}
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name == "dialer" && status.Ready && status.State.Running != nil && status.ContainerID != "" {
				return status.ContainerID, nil
			}
		}
		return "", fmt.Errorf("policy recovery agent is not ready")
	}
	container, err := agent()
	if err != nil {
		return err
	}
	var adoption *corev1.Secret
	var groups []receiveAdvertisementGroup
	if cloud {
		adoption, err = api.CoreV1().Secrets(namespace).Get(ctx, plan.AdoptionName, metav1.GetOptions{})
		if err == nil {
			groups, err = planCloudReceivePolicy(cfg, key, plan, view, nodeUID, intent, adoption)
		}
	} else {
		projection := &siteForwardingCache{MeshUID: plan.MeshUID, NodeName: cfg.nodeName, PublicKey: key.PublicKey().String(), Data: mesh.Data}
		groups, err = planReceiveAdvertisements(cfg, projection, nodeUID, intent)
	}
	if err != nil {
		return err
	}
	check := func() error {
		cluster, err := api.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
		if err != nil {
			return err
		}
		if string(cluster.UID) != store.ClusterUID || cluster.DeletionTimestamp != nil {
			return fmt.Errorf("policy recovery cluster identity changed")
		}
		current, err := api.CoreV1().Secrets(mesh.Namespace).Get(ctx, mesh.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if !sameReceiveRevision(mesh.ObjectMeta, current.ObjectMeta) {
			return fmt.Errorf("policy recovery mesh publication changed")
		}
		if adoption != nil {
			currentAdoption, err := api.CoreV1().Secrets(namespace).Get(ctx, adoption.Name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			if !sameReceiveRevision(adoption.ObjectMeta, currentAdoption.ObjectMeta) {
				return fmt.Errorf("policy recovery adoption publication changed")
			}
		}
		currentSet, err := loadReceiveSet(ctx, api, store, scope, cfg.nodeName, key)
		if err != nil {
			return err
		}
		if !sameReceiveSet(set, currentSet) {
			return fmt.Errorf("policy recovery journal inventory changed")
		}
		currentContainer, err := agent()
		if err != nil {
			return err
		}
		if currentContainer != container {
			return fmt.Errorf("policy recovery agent container changed")
		}
		return ctx.Err()
	}
	if err := check(); err != nil {
		return err
	}
	return native(groups, check)
}

// Cloud policy recovery is terminal maintenance, not permission to advance a
// phase or apply a new peer generation. The published document must still be
// one of the journal's two exact documents; the journal selects the active one.
func planCloudReceivePolicy(cfg config, key wgtypes.Key, plan tunnel.ReceiveTransitionPlan, view handover.View, nodeUID string, intent tunnel.ReceiveJournalRoutes, adoption *corev1.Secret) ([]receiveAdvertisementGroup, error) {
	if adoption == nil || string(adoption.UID) != plan.AdoptionUID || adoption.Name != plan.AdoptionName || adoption.Namespace != plan.Namespace || adoption.ResourceVersion == "" || adoption.DeletionTimestamp != nil {
		return nil, fmt.Errorf("cloud policy recovery adoption identity differs")
	}
	var published, base tunnel.PeerListDoc
	if err := json.Unmarshal(plan.Base, &base); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(adoption.Data[tunnel.CloudPeersKey]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&published); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("trailing cloud policy publication data")
	}
	if !reflect.DeepEqual(published, base) && !reflect.DeepEqual(published, plan.Desired) {
		return nil, fmt.Errorf("cloud policy recovery publication differs from committed documents")
	}
	return planSelectedCloudReceivePolicy(cfg, key, plan, view, nodeUID, intent)
}

func planSelectedCloudReceivePolicy(cfg config, key wgtypes.Key, plan tunnel.ReceiveTransitionPlan, view handover.View, nodeUID string, intent tunnel.ReceiveJournalRoutes) ([]receiveAdvertisementGroup, error) {
	if key == (wgtypes.Key{}) || !view.Complete || nodeUID != plan.CloudNodeUID || cfg.nodeName != plan.CloudNodeName || key.PublicKey().String() != plan.RootPublicKey || !reflect.DeepEqual(intent.Plans[plan.MachineUID], plan) || !reflect.DeepEqual(intent.Views[plan.MachineUID], view) {
		return nil, fmt.Errorf("cloud policy recovery requires exact completed local intent")
	}
	var base tunnel.PeerListDoc
	if err := json.Unmarshal(plan.Base, &base); err != nil {
		return nil, err
	}
	if view.ServingGeneration != "legacy:"+tunnel.HashPeerList(plan.Base) && view.ServingGeneration != "receivers:"+view.Round.PlanHash {
		return nil, fmt.Errorf("cloud policy recovery generation differs")
	}
	selected := plan.Desired
	_, legacy := intent.Legacy[plan.MachineUID]
	if legacy != (view.ServingGeneration == "legacy:"+tunnel.HashPeerList(plan.Base)) {
		return nil, fmt.Errorf("cloud policy recovery legacy selection differs")
	}
	if legacy {
		if _, err := selectedReceiveLegacyPeers(key, nodeUID, intent); err != nil {
			return nil, err
		}
		selected = base
	}
	expected, err := tunnel.PlanReceiveRoutes(plan)
	if err != nil {
		return nil, err
	}
	if legacy {
		expected[nodeUID] = map[string]tunnel.ReceiveRouteSelection{}
	}
	if !reflect.DeepEqual(expected[nodeUID], intent.Receivers[nodeUID]) {
		return nil, fmt.Errorf("cloud policy recovery receiver selection differs")
	}
	owners := map[netip.Prefix]string{}
	var groups []receiveAdvertisementGroup
	for _, peer := range selected.Peers {
		withdrawn := false
		for _, retired := range intent.Withdrawn {
			if retired == peer.PublicKey {
				if !peer.Remote {
					return nil, fmt.Errorf("withdrawn cloud peer has site permissions")
				}
				withdrawn = true
			}
		}
		if withdrawn {
			continue
		}
		g := receiveAdvertisementGroup{peer: peer, config: cfg, legacy: legacy || peer.Remote}
		g.config.receiveRouteScope = map[string]bool{}
		g.config.receiveRoutes = map[string]receiveRouteSelection{}
		g.config.receiveRelayRoutes = map[string]string{}
		prefixes := map[string]bool{}
		for _, raw := range peer.WGAllowedIPs {
			prefixes[raw] = true
		}
		for _, host := range peer.AllRouteHosts() {
			prefixes[tunnel.HostCIDR(host)] = true
		}
		for raw := range prefixes {
			prefix, err := netip.ParsePrefix(raw)
			if err != nil || prefix != prefix.Masked() || prefix.Bits() == 0 || !prefix.Addr().IsGlobalUnicast() || prefix.Addr().Is4In6() {
				return nil, fmt.Errorf("invalid cloud policy destination")
			}
			for other, owner := range owners {
				if prefix.Overlaps(other) && owner != peer.PublicKey {
					return nil, fmt.Errorf("cloud policy destination has multiple owners")
				}
			}
			owners[prefix] = peer.PublicKey
			_, dst, _ := net.ParseCIDR(raw)
			g.destinations = append(g.destinations, *dst)
			g.config.receiveRouteScope[raw] = true
			if !g.legacy {
				selection, ok := intent.Receivers[nodeUID][raw]
				if !ok {
					return nil, fmt.Errorf("cloud policy destination lacks selected receiver")
				}
				g.config.receiveRoutes[raw] = selection
			}
		}
		groups = append(groups, g)
	}
	return groups, nil
}
