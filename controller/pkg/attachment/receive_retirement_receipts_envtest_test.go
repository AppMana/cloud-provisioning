package attachment

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"k8s.io/client-go/rest"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

const retirementTestImage = "example.invalid/dialer@sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestRetirementReceiptAuthenticationRealAPI(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("requires real Kubernetes API assets")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Error(err)
		}
	})
	api, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ns := "receipt-auth"
	_, err = api.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sa, err := api.CoreV1().ServiceAccounts(ns).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "dialer"}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{"app": "dialer"}
	daemon, err := api.AppsV1().DaemonSets(ns).Create(ctx, &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "dialer"}, Spec: appsv1.DaemonSetSpec{Selector: &metav1.LabelSelector{MatchLabels: labels}, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{ServiceAccountName: sa.Name, Containers: []corev1.Container{{Name: "dialer", Image: retirementTestImage}}}}}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	verifier := handover.PodReceiptAuthenticator{API: api, Audience: "cloud-provisioning-receipts", Namespace: ns, ServiceAccountName: sa.Name, ServiceAccountUID: string(sa.UID), DaemonSetName: daemon.Name, DaemonSetUID: string(daemon.UID), ContainerName: "dialer", Image: retirementTestImage, ImageID: retirementTestImage}
	var nodes []*corev1.Node
	var pods []*corev1.Pod
	for _, name := range []string{"worker-a", "worker-b"} {
		node, err := api.CoreV1().Nodes().Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		node.Status.NodeInfo.BootID = "boot-" + name
		node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
		node, err = api.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		pod, err := api.CoreV1().Pods(ns).Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(daemon, appsv1.SchemeGroupVersion.WithKind("DaemonSet"))}}, Spec: corev1.PodSpec{NodeName: node.Name, ServiceAccountName: sa.Name, Containers: []corev1.Container{{Name: "dialer", Image: retirementTestImage}}}}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		pod.Status.Phase = corev1.PodRunning
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "dialer", Ready: true, ContainerID: "containerd://running-agent", Image: "sha256:runtime-config", ImageID: retirementTestImage, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}}}}
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		pod, err = api.CoreV1().Pods(ns).UpdateStatus(ctx, pod, metav1.UpdateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, node)
		pods = append(pods, pod)
	}
	mint := func(pod *corev1.Pod, audience string) string {
		t.Helper()
		req := &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{Audiences: []string{audience}}}
		if pod != nil {
			req.Spec.BoundObjectRef = &authenticationv1.BoundObjectReference{APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID}
		}
		result, err := api.CoreV1().ServiceAccounts(ns).CreateToken(ctx, sa.Name, req, metav1.CreateOptions{})
		if err != nil {
			t.Fatal("cannot mint fixture token")
		}
		return result.Status.Token
	}

	// This fixture exercises the retirement batch through real TokenRequest and
	// TokenReview APIs. Persisted-scope construction has a separate real-API
	// test; these agents do not execute native cleanup or release reservations.
	scope := ReceiveRetirementReceiptScope{digest: strings.Repeat("a", 64), nodes: map[string]string{}}
	policies := map[string]handover.PodReceiptAuthenticator{}
	var batch []handover.PodReceipt
	for i, node := range nodes {
		uid := string(node.UID)
		scope.nodes[uid] = node.Name
		policies[uid] = verifier
		batch = append(batch, handover.PodReceipt{BearerToken: mint(pods[i], verifier.Audience), BootID: node.Status.NodeInfo.BootID, Receipt: handover.Receipt{NodeUID: uid, RoundHash: scope.digest, AgentContainerID: "containerd://running-agent", ObservedAt: time.Now()}})
	}

	// Each consumer submits its own pod-bound credential. Partial admission
	// must not relax complete-batch authorization, and rejects a different
	// consumer's token. This checks authentication, not native absence.
	for i := range batch {
		single := []handover.PodReceipt{batch[i]}
		if proofs, err := scope.AuthenticatePartial(ctx, single, policies, time.Minute); err != nil || len(proofs) != 1 {
			t.Fatal("independent consumer authentication failed", err)
		}
		if _, err := scope.AuthenticateComplete(ctx, single, policies, time.Minute); err == nil {
			t.Fatal("partial submission authorized completion")
		}
		single[0].BearerToken = batch[1-i].BearerToken
		if _, err := scope.AuthenticatePartial(ctx, single, policies, time.Minute); err == nil {
			t.Fatal("partial submission accepted another consumer credential")
		}
	}
	if _, err := scope.AuthenticatePartial(ctx, nil, policies, time.Minute); err == nil {
		t.Fatal("empty partial submission accepted")
	}
	verifyRetirementReceiptCollection(t, scope, batch, policies)
	proofs, err := scope.AuthenticateComplete(ctx, batch, policies, time.Minute)
	if err != nil || len(proofs) != 2 {
		t.Fatal("complete retirement batch failed authentication", err)
	}
	for i, proof := range proofs {
		if proof.AuthenticatedNodeUID != string(nodes[i].UID) || proof.Receipt.AgentPodUID != string(pods[i].UID) || proof.Receipt.ObservedBootID != batch[i].BootID {
			t.Fatal("retirement lost authenticated incarnation")
		}
	}
	reject := func(name string, candidate []handover.PodReceipt) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			got, err := scope.AuthenticateComplete(ctx, candidate, policies, time.Minute)
			if err == nil || len(got) != 0 {
				t.Fatal("invalid retirement batch authenticated")
			}
			for _, entry := range candidate {
				if entry.BearerToken != "" && strings.Contains(err.Error(), entry.BearerToken) {
					t.Fatal("authentication error exposed bearer")
				}
			}
		})
	}
	for _, fault := range []string{"boot", "container"} {
		t.Run("first-consumer-changes-during-second-authentication/"+fault, func(t *testing.T) {
			reviews := 0
			racing := rest.CopyConfig(cfg)
			racing.WrapTransport = func(base http.RoundTripper) http.RoundTripper {
				return retirementRoundTripper(func(request *http.Request) (*http.Response, error) {
					if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/tokenreviews") {
						reviews++
						if reviews == 2 {
							if fault == "boot" {
								nodes[0].Status.NodeInfo.BootID = "raced-boot"
								nodes[0], err = api.CoreV1().Nodes().UpdateStatus(ctx, nodes[0], metav1.UpdateOptions{})
							} else {
								pods[0].Status.ContainerStatuses[0].ContainerID = "containerd://raced"
								pods[0], err = api.CoreV1().Pods(ns).UpdateStatus(ctx, pods[0], metav1.UpdateOptions{})
							}
							if err != nil {
								t.Fatal(err)
							}
						}
					}
					return base.RoundTrip(request)
				})
			}
			racingAPI, e := kubernetes.NewForConfig(racing)
			if e != nil {
				t.Fatal(e)
			}
			racingPolicy := verifier
			racingPolicy.API = racingAPI
			both := map[string]handover.PodReceiptAuthenticator{}
			for uid := range policies {
				both[uid] = racingPolicy
			}
			got, e := scope.AuthenticateComplete(ctx, batch, both, time.Minute)
			nodes[0].Status.NodeInfo.BootID = batch[0].BootID
			nodes[0], err = api.CoreV1().Nodes().UpdateStatus(ctx, nodes[0], metav1.UpdateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			pods[0].Status.ContainerStatuses[0].ContainerID = "containerd://running-agent"
			pods[0], err = api.CoreV1().Pods(ns).UpdateStatus(ctx, pods[0], metav1.UpdateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if reviews != 2 {
				t.Fatal("race did not reach second authentication")
			}
			if e == nil || len(got) != 0 {
				t.Fatal("batch retained proof from a consumer that rebooted during authentication")
			}
		})
	}
	copyBatch := func() []handover.PodReceipt { return append([]handover.PodReceipt(nil), batch...) }
	swapped := copyBatch()
	swapped[0].BearerToken = batch[1].BearerToken
	reject("another-node-token", swapped)
	wrongAudience := copyBatch()
	wrongAudience[0].BearerToken = mint(pods[0], "other-audience")
	reject("wrong-audience", wrongAudience)
	unbound := copyBatch()
	unbound[0].BearerToken = mint(nil, verifier.Audience)
	reject("unbound-token", unbound)
	spoofed := copyBatch()
	spoofed[0].Receipt.AgentPodUID = "invented"
	spoofed[0].Receipt.ObservedBootID = "invented"
	proof, err := scope.AuthenticateComplete(ctx, spoofed, policies, time.Minute)
	if err != nil || proof[0].Receipt.AgentPodUID != string(pods[0].UID) || proof[0].Receipt.ObservedBootID != batch[0].BootID {
		t.Fatal("trusted payload incarnation", err)
	}
	nodes[0].Status.NodeInfo.BootID = "rebooted"
	nodes[0], err = api.CoreV1().Nodes().UpdateStatus(ctx, nodes[0], metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	reject("old-boot", batch)
	nodes[0].Status.NodeInfo.BootID = batch[0].BootID
	nodes[0], err = api.CoreV1().Nodes().UpdateStatus(ctx, nodes[0], metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pods[0].Status.ContainerStatuses[0].ContainerID = "containerd://replacement"
	pods[0], err = api.CoreV1().Pods(ns).UpdateStatus(ctx, pods[0], metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	reject("old-container", batch)
	fresh := copyBatch()
	fresh[0].Receipt.AgentContainerID = "containerd://replacement"
	fresh[0].Receipt.ObservedAt = time.Now()
	if _, err := scope.AuthenticateComplete(ctx, fresh, policies, time.Minute); err != nil {
		t.Fatal("fresh replacement observation rejected", err)
	}
	verifyRetirementAuthenticatedCommit(t, api, verifier, nodes[0], pods[0], fresh[0])
	pods[0].Status.ContainerStatuses[0].ImageID = "sha256:" + strings.Repeat("b", 64)
	pods[0], err = api.CoreV1().Pods(ns).UpdateStatus(ctx, pods[0], metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	reject("wrong-running-image", fresh)
}

type retirementRoundTripper func(*http.Request) (*http.Response, error)

func (f retirementRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Real TLS submissions and real pod-bound authentication, with synthetic scope
// and completion callback. Proves independent collection and reauthentication;
// does not prove native cleanup, source admission or persisted completion.
func verifyRetirementReceiptCollection(t *testing.T, scope ReceiveRetirementReceiptScope, batch []handover.PodReceipt, policies map[string]handover.PodReceiptAuthenticator) {
	t.Helper()
	var completed atomic.Int32
	handler, err := handover.NewReceiptCollectionHandler(func(ctx context.Context, machine string) (handover.ReceiptCollection, error) {
		target := handover.ReceiptCollection{UID: "retirement-fixture", Digest: scope.Digest(), ExpiresAt: time.Now().Add(time.Minute), MaxAge: time.Minute}
		for _, entry := range batch {
			target.Members = append(target.Members, handover.ReceiptMember{NodeUID: entry.Receipt.NodeUID})
		}
		target.Observe = func(ctx context.Context, entries []handover.PodReceipt) error {
			_, err := scope.AuthenticatePartial(ctx, entries, policies, time.Minute)
			return err
		}
		target.Complete = func(ctx context.Context, entries []handover.PodReceipt) error {
			if _, err := scope.AuthenticateComplete(ctx, entries, policies, time.Minute); err != nil {
				return err
			}
			completed.Add(1)
			return nil
		}
		return target, nil
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(handler)
	defer server.Close()
	submit := func(entry handover.PodReceipt, bearer string, expected int) {
		t.Helper()
		entry.BearerToken = ""
		raw, err := json.Marshal(handover.ReceiptRequest{MachineUID: "worker", RoundHash: scope.Digest(), Receipts: []handover.PodReceipt{entry}})
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest(http.MethodPost, server.URL, bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
		response, err := server.Client().Do(req)
		if err != nil {
			t.Fatal("receipt transport request failed")
		}
		response.Body.Close()
		if response.StatusCode != expected {
			t.Fatalf("receipt status %d, want %d", response.StatusCode, expected)
		}
	}
	submit(batch[0], batch[0].BearerToken, http.StatusAccepted)
	if completed.Load() != 0 {
		t.Fatal("partial collection completed")
	}
	submit(batch[1], batch[0].BearerToken, http.StatusConflict)
	// Authentication failure clears the earlier retained credential. The second
	// consumer alone must still be insufficient after the failed submission.
	submit(batch[1], batch[1].BearerToken, http.StatusAccepted)
	if completed.Load() != 0 {
		t.Fatal("failed collection retained authority")
	}
	submit(batch[0], batch[0].BearerToken, http.StatusNoContent)
	if completed.Load() != 1 {
		t.Fatal("independent consumers did not complete exactly once")
	}
}
