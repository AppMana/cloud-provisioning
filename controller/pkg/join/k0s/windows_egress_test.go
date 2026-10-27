package k0s

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func daemonSet(name string) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kube-system"}}
}

// Through Konnectivity, an API server reaches a webhook Service by any
// agent, and a Windows agent cannot reach Services. k0s with the
// native-linux-egress patch (providers/k0s) adds Linux default-route agents;
// without them a Windows worker breaks admission, so it is refused.
func TestWindowsWorkersNeedLinuxDefaultRouteAgents(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		objects []client.Object
		ok      bool
	}{
		{nil, true}, // no Konnectivity: the API server dials directly
		{[]client.Object{daemonSet("konnectivity-agent")}, false},
		{[]client.Object{daemonSet("konnectivity-agent"), daemonSet(DefaultRouteAgent)}, true},
	} {
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tc.objects...).Build()
		err := WindowsEgressReady(context.Background(), c)
		if (err == nil) != tc.ok {
			t.Errorf("%d objects: %v", len(tc.objects), err)
		}
		if err != nil && !strings.Contains(err.Error(), "native-linux-egress") {
			t.Errorf("the refusal does not say what fixes it: %v", err)
		}
	}
}
