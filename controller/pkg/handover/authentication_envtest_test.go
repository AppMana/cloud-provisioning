package handover

import (
	"context"
	"os"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestPodReceiptAuthenticationRealAPI(t *testing.T) {
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
	daemon, err := api.AppsV1().DaemonSets(ns).Create(ctx, &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "dialer"}, Spec: appsv1.DaemonSetSpec{Selector: &metav1.LabelSelector{MatchLabels: labels}, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{ServiceAccountName: sa.Name, Containers: []corev1.Container{{Name: "dialer", Image: "example.invalid/dialer"}}}}}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	verifier := PodReceiptAuthenticator{API: api, Audience: "cloud-provisioning-receipts", Namespace: ns, ServiceAccountName: sa.Name, ServiceAccountUID: string(sa.UID), DaemonSetName: daemon.Name, DaemonSetUID: string(daemon.UID)}
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
		pod, err := api.CoreV1().Pods(ns).Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(daemon, appsv1.SchemeGroupVersion.WithKind("DaemonSet"))}}, Spec: corev1.PodSpec{NodeName: node.Name, ServiceAccountName: sa.Name, Containers: []corev1.Container{{Name: "dialer", Image: "example.invalid/dialer"}}}}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		pod.Status.Phase = corev1.PodRunning
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
	bearer := mint(pods[0], verifier.Audience)
	receipt := Receipt{NodeUID: string(nodes[0].UID)}
	boot := nodes[0].Status.NodeInfo.BootID
	proof, err := verifier.Authenticate(ctx, bearer, receipt, boot)
	if err != nil || proof.AuthenticatedNodeUID != receipt.NodeUID || proof.Receipt != receipt {
		t.Fatal("valid pod credential did not authenticate its node", err)
	}
	reject := func(name, token string, policy PodReceiptAuthenticator, candidate Receipt, observedBoot string) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			proof, err := policy.Authenticate(ctx, token, candidate, observedBoot)
			if err == nil || proof != (AuthenticatedReceipt{}) {
				t.Fatal("unsafe receipt authenticated")
			}
			if token != "" && strings.Contains(err.Error(), token) {
				t.Fatal("authentication error exposed bearer credential")
			}
		})
	}
	otherBearer := mint(pods[1], verifier.Audience)
	if _, err := verifier.Authenticate(ctx, otherBearer, Receipt{NodeUID: string(nodes[1].UID)}, nodes[1].Status.NodeInfo.BootID); err != nil {
		t.Fatal("second daemon could not authenticate its own node", err)
	}
	verifyAuthenticatedAdvancement(t, verifier, nodes, []string{bearer, otherBearer})
	reject("another-node-same-account", otherBearer, verifier, receipt, boot)
	reject("unbound-account-token", mint(nil, verifier.Audience), verifier, receipt, boot)
	reject("wrong-audience", mint(pods[0], "another-audience"), verifier, receipt, boot)
	reject("invalid-token", "not-a-valid-credential", verifier, receipt, boot)
	reject("old-boot", bearer, verifier, receipt, "previous-boot")
	wrong := verifier
	wrong.ServiceAccountUID = "replacement-account"
	reject("account-replacement", bearer, wrong, receipt, boot)
	wrong = verifier
	wrong.DaemonSetUID = "replacement-daemon"
	reject("daemon-replacement", bearer, wrong, receipt, boot)
	pods[0].Status.Conditions[0].Status = corev1.ConditionFalse
	pods[0], err = api.CoreV1().Pods(ns).UpdateStatus(ctx, pods[0], metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	reject("pod-not-ready", bearer, verifier, receipt, boot)
	pods[0].Status.Conditions[0].Status = corev1.ConditionTrue
	pods[0], err = api.CoreV1().Pods(ns).UpdateStatus(ctx, pods[0], metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	nodes[0].Status.Conditions[0].Status = corev1.ConditionFalse
	nodes[0], err = api.CoreV1().Nodes().UpdateStatus(ctx, nodes[0], metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	reject("node-not-ready", bearer, verifier, receipt, boot)
	nodes[0].Status.Conditions[0].Status = corev1.ConditionTrue
	nodes[0], err = api.CoreV1().Nodes().UpdateStatus(ctx, nodes[0], metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The API may still authenticate a pod-bound token after Node deletion.
	// Recreating the same name must never grant the old credential its new UID.
	if err := api.CoreV1().Nodes().Delete(ctx, nodes[0].Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	reject("deleted-node", bearer, verifier, receipt, boot)
	replacement, err := api.CoreV1().Nodes().Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodes[0].Name}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	replacement.Status = nodes[0].Status
	_, err = api.CoreV1().Nodes().UpdateStatus(ctx, replacement, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	reject("node-name-reuse", bearer, verifier, receipt, boot)
	replacementReceipt := receipt
	replacementReceipt.NodeUID = string(replacement.UID)
	reject("forged-replacement-uid", bearer, verifier, replacementReceipt, boot)
	zero := int64(0)
	if err := api.CoreV1().Pods(ns).Delete(ctx, pods[1].Name, metav1.DeleteOptions{GracePeriodSeconds: &zero}); err != nil {
		t.Fatal(err)
	}
	// Use a token minted before deletion, even while the API's deletion grace
	// might still accept it. The live object check must reject immediately.
	reject("missing-pod", otherBearer, verifier, Receipt{NodeUID: string(nodes[1].UID)}, nodes[1].Status.NodeInfo.BootID)
}
