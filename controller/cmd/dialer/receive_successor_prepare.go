package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Caller holds the completed-maintenance host lock and restores serving state
// first. Preparation is additive: it cannot switch routes, retire devices or
// replace the journal. Revalidate the complete admitted set before native work
// and readiness; cross-object API reads do not make publication atomic.
func admitReceiveSuccessorPreparation(ctx context.Context, api kubernetes.Interface, cfg config, key wgtypes.Key, plan tunnel.ReceiveTransitionPlan, view handover.View, nodeUID string, intent tunnel.ReceiveJournalRoutes) (*corev1.Secret, tunnel.ReceivePrepare, func() error, error) {
	fail := func(err error) (*corev1.Secret, tunnel.ReceivePrepare, func() error, error) {
		return nil, tunnel.ReceivePrepare{}, nil, err
	}
	if !view.Complete {
		return fail(fmt.Errorf("successor preparation requires completed maintenance"))
	}
	secret, err := api.CoreV1().Secrets(plan.Namespace).Get(ctx, plan.AdoptionName, metav1.GetOptions{})
	if err != nil {
		return fail(err)
	}
	if string(secret.UID) != plan.AdoptionUID || secret.DeletionTimestamp != nil {
		return fail(fmt.Errorf("successor adoption identity changed"))
	}
	raw := secret.Data[tunnel.ReceivePrepareKey]
	if len(raw) == 0 {
		return nil, tunnel.ReceivePrepare{}, nil, nil
	}
	var request tunnel.ReceivePrepare
	if err := json.Unmarshal(raw, &request); err != nil {
		return fail(fmt.Errorf("invalid successor preparation request"))
	}
	// The last request remains in the Secret after activation or rollback. It
	// must never recreate a retired candidate during completed maintenance.
	if request.Predecessor == nil || request.ReceiverGeneration == plan.ReceiverGeneration {
		return nil, tunnel.ReceivePrepare{}, nil, nil
	}
	scope := receiveJournalScope{Namespace: plan.Namespace, MeshName: plan.MeshName, MeshUID: plan.MeshUID, MachineUID: plan.MachineUID}
	store := handover.Store{ConfigMaps: api.CoreV1().ConfigMaps(plan.Namespace), Name: tunnel.ReceiveTransitionName(plan.MachineUID), ClusterUID: view.Round.ClusterUID}
	admitted, err := loadReceiveSet(ctx, api, store, scope, cfg.nodeName, key)
	if err != nil {
		return fail(err)
	}
	local := admitted.Local[plan.MachineUID]
	currentView, err := local.Snapshot.Transition().View()
	if err != nil {
		return fail(err)
	}
	if local.NodeUID != nodeUID || !reflect.DeepEqual(local.Plan, plan) || !reflect.DeepEqual(currentView, view) || !reflect.DeepEqual(admitted.Routes, intent) {
		return fail(fmt.Errorf("successor preparation differs from completed maintenance authority"))
	}
	if _, err := request.ValidatePredecessor(local.Snapshot); err != nil {
		return fail(err)
	}
	check := func() error {
		fresh, err := loadReceiveSet(ctx, api, store, scope, cfg.nodeName, key)
		if err != nil {
			return err
		}
		if !sameReceiveSet(admitted, fresh) {
			return fmt.Errorf("receiver set changed during successor preparation")
		}
		if _, err := request.ValidatePredecessor(fresh.Local[plan.MachineUID].Snapshot); err != nil {
			return err
		}
		latest, err := api.CoreV1().Secrets(plan.Namespace).Get(ctx, plan.AdoptionName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if latest.UID != secret.UID || latest.DeletionTimestamp != nil || !bytes.Equal(latest.Data[tunnel.ReceivePrepareKey], raw) || !bytes.Equal(latest.Data[tunnel.CloudPeersKey], secret.Data[tunnel.CloudPeersKey]) {
			return fmt.Errorf("successor proposal changed")
		}
		return ctx.Err()
	}
	if err := check(); err != nil {
		return fail(err)
	}
	return secret, request, check, nil
}

func reconcileReceiveSuccessorPreparation(ctx context.Context, api kubernetes.Interface, devices receiverDevices, cfg config, key wgtypes.Key, plan tunnel.ReceiveTransitionPlan, view handover.View, nodeUID string, intent tunnel.ReceiveJournalRoutes) error {
	secret, request, check, err := admitReceiveSuccessorPreparation(ctx, api, cfg, key, plan, view, nodeUID, intent)
	if err != nil || secret == nil {
		return err
	}
	mtu := cfg.mtu
	if mtu == 0 {
		mtu = underlayMTU()
	}
	if nodeUID == plan.CloudNodeUID {
		return reconcileReceivePreparationScoped(ctx, api, secret, cfg.nodeName, key, func(r tunnel.ReceivePrepare, previous tunnel.PeerListDoc) ([]receiveBinding, error) {
			scope, err := r.ReceiveScope()
			if err != nil {
				return nil, err
			}
			bindings, err := prepareReceivePaths(devices, scope, key, previous, r.Desired, r.Native, mtu, cfg.fwmark, r.Ports)
			if err != nil {
				return nil, err
			}
			if err := cacheReceivePreparation(cfg, key, secret.Data[tunnel.CloudPeersKey], r); err != nil {
				return nil, err
			}
			return bindings, nil
		}, check)
	}
	mesh, err := api.CoreV1().Secrets(plan.Namespace).Get(ctx, plan.MeshName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if string(mesh.UID) != plan.MeshUID || mesh.Namespace != cfg.secretNamespace || mesh.Name != cfg.secretName {
		return fmt.Errorf("successor site mesh identity changed")
	}
	path := siteReceiveCachePath(cfg)
	if path == "" {
		return fmt.Errorf("successor preparation requires durable host storage")
	}
	scope := &siteReceiveScope{roots: map[string]bool{plan.RootPublicKey: true}, check: check}
	return reconcileSiteReceivePreparationScoped(ctx, api, mesh, cfg.nodeName, key, scope, func(root string, binding receiveBinding, previous, desired tunnel.PeerListDoc, generation string) error {
		if root != plan.RootPublicKey || generation != request.ReceiverGeneration {
			return fmt.Errorf("successor site proposal differs from admitted request")
		}
		ownership := siteReceiveCache{Version: 1, Namespace: mesh.Namespace, SecretName: mesh.Name, MeshUID: string(mesh.UID), NodeName: cfg.nodeName, PublicKey: key.PublicKey().String(), Peers: desired, Bindings: map[string]receiveBinding{root: binding}, Generations: map[string]string{root: generation}, Previous: map[string]tunnel.PeerListDoc{root: previous}}
		// Persist additive ownership without replacing replay state or retiring any
		// prior record. Recovery still requires live journal and inventory admission.
		if err := preserveSiteReceiveOwnership(path+".admitted", ownership, cfg, key); err != nil {
			return err
		}
		receiveScope, err := request.ReceiveScope()
		if err != nil {
			return err
		}
		_, err = prepareSiteReceiveClient(devices, receiveScope, key, root, binding, previous, desired, mtu, cfg.fwmark)
		return err
	}, func(client tunnel.SiteReceiveClient) (time.Time, error) {
		return siteReceiveHandshake(devices, key, client, cfg.fwmark, time.Now())
	})
}
