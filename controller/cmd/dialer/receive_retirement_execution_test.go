package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Reuse the real API fixture after cloud deletion. Native cleanup is a callback
// stub: this proves checkpoint ordering and receipt suppression on failure or
// identity/source races, not kernel absence, token authentication or transport.
// The executor sets a 30-second context deadline; native callbacks must honor
// it. No interruption allowance applies here.
func verifyReceiveRetirementExecution(t *testing.T, api kubernetes.Interface, store handover.Store, scope receiveJournalScope, key wgtypes.Key) {
	t.Helper()
	ctx := context.Background()
	pod, err := api.CoreV1().Pods(scope.Namespace).Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "retirement-agent"}, Spec: corev1.PodSpec{NodeName: "site", Containers: []corev1.Container{{Name: "dialer", Image: "test.invalid/agent"}}}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, fault := range []string{"none", "native-error", "pod-before", "pod-after", "mesh-after", "inventory-after", "cancelled", "cancelled-after"} {
		t.Run("retirement-execution/"+fault, func(t *testing.T) {
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: []corev1.ContainerStatus{{Name: "dialer", Ready: true, ContainerID: "containerd://agent", Image: "test.invalid/agent", ImageID: "test-image", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(time.Now().Add(-time.Minute))}}}}}
			pod, err = api.CoreV1().Pods(scope.Namespace).UpdateStatus(ctx, pod, metav1.UpdateOptions{})
			must(err)
			cfg := config{iface: "retireexec", nodeName: "site", privateKeyFile: filepath.Join(t.TempDir(), "key"), receiptPodName: pod.Name, receiptPodUID: string(pod.UID)}
			if fault == "pod-before" {
				cfg.receiptPodUID = "replaced"
			}
			runCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			if fault == "cancelled" {
				cancel()
			}
			called := false
			receipts, runErr := executeReceiveRetirement(runCtx, api, store, scope, cfg, key, "containerd://agent", func(_ context.Context, lockPath string, admitted admittedReceiveRetirement) error {
				called = true
				if other, err := lockReconciliation(cfg); err == nil {
					other.Close()
					return errors.New("native callback did not hold the host lock")
				}
				if _, err := os.Stat(lockPath + ".inventory.json"); err != nil {
					return err
				}
				switch fault {
				case "cancelled-after":
					cancel()
					return nil
				case "native-error":
					return errors.New("cleanup did not verify")
				case "pod-after":
					pod.Status.ContainerStatuses[0].ContainerID = "containerd://replacement"
					pod, err = api.CoreV1().Pods(scope.Namespace).UpdateStatus(ctx, pod, metav1.UpdateOptions{})
					return err
				case "mesh-after":
					mesh, e := api.CoreV1().Secrets(scope.Namespace).Get(ctx, scope.MeshName, metav1.GetOptions{})
					if e != nil {
						return e
					}
					mesh.Annotations = map[string]string{"execution-race": fault}
					_, e = api.CoreV1().Secrets(scope.Namespace).Update(ctx, mesh, metav1.UpdateOptions{})
					return e
				case "inventory-after":
					index, e := store.ConfigMaps.Get(ctx, admitted.Metadata.Name, metav1.GetOptions{})
					if e != nil {
						return e
					}
					index.Annotations = map[string]string{"execution-race": fault}
					_, e = store.ConfigMaps.Update(ctx, index, metav1.UpdateOptions{})
					return e
				}
				return nil
			})
			if fault != "none" {
				if runErr == nil || len(receipts) != 0 {
					t.Fatal("failed cleanup emitted receipt")
				}
				if fault != "pod-before" && fault != "cancelled" && !called {
					t.Fatal("fault was not exercised", runErr)
				}
				if (fault == "pod-before" || fault == "cancelled") && called {
					t.Fatal("invalid agent reached native callback")
				}
				return
			}
			must(runErr)
			if !called || len(receipts) != 1 {
				t.Fatal("verified callback did not produce one observation")
			}
			got := receipts[0]
			if got.BearerToken != "" || got.Receipt.RoundHash == "" || got.Receipt.AgentPodUID != string(pod.UID) || got.Receipt.AgentContainerID != "containerd://agent" || got.Receipt.DeviceID != "" || got.BootID == "" {
				t.Fatal("invalid retirement observation")
			}
		})
	}
}
