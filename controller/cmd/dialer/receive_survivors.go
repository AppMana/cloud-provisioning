package main

import (
	"context"
	"fmt"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Admission keeps withdrawn ownership and histories while exposing only serving
// routes to phase maintenance. Every withdrawal must be published and the local
// node must remain a captured consumer; absence is never cleanup completion.
func loadSurvivingReceiveSet(ctx context.Context, api kubernetes.Interface, store handover.Store, scope receiveJournalScope, nodeName string, key wgtypes.Key, inventory tunnel.ReceiveInventory, metadata metav1.ObjectMeta) (admittedReceiveSet, error) {
	fail := func(err error) (admittedReceiveSet, error) { return admittedReceiveSet{}, err }
	result := admittedReceiveSet{Inventory: inventory, Metadata: metadata, Local: map[string]admittedReceiveJournal{}, Retirements: map[string]admittedReceiveRetirement{}}
	var shared *admittedReceiveRetirement
	for _, uid := range inventory.MachineUIDs {
		if len(inventory.Publications[uid].Retirement) == 0 {
			continue
		}
		memberStore, memberScope := store, scope
		memberStore.Name, memberStore.ExpectedUID, memberScope.MachineUID = tunnel.ReceiveTransitionName(uid), inventory.Publications[uid].JournalUID, uid
		var retired admittedReceiveRetirement
		if shared == nil {
			var err error
			retired, err = loadReceiveRetirement(ctx, api, memberStore, memberScope, nodeName, key)
			if err != nil {
				return fail(err)
			}
			shared = &retired
		} else {
			// The first admission authenticated every reserved history, ownership
			// and route. Bind each sibling withdrawal to that same snapshot;
			// its consumer and live-source recheck remains mandatory below.
			var err error
			retired, err = receiveRetirementMember(memberStore, memberScope, *shared)
			if err != nil {
				return fail(err)
			}
		}
		if !sameReceiveRevision(metadata, retired.Metadata) {
			return fail(fmt.Errorf("survivor inventory changed during withdrawal admission"))
		}
		result.Retirements[uid] = retired
		if result.Journals == nil {
			result.Journals = retired.Journals
			result.ReleasedJournals = retired.ReleasedJournals
			result.Routes = retired.Routes.Serving
		}
	}
	for _, uid := range inventory.MachineUIDs {
		if _, retiring := result.Retirements[uid]; retiring {
			continue
		}
		snapshot, found := result.Journals[uid]
		if !found {
			return fail(fmt.Errorf("survivor journal absent from retained inventory"))
		}
		memberStore, memberScope := store, scope
		memberStore.Name, memberScope.MachineUID = tunnel.ReceiveTransitionName(uid), uid
		if uid == scope.MachineUID && store.ExpectedUID != "" && store.ExpectedUID != string(snapshot.Metadata().UID) {
			return fail(fmt.Errorf("survivor target journal incarnation changed"))
		}
		memberStore.ExpectedUID = string(snapshot.Metadata().UID)
		plan := result.Routes.Plans[uid]
		local := plan.CloudNodeName == nodeName || plan.RootPublicKey == key.PublicKey().String()
		for public, site := range plan.Sites {
			local = local || site.NodeName == nodeName || public == key.PublicKey().String()
		}
		if !local {
			continue
		}
		admitted, err := loadReceiveJournal(ctx, api, memberStore, memberScope, nodeName, key)
		if err != nil {
			return fail(err)
		}
		if err := admitted.Snapshot.ContinuesExecution(snapshot); err != nil {
			return fail(err)
		}
		result.Local[uid] = admitted
	}
	if _, found := result.Local[scope.MachineUID]; !found {
		return fail(fmt.Errorf("survivor target is not a local serving journal"))
	}
	for uid, retired := range result.Retirements {
		memberStore := store
		memberStore.Name, memberStore.ExpectedUID = tunnel.ReceiveTransitionName(uid), string(retired.Journals[uid].Metadata().UID)
		// History was authenticated above; fence current sources without loading
		// that immutable ancestry again at the final admission boundary.
		if err := recheckReceiveRetirement(ctx, api, memberStore, retired, nodeName, key); err != nil {
			return fail(err)
		}
	}
	if err := appendReleasedReceiveExclusions(inventory, &result.Routes); err != nil {
		return fail(err)
	}
	return result, nil
}
