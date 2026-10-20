package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Ordinary workers retain the existing applied-document contract. Never elect
// a new relay from the mesh while the cloud still accepts a different owner.
// Reservations and completed withdrawals exclude their roots from this path.
func admitReceiveFollowerOrdinary(ctx context.Context, api kubernetes.Interface, cfg config, admitted receiveObserverAdmission, peers []tunnel.PeerSpec, plans []tunnel.ReceiveTransitionPlan) (map[string]map[string]string, bool, func() error, error) {
	fail := func(err error) (map[string]map[string]string, bool, func() error, error) { return nil, false, nil, err }
	ordinary, err := tunnel.PlanReceiveNeighborPeers(peers, plans)
	if err != nil {
		return fail(err)
	}
	exclusions := tunnel.ReceiveJournalRoutes{Plans: map[string]tunnel.ReceiveTransitionPlan{}}
	for _, plan := range plans {
		exclusions.Plans[plan.MachineUID] = plan
	}
	if err := appendReleasedReceiveExclusions(admitted.Inventory, &exclusions); err != nil {
		return fail(err)
	}
	desired := map[string]map[string]string{}
	pending := false
	observed := map[string]*corev1.Secret{}
	for _, peer := range ordinary {
		for _, root := range exclusions.Withdrawn {
			if peer.PublicKey == root {
				return fail(fmt.Errorf("withdrawn worker reappeared as an ordinary follower destination"))
			}
		}
		machine := ""
		for name, public := range admitted.Mesh.Data {
			if strings.HasPrefix(name, tunnel.PeerPublicKeyPrefix) && string(public) == peer.PublicKey {
				if machine != "" {
					return fail(fmt.Errorf("ordinary follower worker identity is ambiguous"))
				}
				machine = strings.TrimPrefix(name, tunnel.PeerPublicKeyPrefix)
			}
		}
		if machine == "" {
			return fail(fmt.Errorf("ordinary follower worker publication is missing"))
		}
		name := tunnel.AdoptionSecretName(machine)
		adoption, err := api.CoreV1().Secrets(admitted.Mesh.Namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			observed[name] = nil
			pending = true
			continue
		}
		if err != nil {
			return fail(err)
		}
		if adoption.UID == "" || adoption.ResourceVersion == "" || adoption.DeletionTimestamp != nil {
			return fail(fmt.Errorf("ordinary follower adoption identity is unavailable"))
		}
		observed[name] = adoption
		raw := adoption.Data[tunnel.CloudPeersKey]
		if len(raw) == 0 || adoption.Annotations[tunnel.AppliedListAnnotation] != tunnel.HashPeerList(raw) {
			pending = true
			continue
		}
		var doc tunnel.PeerListDoc
		if err := json.Unmarshal(raw, &doc); err != nil {
			return fail(err)
		}
		path, err := tunnel.PlanReceiveFollowerPath(admitted.Mesh.Data, cfg.nodeName, doc, peer)
		if err != nil {
			return fail(err)
		}
		routes, err := receiveFollowerTransitRoutes(path.Transit)
		if err != nil {
			return fail(err)
		}
		desired[peer.PublicKey] = routes
	}
	check := func() error {
		for name, before := range observed {
			current, err := api.CoreV1().Secrets(admitted.Mesh.Namespace).Get(ctx, name, metav1.GetOptions{})
			if before == nil && apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return err
			}
			if before == nil || !sameReceiveRevision(before.ObjectMeta, current.ObjectMeta) {
				return fmt.Errorf("ordinary follower adoption changed during execution")
			}
		}
		return ctx.Err()
	}
	return desired, pending, check, nil
}
