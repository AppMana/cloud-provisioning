package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Real API storage and synthetic completed serving histories. The default uses
// native callbacks for ordering, deadline sharing and receipt fencing. The
// explicitly isolated native variant also checks kernel cleanup and authenticated
// release; neither variant proves live continuity or actual worker reboot.
func verifyJointRetirementExecution(t *testing.T, api kubernetes.Interface, clusterUID string, nativeMode ...bool) {
	useNative := len(nativeMode) != 0 && nativeMode[0]
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	ns, err := api.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "joint-retirement"}}, metav1.CreateOptions{})
	must(err)
	mesh, err := api.CoreV1().Secrets(ns.Name).Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mesh"}}, metav1.CreateOptions{})
	must(err)
	node, err := api.CoreV1().Nodes().Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "joint-site"}}, metav1.CreateOptions{})
	must(err)
	boot, err := localReceiveBootID()
	must(err)
	node.Status.NodeInfo.BootID = boot
	node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	node, err = api.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{})
	must(err)
	site, err := wgtypes.GeneratePrivateKey()
	must(err)
	public := site.PublicKey().String()
	mesh.Data = map[string][]byte{tunnel.NodePublicKeyPrefix + node.Name: []byte(public), tunnel.NodeTunnelAddressPrefix + node.Name: []byte("10.90.0.2/24")}
	plans := []tunnel.ReceiveTransitionPlan{}
	stores := []handover.Store{}
	snapshots := []handover.Snapshot{}
	inventory := tunnel.ReceiveInventory{Version: 1, ClusterUID: clusterUID, Namespace: ns.Name, MeshName: mesh.Name, MeshUID: string(mesh.UID), Publications: map[string]tunnel.ReceiveJournalPublication{}}
	for i := 0; i < 2; i++ {
		root, err := wgtypes.GeneratePrivateKey()
		must(err)
		plan := receiveRestoreTestPlan(t, root, site)
		plan.Namespace, plan.MeshUID = ns.Name, string(mesh.UID)
		plan.MachineName, plan.MachineUID = fmt.Sprintf("worker-%d", i), fmt.Sprintf("worker-uid-%d", i)
		plan.CloudNodeName, plan.CloudNodeUID = fmt.Sprintf("cloud-%d", i), fmt.Sprintf("cloud-uid-%d", i)
		plan.AdoptionName, plan.AdoptionUID = tunnel.AdoptionSecretName(plan.MachineName), fmt.Sprintf("adoption-%d", i)
		preparation, err := tunnel.PlanReceiveHandover(plan.MeshUID, plan.Desired, plan.Desired, nil)
		must(err)
		binding := plan.Bindings[public]
		binding.Interface = preparation.Receivers[0].Interface
		binding.PublicKey = receiverKey(root, plan.MeshUID, public).PublicKey().String()
		plan.Bindings[public] = binding
		plan.CloudAddresses = []string{fmt.Sprintf("172.29.0.%d/32", i+1), fmt.Sprintf("fd29::%d/128", i+1)}
		plan.SiteDesired.Peers[0].WGAllowedIPs = slices.Clone(plan.CloudAddresses)
		plan.SiteDesired.Peers[0].RouteHosts = []string{fmt.Sprintf("172.29.0.%d", i+1), fmt.Sprintf("fd29::%d", i+1)}
		local := plan.Sites[public]
		local.NodeName, local.NodeUID = node.Name, string(node.UID)
		local.Client, err = tunnel.PlanSiteReceiveClient(plan.MeshUID, public, plan.RootPublicKey, binding, plan.SiteDesired, plan.SiteDesired)
		must(err)
		plan.Sites[public] = local
		raw, err := json.Marshal(plan)
		must(err)
		spec, err := tunnel.ReceiveTransitionSpec(plan, clusterUID, fmt.Sprintf("%x", sha256.Sum256(raw)))
		must(err)
		transition, err := handover.NewTransition(spec, time.Now(), handover.Policy{RoundLifetime: time.Minute, MaxReceiptAge: 30 * time.Second, DrainInterval: time.Second})
		must(err)
		store := handover.Store{ConfigMaps: api.CoreV1().ConfigMaps(ns.Name), ClusterUID: clusterUID, Name: tunnel.ReceiveTransitionName(plan.MachineUID)}
		_, err = store.CreateWithPlan(ctx, transition, raw)
		must(err)
		snapshot := completeReceiveTestJournal(t, store)
		snapshot, err = store.FreezeCompleted(ctx, snapshot)
		must(err)
		store.ExpectedUID = string(snapshot.Metadata().UID)
		inventory.MachineUIDs = append(inventory.MachineUIDs, plan.MachineUID)
		inventory.Publications[plan.MachineUID] = tunnel.ReceiveJournalPublication{JournalUID: store.ExpectedUID}
		addNativeWithdrawalPeer(mesh, plan)
		plans, stores, snapshots = append(plans, plan), append(stores, store), append(snapshots, snapshot)
	}
	mesh.Data[tunnel.NodeTunnelAddress6Prefix+node.Name] = []byte("fd90::2/64")
	mesh.Data[tunnel.NodeAddressesPrefix+node.Name] = []byte("10.90.0.2,fd90::2")
	mesh, err = api.CoreV1().Secrets(ns.Name).Update(ctx, mesh, metav1.UpdateOptions{})
	must(err)
	raw, err := json.Marshal(inventory)
	must(err)
	index, err := stores[0].ConfigMaps.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: tunnel.ReceiveInventoryName(string(mesh.UID))}, Data: map[string]string{"inventory.json": string(raw)}}, metav1.CreateOptions{})
	must(err)
	raw, err = json.Marshal(mesh.Data)
	must(err)
	sourceHash := tunnel.HashPeerList(raw)
	for i, plan := range plans {
		other := plans[1-i]
		w := attachment.RemoteWithdrawalIntent{MeshNamespace: ns.Name, MeshName: mesh.Name, MeshUID: string(mesh.UID), SourceVersion: mesh.ResourceVersion, SourceDataHash: sourceHash, MachineName: plan.MachineName, MachineUID: plan.MachineUID, PublicKey: plan.RootPublicKey, DrainIntent: "test/joint-retirement", Consumers: []attachment.ConsumerTarget{
			{Site: true, NodeName: node.Name, NodeUID: string(node.UID), PublicKey: public},
			{NodeName: other.CloudNodeName, NodeUID: other.CloudNodeUID, MachineName: other.MachineName, SecretUID: other.AdoptionUID, PublicKey: other.RootPublicKey, TunnelAddress: fmt.Sprintf("172.29.0.%d", 2-i)},
		}}
		must(w.ReserveReceiveWithdrawal(ctx, stores[i].ConfigMaps, clusterUID, string(index.UID), mesh, snapshots[i]))
	}
	publishedMesh := mesh.DeepCopy()
	for _, plan := range plans {
		mesh.Data, _, err = tunnel.WithdrawRemotePeer(mesh.Data, plan.MachineName, plan.RootPublicKey)
		must(err)
	}
	mesh, err = api.CoreV1().Secrets(ns.Name).Update(ctx, mesh, metav1.UpdateOptions{})
	must(err)
	podSpec := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "agent"}, Spec: corev1.PodSpec{NodeName: node.Name, Containers: []corev1.Container{{Name: "dialer", Image: "test.invalid/agent"}}}}
	if useNative {
		podSpec = nativeJointReceiptPod(t, api, ns.Name, node.Name)
	}
	pod, err := api.CoreV1().Pods(ns.Name).Create(ctx, podSpec, metav1.CreateOptions{})
	must(err)
	pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: []corev1.ContainerStatus{{Name: "dialer", Ready: true, ContainerID: "containerd://joint", Image: pod.Spec.Containers[0].Image, ImageID: pod.Spec.Containers[0].Image, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(time.Now().Add(-time.Minute))}}}}}
	pod, err = api.CoreV1().Pods(ns.Name).UpdateStatus(ctx, pod, metav1.UpdateOptions{})
	must(err)
	scope := receiveJournalScope{Namespace: ns.Name, MeshName: mesh.Name, MeshUID: string(mesh.UID), MachineUID: plans[0].MachineUID}
	cfg := config{iface: "jointtest", nodeName: node.Name, privateKeyFile: filepath.Join(t.TempDir(), "key"), receiptPodName: pod.Name, receiptPodUID: string(pod.UID)}
	var native receiveRetirementNative
	if useNative {
		cfg.secretNamespace, cfg.secretName = ns.Name, mesh.Name
		cfg.routeTable, cfg.fwmark, cfg.mtu, cfg.bgpListenPort = 518, 518, 1400, -1
		native = prepareNativeJointExecution(t, api, stores[0], scope, cfg, site, plans, publishedMesh)
	}
	t.Run("dispatch-once", func(t *testing.T) { verifyJointDispatchOnce(t, api, stores, mesh, cfg, site, native) })
	t.Run("member-scope", func(t *testing.T) {
		admitted, err := loadReceiveRetirement(ctx, api, stores[0], scope, cfg.nodeName, site)
		must(err)
		for _, fault := range []string{"none", "cluster", "namespace", "mesh", "journal-name", "journal-uid"} {
			t.Run(fault, func(t *testing.T) {
				store, memberScope := retirementMemberScope(stores[0], scope, admitted, plans[1].MachineUID)
				switch fault {
				case "cluster":
					store.ClusterUID = "replacement"
				case "namespace":
					memberScope.Namespace = "replacement"
				case "mesh":
					memberScope.MeshUID = "replacement"
				case "journal-name":
					store.Name = stores[0].Name
				case "journal-uid":
					store.ExpectedUID = "replacement"
				}
				member, err := receiveRetirementMember(store, memberScope, admitted)
				if fault != "none" {
					if err == nil {
						t.Fatal("member admitted under a different source scope")
					}
					return
				}
				must(err)
				if member.Withdrawal.MachineUID != plans[1].MachineUID || !sameReceiveRetirement(admitted, member) {
					t.Fatal("member selection changed admitted source or selected the wrong withdrawal")
				}
				must(recheckReceiveRetirement(ctx, api, store, member, cfg.nodeName, site))
			})
		}
	})
	for _, fault := range []string{"second-fails", "cancelled-after-first", "cancelled-post-native", "pod-after-first", "parent-deadline", "none", "source-after-first"} {
		t.Run(fault, func(t *testing.T) {
			pod.Status.ContainerStatuses[0].ContainerID = "containerd://joint"
			pod, err = api.CoreV1().Pods(ns.Name).UpdateStatus(ctx, pod, metav1.UpdateOptions{})
			must(err)
			runCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			var parentDeadline time.Time
			if fault == "parent-deadline" {
				parentDeadline = time.Now().Add(20 * time.Second)
				var stop context.CancelFunc
				runCtx, stop = context.WithDeadline(runCtx, parentDeadline)
				defer stop()
			}
			var calls []string
			var deadline, secondStarted time.Time
			var reads atomic.Int64
			var cancelPostNative atomic.Bool
			var beforeNative int64
			reader := stores[0]
			reader.ConfigMaps = receiveSetReader{ConfigMapInterface: reader.ConfigMaps, before: func(string) error {
				reads.Add(1)
				if cancelPostNative.Load() {
					cancel()
					return runCtx.Err()
				}
				return nil
			}}
			receipts, err := executeReceiveRetirementCohort(runCtx, api, reader, scope, cfg, site, "containerd://joint", func(run context.Context, path string, admitted admittedReceiveRetirement) error {
				if len(calls) == 0 {
					beforeNative = reads.Load()
				}
				calls = append(calls, admitted.Withdrawal.MachineUID)
				if len(calls) == 2 {
					secondStarted = time.Now()
				}
				got, ok := run.Deadline()
				if !ok {
					return fmt.Errorf("missing cleanup deadline")
				}
				if !parentDeadline.IsZero() && !got.Equal(parentDeadline) {
					return fmt.Errorf("cohort extended caller deadline")
				}
				if deadline.IsZero() {
					deadline = got
				} else if !deadline.Equal(got) {
					return fmt.Errorf("member reset deadline")
				}
				if lock, err := lockReconciliation(cfg); err == nil {
					lock.Close()
					return fmt.Errorf("native cleanup lost host lock")
				}
				if fault == "second-fails" && len(calls) == 2 {
					return fmt.Errorf("second native cleanup failed")
				}
				if native != nil {
					if err := native(run, path, admitted); err != nil {
						return err
					}
				}
				if fault == "cancelled-after-first" && len(calls) == 1 {
					cancel()
				}
				if fault == "cancelled-post-native" && len(calls) == 2 {
					// Both native callbacks succeeded. Cancel the next journal
					// read during full post-cleanup admission, not native work.
					cancelPostNative.Store(true)
				}
				if fault == "pod-after-first" && len(calls) == 1 {
					pod.Status.ContainerStatuses[0].ContainerID = "containerd://replacement"
					var e error
					pod, e = api.CoreV1().Pods(ns.Name).UpdateStatus(ctx, pod, metav1.UpdateOptions{})
					return e
				}
				if fault == "source-after-first" && len(calls) == 1 {
					current, e := api.CoreV1().Secrets(ns.Name).Get(ctx, mesh.Name, metav1.GetOptions{})
					if e != nil {
						return e
					}
					current.Annotations = map[string]string{"test-race": "changed"}
					_, e = api.CoreV1().Secrets(ns.Name).Update(ctx, current, metav1.UpdateOptions{})
					return e
				}
				return nil
			})
			if fault == "none" || fault == "parent-deadline" {
				t.Logf("cohort ConfigMap reads: before native=%d total=%d", beforeNative, reads.Load())
				if !useNative && (beforeNative > 25 || reads.Load() > 47) {
					t.Fatalf("cohort reconstructed shared histories: before native=%d total=%d", beforeNative, reads.Load())
				}
				if err != nil || len(receipts) != len(stores) || !slices.Equal(calls, inventory.MachineUIDs) {
					t.Fatalf("joint cleanup did not complete: calls=%v receipts=%d err=%v", calls, len(receipts), err)
				}
				var observed time.Time
				bindings := map[string]bool{}
				for i, plan := range plans {
					entries := receipts[plan.MachineUID]
					if len(entries) != 1 || entries[0].BearerToken != "" {
						t.Fatal("cohort member missing credential-free observation")
					}
					entry := entries[0].Receipt
					if entry.ObservedAt.After(secondStarted) || (!observed.IsZero() && !entry.ObservedAt.Equal(observed)) {
						t.Fatal("later member renewed the first observation timestamp")
					}
					observed = entry.ObservedAt
					current, e := stores[i].Load(ctx)
					must(e)
					binding, e := attachment.NewReceiveRetirementReceiptScope(ctx, stores[i].ConfigMaps, clusterUID, string(index.UID), current, mesh)
					must(e)
					if entry.RoundHash != binding.Digest() || bindings[entry.RoundHash] {
						t.Fatal("cohort receipt reused another member's binding")
					}
					bindings[entry.RoundHash] = true
				}
			} else {
				if err == nil || len(receipts) != 0 {
					t.Fatal("partial or raced cleanup emitted receipt")
				}
				want := 2
				if fault != "second-fails" && fault != "cancelled-post-native" {
					want = 1
				}
				if len(calls) != want {
					t.Fatalf("fault not exercised: calls=%v err=%v", calls, err)
				}
				if fault == "cancelled-post-native" && !errors.Is(err, context.Canceled) {
					t.Fatalf("post-cleanup cancellation was not preserved: %v", err)
				}
			}
		})
	}
	if useNative {
		verifyNativeJointReceiptLifecycle(t, api, stores, scope, cfg, site, pod, native)
	}
}
