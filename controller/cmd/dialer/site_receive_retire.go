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
	if retire == nil {
		return fmt.Errorf("receiver retirement requires native guard")
	}
	return retireSiteReceiverRecords(ctx, api, cfg, mesh, key, path, scope, "", nil, func(selections []receiveRouteSelection, _ []string) error { return retire(selections) })
}

// withdrawnRoot is supplied only by the journal withdrawal executor. Ordinary
// absent-peer reconciliation must continue protecting every reserved root.
func retireSiteReceiverRecords(ctx context.Context, api kubernetes.Interface, cfg config, mesh *corev1.Secret, key wgtypes.Key, path string, scope *siteReceiveScope, withdrawnRoot string, prepare func(map[string][]receiveRouteSelection, map[string][]string) error, retire func([]receiveRouteSelection, []string) error, additionalPaths ...string) error {
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
	var files []string
	seenDirectories := map[string]bool{}
	for _, base := range append([]string{path}, additionalPaths...) {
		directory := filepath.Clean(base + ".owners")
		if seenDirectories[directory] {
			continue
		}
		seenDirectories[directory] = true
		info, err := os.Lstat(directory)
		if os.IsNotExist(err) {
			continue
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
		for _, entry := range entries {
			// Interrupted atomic writes are not committed ownership evidence.
			if strings.HasPrefix(entry.Name(), ".") && !entry.IsDir() {
				continue
			}
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
				return fmt.Errorf("unexpected receiver ownership entry")
			}
			files = append(files, filepath.Join(directory, entry.Name()))
		}
	}
	groups := map[string][]receiveRouteSelection{}
	destinations := map[string]map[string]bool{}
	removable := []string{}
	retained := map[string]siteReceiveCache{}
	// Validate all records and plans before the first native mutation.
	for _, name := range files {
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
		if filepath.Base(name) != fmt.Sprintf("%x.json", sha256.Sum256(raw)) {
			return fmt.Errorf("receiver ownership content digest changed")
		}
		record, err := decodeSiteReceiveCache(raw, cfg, key)
		if err != nil {
			return err
		}
		if record.MeshUID != string(mesh.UID) {
			continue
		}
		eligible := map[string]bool{}
		if withdrawnRoot == "" {
			absent, err := tunnel.PlanReceiveNeighborRetirements(record.Peers.Peers, current, scope.plans)
			if err != nil {
				return err
			}
			for _, peer := range absent {
				eligible[peer.PublicKey] = true
			}
		} else {
			for _, peer := range current {
				if peer.PublicKey == withdrawnRoot {
					return fmt.Errorf("withdrawn receiver root was republished")
				}
			}
			eligible[withdrawnRoot] = true
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
			plan, err := tunnel.PlanSiteReceiveClient(receiveScope, record.PublicKey, root, binding, record.previousPeers(root), record.Peers)
			if err != nil {
				return err
			}
			sum := sha256.Sum256([]byte(receiveScope + "\x00" + record.PublicKey + "\x00" + root))
			selection := receiveRouteSelection{Interface: plan.Interface, Alias: fmt.Sprintf("cloud-provisioning:receive-client:v1:%x", sum), PublicKey: record.PublicKey, PeerPublicKey: plan.PublicKey, AllowedSources: plan.AllowedSources}
			if withdrawnRoot != "" {
				for _, protected := range scope.plans {
					if protected.RootPublicKey == withdrawnRoot {
						continue
					}
					graphs := []tunnel.ReceiveTransitionPlan{protected}
					if protected.PreviousReceiver != nil {
						graphs = append(graphs, *protected.PreviousReceiver.Plan)
					}
					for _, graph := range graphs {
						if member, found := graph.Sites[record.PublicKey]; found && member.NodeName == cfg.nodeName && member.Client.Interface == selection.Interface {
							return fmt.Errorf("withdrawn receiver is reserved by another journal")
						}
					}
				}
			}
			groups[plan.Interface] = append(groups[plan.Interface], selection)
			if destinations[plan.Interface] == nil {
				destinations[plan.Interface] = map[string]bool{}
			}
			for _, doc := range []tunnel.PeerListDoc{record.previousPeers(root), record.Peers} {
				for _, peer := range doc.Peers {
					if peer.PublicKey != root {
						continue
					}
					for _, prefix := range peer.WGAllowedIPs {
						destinations[plan.Interface][prefix] = true
					}
					for _, host := range peer.AllRouteHosts() {
						destinations[plan.Interface][tunnel.HostCIDR(host)] = true
					}
				}
			}
		}
		if all {
			removable = append(removable, name)
		} else if withdrawnRoot != "" {
			if _, found := record.Bindings[withdrawnRoot]; found {
				next, err := withoutSiteReceiveBinding(record, withdrawnRoot)
				if err != nil {
					return err
				}
				retained[name] = next
			}
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
	routeLists := map[string][]string{}
	for _, name := range names {
		for prefix := range destinations[name] {
			routeLists[name] = append(routeLists[name], prefix)
		}
		sort.Strings(routeLists[name])
	}
	if prepare != nil && len(names) > 0 {
		if err := check(); err != nil {
			return err
		}
		if err := prepare(groups, routeLists); err != nil {
			return err
		}
	}
	for _, name := range names {
		if err := check(); err != nil {
			return err
		}
		if err := retire(groups[name], routeLists[name]); err != nil {
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
	for name, record := range retained {
		// Write surviving ownership before removing the mixed record. A crash
		// may duplicate intent, but cannot lose another receiver's provenance.
		base := strings.TrimSuffix(filepath.Dir(name), ".owners")
		if err := preserveSiteReceiveOwnership(base, record, cfg, key); err != nil {
			return err
		}
		if err := removeDurableCache(name); err != nil {
			return err
		}
	}
	return nil
}

// A crash can leave either the previously installed permissions or a prepared
// successor. Only an exact recorded state is eligible, never their union.
func retireRecordedSiteReceiver(devices receiverDevices, cfg config, selections []receiveRouteSelection) error {
	selection, err := selectRecordedSiteReceiver(devices, cfg, selections)
	if err != nil {
		return err
	}
	return retireReceiveDevice(devices, selection)
}

func selectRecordedSiteReceiver(devices receiverDevices, cfg config, selections []receiveRouteSelection) (receiveRouteSelection, error) {
	fail := func(message string) (receiveRouteSelection, error) {
		return receiveRouteSelection{}, fmt.Errorf("%s", message)
	}
	if len(selections) == 0 || devices == nil {
		return fail("receiver retirement requires recorded identities")
	}
	first := selections[0]
	for _, s := range selections {
		if s.Interface != first.Interface || s.Alias != first.Alias || s.PublicKey != first.PublicKey {
			return fail("receiver ownership records disagree")
		}
	}
	live, err := devices.Device(first.Interface)
	if err != nil {
		if os.IsNotExist(err) {
			return first, nil
		}
		return receiveRouteSelection{}, err
	}
	if live.FirewallMark != cfg.fwmark || live.PublicKey.String() != first.PublicKey {
		return fail("receiver retirement identity or firewall mark changed")
	}
	for _, s := range selections {
		if len(live.Peers) == 0 || (len(live.Peers) == 1 && live.Peers[0].PublicKey.String() == s.PeerPublicKey && receiveSourcesMatch(s.AllowedSources, live.Peers[0].AllowedIPs)) {
			return s, nil
		}
	}
	return fail("receiver state does not match recorded ownership")
}
