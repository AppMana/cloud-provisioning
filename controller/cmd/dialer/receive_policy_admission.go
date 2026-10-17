package main

import (
	"context"
	"fmt"
	"reflect"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
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
	if cfg.secretName == "" {
		return fmt.Errorf("cloud policy recovery requires its own live projection")
	}
	mesh, err := api.CoreV1().Secrets(cfg.secretNamespace).Get(ctx, cfg.secretName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if string(mesh.UID) != plan.MeshUID || mesh.ResourceVersion == "" || mesh.DeletionTimestamp != nil || mesh.Name != plan.MeshName || mesh.Namespace != plan.Namespace {
		return fmt.Errorf("policy recovery mesh differs from admitted journal")
	}
	role, _, err := tunnel.SiteForwardingRole(mesh.Data, cfg.nodeName, nil)
	if err != nil || role != "direct" {
		return fmt.Errorf("policy recovery requires a direct site projection: %v", err)
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
		pod, err := api.CoreV1().Pods(cfg.secretNamespace).Get(ctx, cfg.receiptPodName, metav1.GetOptions{})
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
	projection := &siteForwardingCache{MeshUID: plan.MeshUID, NodeName: cfg.nodeName, PublicKey: key.PublicKey().String(), Data: mesh.Data}
	groups, err := planReceiveAdvertisements(cfg, projection, nodeUID, intent)
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
