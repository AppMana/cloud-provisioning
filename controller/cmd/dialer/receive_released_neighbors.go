package main

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// A direct site can maintain ordinary neighbors after its final receiver is
// released. This admits current mesh ownership while keeping every historical
// journal and local retirement fence; it never restores a released receiver.
// Cloud workers still require their own active journal.
func admitReleasedReceiveNeighbors(ctx context.Context, api kubernetes.Interface, store handover.Store, scope receiveJournalScope, cfg config, key wgtypes.Key, lockPath string, mesh *corev1.Secret) (admittedReceiveSet, admittedReceiveJournal, receiveReleasedCheckpointAdmission, error) {
	fail := func(err error) (admittedReceiveSet, admittedReceiveJournal, receiveReleasedCheckpointAdmission, error) {
		return admittedReceiveSet{}, admittedReceiveJournal{}, receiveReleasedCheckpointAdmission{}, err
	}
	if cfg.secretName == "" || mesh == nil || strings.TrimSpace(string(mesh.Data[tunnel.NodePublicKeyPrefix+cfg.nodeName])) != key.PublicKey().String() {
		return fail(fmt.Errorf("released neighbor recovery requires current site ownership"))
	}
	address, err := netip.ParsePrefix(strings.TrimSpace(string(mesh.Data[tunnel.NodeTunnelAddressPrefix+cfg.nodeName])))
	if err != nil || !address.Addr().Is4() || !address.Addr().IsGlobalUnicast() {
		return fail(fmt.Errorf("released neighbor recovery requires a published site tunnel address"))
	}
	if raw := strings.TrimSpace(string(mesh.Data[tunnel.NodeTunnelAddress6Prefix+cfg.nodeName])); raw != "" {
		address, err := netip.ParsePrefix(raw)
		if err != nil || !address.Addr().Is6() || address.Addr().Is4In6() || !address.Addr().IsGlobalUnicast() {
			return fail(fmt.Errorf("released neighbor recovery has invalid site IPv6 allocation"))
		}
	}
	role, _, err := tunnel.SiteForwardingRole(mesh.Data, cfg.nodeName, nil)
	if err != nil {
		return fail(err)
	}
	if role != "direct" {
		return fail(fmt.Errorf("released neighbor recovery requires a direct site"))
	}
	inventory, index, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, scope.Namespace, scope.MeshName, scope.MeshUID)
	if err != nil {
		return fail(err)
	}
	if len(inventory.MachineUIDs) != 0 || len(inventory.Released) == 0 {
		return fail(fmt.Errorf("released neighbor recovery requires fully retired inventory"))
	}
	node, err := api.CoreV1().Nodes().Get(ctx, cfg.nodeName, metav1.GetOptions{})
	if err != nil {
		return fail(err)
	}
	boot, err := localReceiveBootID()
	if err != nil {
		return fail(err)
	}
	if err := validateReceiveParticipantNode(node, string(node.UID), boot); err != nil {
		return fail(err)
	}
	local := admittedReceiveJournal{NodeUID: string(node.UID), BootID: boot}
	next := receiveInventoryCheckpoint{Version: 1, InventoryUID: string(index.UID), Inventory: inventory, NodeName: cfg.nodeName, NodeUID: local.NodeUID, BootID: boot, PublicKey: key.PublicKey().String()}
	proof, err := admitReleasedReceiveCheckpoints(ctx, api, store, scope, cfg, key, lockPath, next, index.ObjectMeta)
	if err != nil {
		return fail(err)
	}
	if err := validateReceiveInventoryCheckpointWithRelease(lockPath+".inventory.json", next, proof); err != nil {
		return fail(err)
	}
	entries, err := os.ReadDir(filepath.Dir(lockPath))
	if err != nil {
		return fail(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), filepath.Base(lockPath)+".receive-transition-") && !proof.localFiles[entry.Name()] {
			return fail(fmt.Errorf("released neighbor recovery omitted a local journal"))
		}
	}
	current, err := api.CoreV1().Secrets(scope.Namespace).Get(ctx, scope.MeshName, metav1.GetOptions{})
	if err != nil {
		return fail(err)
	}
	if !sameReceiveRevision(mesh.ObjectMeta, current.ObjectMeta) {
		return fail(fmt.Errorf("released neighbor mesh changed"))
	}
	if err := proof.recheck(ctx, api, store); err != nil {
		return fail(err)
	}
	set := admittedReceiveSet{Inventory: inventory, Metadata: index.ObjectMeta}
	if err := appendReleasedReceiveExclusions(inventory, &set.Routes); err != nil {
		return fail(err)
	}
	return set, local, proof, nil
}

func checkpointReleasedReceiveNeighbors(ctx context.Context, api kubernetes.Interface, store handover.Store, cfg config, key wgtypes.Key, lockPath string, set admittedReceiveSet, local admittedReceiveJournal, proof receiveReleasedCheckpointAdmission) error {
	next := receiveInventoryCheckpoint{Version: 1, InventoryUID: string(set.Metadata.UID), Inventory: set.Inventory, NodeName: cfg.nodeName, NodeUID: local.NodeUID, BootID: local.BootID, PublicKey: key.PublicKey().String()}
	if err := validateReceiveInventoryCheckpointWithRelease(lockPath+".inventory.json", next, proof); err != nil {
		return err
	}
	if err := proof.recheck(ctx, api, store); err != nil {
		return err
	}
	return writeDurableJSON(lockPath+".inventory.json", next)
}
