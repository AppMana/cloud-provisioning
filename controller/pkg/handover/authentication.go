package handover

import (
	"context"
	"fmt"
	"slices"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// PodReceiptAuthenticator verifies a directly presented bearer credential.
// Its policy comes from controller-owned configuration, never the request.
// Call only behind authenticated server TLS; never persist tokens alongside
// receipts or accept them from shared Secrets. Phase admission must separately
// validate the committed round, native observations and current boot identity.
type PodReceiptAuthenticator struct {
	API                kubernetes.Interface
	Audience           string
	Namespace          string
	ServiceAccountName string
	ServiceAccountUID  string
	DaemonSetName      string
	DaemonSetUID       string
}

// Authenticate binds a receipt to a live, Ready daemon pod and its current
// Node UID and boot. Service account identity alone is shared across nodes.
// Kubernetes does not validate the Node of a pod-bound token, so all four
// bound pod/node claims are required and checked against live API objects.
// No verification result is cached across requests or controller restarts.
func (a PodReceiptAuthenticator) Authenticate(ctx context.Context, bearer string, receipt Receipt, bootID string) (AuthenticatedReceipt, error) {
	fail := func(message string) (AuthenticatedReceipt, error) {
		return AuthenticatedReceipt{}, fmt.Errorf("receipt authentication: %s", message)
	}
	if a.API == nil || a.Audience == "" || a.Namespace == "" || a.ServiceAccountName == "" || a.ServiceAccountUID == "" || a.DaemonSetName == "" || a.DaemonSetUID == "" || bearer == "" || receipt.NodeUID == "" || bootID == "" {
		return fail("missing policy, credential or identity")
	}
	review, err := a.API.AuthenticationV1().TokenReviews().Create(ctx, &authenticationv1.TokenReview{Spec: authenticationv1.TokenReviewSpec{Token: bearer, Audiences: []string{a.Audience}}}, metav1.CreateOptions{})
	// Do not return arbitrary server errors or review bodies: those may contain
	// the submitted bearer credential. Authentication failure is fail-closed.
	if err != nil || review == nil || !review.Status.Authenticated || review.Status.Error != "" || !slices.Contains(review.Status.Audiences, a.Audience) {
		return fail("token review failed")
	}
	user := review.Status.User
	if user.Username != "system:serviceaccount:"+a.Namespace+":"+a.ServiceAccountName || user.UID != a.ServiceAccountUID {
		return fail("service account identity differs")
	}
	claim := func(name string) string {
		values := user.Extra["authentication.kubernetes.io/"+name]
		if len(values) != 1 {
			return ""
		}
		return values[0]
	}
	podName, podUID := claim("pod-name"), claim("pod-uid")
	nodeName, nodeUID := claim("node-name"), claim("node-uid")
	if podName == "" || podUID == "" || nodeName == "" || nodeUID == "" || receipt.NodeUID != nodeUID {
		return fail("pod-bound node identity differs or is missing")
	}
	sa, err := a.API.CoreV1().ServiceAccounts(a.Namespace).Get(ctx, a.ServiceAccountName, metav1.GetOptions{})
	if err != nil || string(sa.UID) != a.ServiceAccountUID || sa.DeletionTimestamp != nil {
		return fail("service account is no longer current")
	}
	pod, err := a.API.CoreV1().Pods(a.Namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil || string(pod.UID) != podUID || pod.DeletionTimestamp != nil || pod.Spec.ServiceAccountName != a.ServiceAccountName || pod.Spec.NodeName != nodeName || pod.Status.Phase != corev1.PodRunning {
		return fail("pod identity or placement is no longer current")
	}
	owner := metav1.GetControllerOf(pod)
	if owner == nil || owner.APIVersion != "apps/v1" || owner.Kind != "DaemonSet" || owner.Name != a.DaemonSetName || string(owner.UID) != a.DaemonSetUID {
		return fail("pod is not owned by the configured daemon")
	}
	daemon, err := a.API.AppsV1().DaemonSets(a.Namespace).Get(ctx, a.DaemonSetName, metav1.GetOptions{})
	if err != nil || string(daemon.UID) != a.DaemonSetUID || daemon.DeletionTimestamp != nil {
		return fail("daemon identity is no longer current")
	}
	node, err := a.API.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil || string(node.UID) != nodeUID || node.DeletionTimestamp != nil || node.Status.NodeInfo.BootID != bootID {
		return fail("node identity or boot is no longer current")
	}
	podReady, nodeReady := false, false
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			podReady = condition.Status == corev1.ConditionTrue
		}
	}
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			nodeReady = condition.Status == corev1.ConditionTrue
		}
	}
	if !podReady || !nodeReady {
		return fail("pod or node is not Ready")
	}
	return AuthenticatedReceipt{AuthenticatedNodeUID: nodeUID, Receipt: receipt}, nil
}
