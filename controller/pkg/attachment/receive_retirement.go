package attachment

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
)

// withdrawalIntentHash uses the same persisted intent fields at freeze, publication
// and historical release admission. It binds authority, not native completion.
func (w RemoteWithdrawalIntent) withdrawalIntentHash() (string, error) {
	raw, err := json.Marshal(w)
	if err != nil {
		return "", err
	}
	return tunnel.HashPeerList(raw), nil
}

func (w RemoteWithdrawalIntent) validateJournalWithdrawalFence(snapshot handover.Snapshot) error {
	if !snapshot.RetirementFrozen() {
		return fmt.Errorf("journal lacks a retirement write fence")
	}
	if bound := snapshot.WithdrawalIntentHash(); bound != "" {
		digest, err := w.withdrawalIntentHash()
		if err != nil {
			return err
		}
		if digest != bound {
			return fmt.Errorf("interrupted retirement lifecycle intent changed")
		}
	}
	return nil
}

// ReceiveRetirementPlan binds a previously persisted withdrawal intent to a
// frozen, still-reserved journal and a published withdrawal. It does not infer
// cleanup from Machine or Node absence. Every recorded receiver participant
// remains required even when unavailable, and new published consumers require
// an explicit extension of the persisted intent.
//
// This is input validation, not native execution authority or acknowledgement.
// Callers must load these objects live, authenticate journal ancestry, verify
// the persisted intent has not changed, and recheck under the native lock.
// Prepared devices outside the journal still require durable ownership records.
// Keep the reservation until all required native retirement receipts are durable.
func (w RemoteWithdrawalIntent) ReceiveRetirementPlan(snapshot handover.Snapshot, inventory tunnel.ReceiveInventory, mesh *corev1.Secret) (tunnel.ReceiveTransitionPlan, error) {
	return w.receiveRetirementPlan(snapshot, inventory, mesh, true)
}

func (w RemoteWithdrawalIntent) receiveRetirementPlan(snapshot handover.Snapshot, inventory tunnel.ReceiveInventory, mesh *corev1.Secret, withdrawn bool) (tunnel.ReceiveTransitionPlan, error) {
	fail := func(message string) (tunnel.ReceiveTransitionPlan, error) {
		return tunnel.ReceiveTransitionPlan{}, fmt.Errorf("receive retirement: %s", message)
	}
	if mesh == nil || mesh.UID == "" || mesh.ResourceVersion == "" || mesh.DeletionTimestamp != nil ||
		w.SourceVersion == "" || w.DrainIntent == "" || w.MachineName == "" || w.MachineUID == "" || w.PublicKey == "" ||
		w.MeshNamespace != mesh.Namespace || w.MeshName != mesh.Name || w.MeshUID != string(mesh.UID) {
		return fail("withdrawal requires exact live mesh and persisted drain identity")
	}
	if w.Orphaned && (w.DrainIntent != "orphan:"+string(snapshot.Metadata().UID) || w.SourceDataHash == "" || w.SourceDataHash != withdrawalSourceDataHash(mesh.Data)) {
		return fail("orphan recovery requires its exact captured mesh and journal identity")
	}
	// Orphan capture explicitly records a publication that was already absent.
	// It cannot use an unrelated later metadata revision as removal evidence;
	// retain the full captured data hash instead, including acknowledgement data.
	if withdrawn && !w.Orphaned && w.SourceVersion == mesh.ResourceVersion {
		return fail("mesh version differs from withdrawal stage")
	}
	if !withdrawn {
		if err := w.validateCapturedWithdrawalSource(inventory, mesh); err != nil {
			return fail(err.Error())
		}
	}

	if inventory.Version != 1 || inventory.ClusterUID == "" || inventory.Namespace != w.MeshNamespace || inventory.MeshName != w.MeshName || inventory.MeshUID != w.MeshUID {
		return fail("inventory scope differs from withdrawal")
	}
	meta := snapshot.Metadata()
	if !snapshot.Frozen() || meta.UID == "" || meta.ResourceVersion == "" || meta.DeletionTimestamp != nil {
		return fail("journal must be live and frozen")
	}
	// Legacy reservations without a published UID cannot bind this destructive
	// operation to an API incarnation, even though ordinary readers accept them.
	publication := inventory.Publications[w.MachineUID]
	if publication.JournalUID != string(meta.UID) || len(publication.Plan) != 0 {
		return fail("journal incarnation is not explicitly reserved")
	}
	if err := inventory.CheckJournal(w.MachineUID, string(meta.UID)); err != nil {
		return fail(err.Error())
	}
	p, err := tunnel.DecodeReceiveTransition(snapshot, inventory.ClusterUID)
	if err != nil {
		return fail(err.Error())
	}
	if err := w.validateJournalWithdrawalFence(snapshot); err != nil {
		return fail(err.Error())
	}
	if p.Namespace != w.MeshNamespace || p.MeshName != w.MeshName || p.MeshUID != w.MeshUID || p.MachineName != w.MachineName || p.MachineUID != w.MachineUID || p.RootPublicKey != w.PublicKey {
		return fail("withdrawal differs from journal ownership")
	}
	if _, present, err := tunnel.WithdrawRemotePeer(mesh.Data, w.MachineName, w.PublicKey); err != nil || (w.Orphaned && present) || (!w.Orphaned && present == withdrawn) {
		return fail("peer publication differs from withdrawal stage")
	}
	sites, remotes, err := w.receiveWithdrawalConsumers(p)
	if err != nil {
		return tunnel.ReceiveTransitionPlan{}, err
	}
	for field, value := range mesh.Data {
		if !withdrawn && field == tunnel.PeerPublicKeyPrefix+w.MachineName {
			continue
		}
		for _, kind := range []struct {
			prefix  string
			targets map[string]ConsumerTarget
			key     bool
		}{
			{tunnel.NodePublicKeyPrefix, sites, true},
			{tunnel.SiteAddressesPrefix, sites, false},
			{tunnel.PeerPublicKeyPrefix, remotes, true},
		} {
			if !strings.HasPrefix(field, kind.prefix) {
				continue
			}
			c, found := kind.targets[strings.TrimPrefix(field, kind.prefix)]
			if !found || (kind.key && string(value) != c.PublicKey) {
				return fail("published consumer membership changed")
			}
		}
	}
	return p, nil
}

// Validate captured identities independently of current mesh membership. Active
// retirement additionally validates every published consumer against these maps.
func (w RemoteWithdrawalIntent) receiveWithdrawalConsumers(p tunnel.ReceiveTransitionPlan) (map[string]ConsumerTarget, map[string]ConsumerTarget, error) {
	names, uids, keys := map[string]bool{}, map[string]bool{}, map[string]bool{}
	sites, remotes := map[string]ConsumerTarget{}, map[string]ConsumerTarget{}
	for _, c := range w.Consumers {
		if c.NodeName == "" || c.NodeUID == "" || (!c.Site && c.PublicKey == "") || names[c.NodeName] || uids[c.NodeUID] || (c.PublicKey != "" && keys[c.PublicKey]) ||
			c.NodeName == p.CloudNodeName || c.NodeUID == p.CloudNodeUID || c.PublicKey == p.RootPublicKey {
			return nil, nil, fmt.Errorf("receive retirement: incomplete, duplicate or retiring consumer identity")
		}
		names[c.NodeName], uids[c.NodeUID], keys[c.PublicKey] = true, true, true
		if c.Site {
			sites[c.NodeName] = c
		} else {
			if c.MachineName == "" || c.MachineName == w.MachineName || c.SecretUID == "" || c.TunnelAddress == "" || remotes[c.MachineName].NodeUID != "" {
				return nil, nil, fmt.Errorf("receive retirement: incomplete or duplicate remote consumer")
			}
			remotes[c.MachineName] = c
		}
	}
	for public, site := range p.Sites {
		c, found := sites[site.NodeName]
		if !found || c.NodeUID != site.NodeUID || c.PublicKey != public {
			return nil, nil, fmt.Errorf("receive retirement: withdrawal omits or replaces a journal participant")
		}
	}
	return sites, remotes, nil
}
