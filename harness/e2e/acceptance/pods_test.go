package acceptance

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// The matrix owns exactly one namespace: it creates it labelled, puts a
// probe pod and dual-stack Service per node in it, and deletes only a
// namespace carrying that label. A name collision with a namespace the
// cluster already has is refused at creation rather than adopted.
func TestTheMatrixOwnsOnlyItsOwnNamespace(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "workloads"}})
	p := &Pods{Client: client, Namespace: "cloud-provisioning-acceptance-1", PullPolicy: corev1.PullIfNotPresent}
	if _, err := p.Start(ctx, []string{"controller", "worker-1"}, 10*time.Millisecond); err == nil {
		t.Fatal("fake pods never become ready, yet Start returned targets")
	}
	ns, err := client.CoreV1().Namespaces().Get(ctx, p.Namespace, metav1.GetOptions{})
	if err != nil || ns.Labels[NamespaceLabel] != "true" {
		t.Fatalf("probe namespace = %+v, %v", ns, err)
	}
	pods, _ := client.CoreV1().Pods(p.Namespace).List(ctx, metav1.ListOptions{})
	services, _ := client.CoreV1().Services(p.Namespace).List(ctx, metav1.ListOptions{})
	if len(pods.Items) != 2 || len(services.Items) != 2 {
		t.Fatalf("%d pods and %d Services, want one of each per node", len(pods.Items), len(services.Items))
	}
	for _, pod := range pods.Items {
		if pod.Spec.Containers[0].ImagePullPolicy != corev1.PullIfNotPresent {
			t.Errorf("%s pull policy = %s", pod.Name, pod.Spec.Containers[0].ImagePullPolicy)
		}
	}
	if err := p.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().Namespaces().Get(ctx, p.Namespace, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the probe namespace survived Stop: %v", err)
	}

	foreign := &Pods{Client: client, Namespace: "workloads"}
	if _, err := foreign.Start(ctx, []string{"controller"}, time.Millisecond); err == nil {
		t.Error("an existing namespace was adopted")
	}
	if err := foreign.Stop(ctx); err == nil {
		t.Error("Stop deleted a namespace this command did not create")
	}
	if _, err := client.CoreV1().Namespaces().Get(ctx, "workloads", metav1.GetOptions{}); err != nil {
		t.Errorf("the cluster's own namespace is gone: %v", err)
	}
}
