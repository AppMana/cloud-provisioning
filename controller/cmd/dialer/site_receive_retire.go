package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Ownership records are intent only. Admit absence from a complete live mesh
// and journal inventory before invoking the existing native retirement guard.
// Legacy callers without an admitted inventory preserve records for later use.
func retireMissingSiteReceivers(ctx context.Context, api kubernetes.Interface, cfg config, mesh *corev1.Secret, key wgtypes.Key, path string, scope *siteReceiveScope, retire func([]receiveRouteSelection) error) error {
	if scope == nil || len(scope.plans) == 0 {
		return nil
	}
	if scope.check == nil || retire == nil || api == nil {
		return fmt.Errorf("receiver retirement requires live admission and native guard")
	}
	current, err := tunnel.SitePeers(mesh.Data)
	if err != nil {
		return err
	}
	directory := path + ".owners"
	info, err := os.Lstat(directory)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		return fmt.Errorf("receiver ownership directory must be private")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	groups := map[string][]receiveRouteSelection{}
	removable := []string{}
	// Validate all records and plans before the first native mutation.
	for _, entry := range entries {
		// An interrupted atomic write can leave an uncommitted temporary file.
		// It is not ownership evidence and cannot authorize a native mutation.
		if strings.HasPrefix(entry.Name(), ".") && !entry.IsDir() {
			continue
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			return fmt.Errorf("unexpected receiver ownership entry")
		}
		name := filepath.Join(path+".owners", entry.Name())
		info, err := os.Lstat(name)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("receiver ownership record is not regular")
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		if entry.Name() != fmt.Sprintf("%x.json", sha256.Sum256(raw)) {
			return fmt.Errorf("receiver ownership content digest changed")
		}
		record, err := decodeSiteReceiveCache(raw, cfg, key)
		if err != nil {
			return err
		}
		if record.MeshUID != string(mesh.UID) {
			continue
		}
		absent, err := tunnel.PlanReceiveNeighborRetirements(record.Peers.Peers, current, scope.plans)
		if err != nil {
			return err
		}
		eligible := map[string]bool{}
		for _, peer := range absent {
			eligible[peer.PublicKey] = true
		}
		all := len(record.Bindings) > 0
		for root, binding := range record.Bindings {
			if !eligible[root] {
				all = false
				continue
			}
			receiveScope, err := record.receiveScope(root)
			if err != nil {
				return err
			}
			plan, err := tunnel.PlanSiteReceiveClient(receiveScope, record.PublicKey, root, binding, record.Peers, record.Peers)
			if err != nil {
				return err
			}
			sum := sha256.Sum256([]byte(receiveScope + "\x00" + record.PublicKey + "\x00" + root))
			selection := receiveRouteSelection{Interface: plan.Interface, Alias: fmt.Sprintf("cloud-provisioning:receive-client:v1:%x", sum), PublicKey: record.PublicKey, PeerPublicKey: plan.PublicKey, AllowedSources: plan.AllowedSources}
			groups[plan.Interface] = append(groups[plan.Interface], selection)
		}
		if all {
			removable = append(removable, name)
		}
	}
	check := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := scope.check(); err != nil {
			return err
		}
		live, err := api.CoreV1().Secrets(mesh.Namespace).Get(ctx, mesh.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if !sameReceiveRevision(mesh.ObjectMeta, live.ObjectMeta) {
			return fmt.Errorf("receiver retirement publication changed")
		}
		return nil
	}
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := check(); err != nil {
			return err
		}
		if err := retire(groups[name]); err != nil {
			return err
		}
	}
	// Native retirement has verified absence while the caller still holds the
	// host lock. Commit that fact even if the admission deadline has now elapsed:
	// this only removes obsolete local evidence, not kernel state. A subsequent
	// preparation must write fresh ownership intent before recreating a device.
	// Re-admitting here can starve this commit under client-side rate limiting.
	for _, name := range removable {
		if err := removeDurableCache(name); err != nil {
			return err
		}
	}
	return nil
}

// A crash can leave either the previously installed permissions or a prepared
// successor. Only an exact recorded state is eligible, never their union.
func retireRecordedSiteReceiver(devices receiverDevices, cfg config, selections []receiveRouteSelection) error {
	if len(selections) == 0 || devices == nil {
		return fmt.Errorf("receiver retirement requires recorded identities")
	}
	first := selections[0]
	for _, s := range selections {
		if s.Interface != first.Interface || s.Alias != first.Alias || s.PublicKey != first.PublicKey {
			return fmt.Errorf("receiver ownership records disagree")
		}
	}
	live, err := devices.Device(first.Interface)
	if err != nil {
		if os.IsNotExist(err) {
			return retireReceiveDevice(devices, first)
		}
		return err
	}
	if live.FirewallMark != cfg.fwmark {
		return fmt.Errorf("receiver retirement firewall mark changed")
	}
	for _, s := range selections {
		if len(live.Peers) == 0 || (len(live.Peers) == 1 && live.Peers[0].PublicKey.String() == s.PeerPublicKey && receiveSourcesMatch(s.AllowedSources, live.Peers[0].AllowedIPs)) {
			return retireReceiveDevice(devices, s)
		}
	}
	return fmt.Errorf("receiver state does not match recorded ownership")
}
