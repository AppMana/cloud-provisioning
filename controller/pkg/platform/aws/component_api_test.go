package aws

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/platform"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func siteNode(name, podCIDR string, labels map[string]string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}, Spec: corev1.NodeSpec{PodCIDR: podCIDR, PodCIDRs: []string{podCIDR}}}
}

func setAddress(t *testing.T, c client.Client, name, address string) {
	t.Helper()
	node := &corev1.Node{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: name}, node); err != nil {
		t.Fatal(err)
	}
	node.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: address}}
	if err := c.Status().Update(context.Background(), node); err != nil {
		t.Fatal(err)
	}
}

// Against a real API server: the facts come from the site's own records,
// aws-node is applied by the product's field manager, taking over an
// aws-node installed by hand, and a site node joining updates its SNAT
// exclusions.
func TestTheVPCCNIIsAppliedFromTheSitesFacts(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("requires isolated Kubernetes API binaries")
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
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	for _, n := range []*corev1.Node{
		siteNode("cp", "10.101.128.0/24", nil),
		siteNode("remote", "10.101.140.0/24", map[string]string{platform.RoleLabel: platform.RoleValue}),
	} {
		if err := c.Create(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	setAddress(t, c, "cp", "10.101.0.10")
	setAddress(t, c, "remote", "172.29.0.5")

	// An aws-node someone installed by hand, which the product adopts.
	_, handInstalled, err := VPCCNIObjects(qualifiedFacts())
	if err != nil {
		t.Fatal(err)
	}
	handInstalled.Labels = map[string]string{"app.kubernetes.io/managed-by": "Helm"}
	if err := c.Create(ctx, handInstalled, client.FieldOwner("helm")); err != nil {
		t.Fatal(err)
	}

	// The cluster DNS a Windows node's plugin configuration names.
	if err := c.Create(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "kube-dns", Namespace: "kube-system"},
		Spec: corev1.ServiceSpec{ClusterIP: "10.0.0.10", Ports: []corev1.ServicePort{{Name: "dns", Port: 53, Protocol: corev1.ProtocolUDP}}}}); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "coredns", Namespace: "kube-system"},
		Data: map[string]string{"Corefile": ".:53 {\n    kubernetes cluster.local in-addr.arpa ip6.arpa {\n    }\n}\n"}}); err != nil {
		t.Fatal(err)
	}

	component := &VPCCNIComponent{Client: c, Family: "ipv4", Windows: true}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := component.Register(mgr); err != nil {
		t.Fatal(err)
	}
	go func() { _ = mgr.Start(ctx) }()

	exclusions := func() string {
		ds := &appsv1.DaemonSet{}
		if err := c.Get(ctx, awsNode, ds); err != nil {
			return ""
		}
		if ds.Labels["app.kubernetes.io/managed-by"] != "cloud-provisioning" {
			return ""
		}
		for _, e := range ds.Spec.Template.Spec.Containers[0].Env {
			if e.Name == "AWS_VPC_K8S_CNI_EXCLUDE_SNAT_CIDRS" {
				return e.Value
			}
		}
		return ""
	}
	wait := func(want string) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for exclusions() != want {
			if time.Now().After(deadline) {
				t.Fatalf("exclusions %q, want %q", exclusions(), want)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	// envtest's Service range is 10.0.0.0/24; the cloud node is left out.
	wait("10.0.0.0/24,10.101.0.10/32,10.101.128.0/24")

	if err := c.Create(ctx, siteNode("w1", "10.101.129.0/24", nil)); err != nil {
		t.Fatal(err)
	}
	setAddress(t, c, "w1", "10.101.0.11")
	wait("10.0.0.0/24,10.101.0.10/32,10.101.0.11/32,10.101.128.0/24,10.101.129.0/24")

	ds := &appsv1.DaemonSet{}
	if err := c.Get(ctx, awsNode, ds); err != nil {
		t.Fatal(err)
	}
	var managers []string
	for _, m := range ds.ManagedFields {
		managers = append(managers, m.Manager)
	}
	if !strings.Contains(strings.Join(managers, ","), FieldOwner) {
		t.Fatalf("field managers %v", managers)
	}
	configMap := &corev1.ConfigMap{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: "amazon-vpc-cni"}, configMap); err != nil {
		t.Fatal(err)
	}

	// Windows nodes' plugin configuration, from the same facts.
	if err := c.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: "aws-vpc-cni-windows"}, &appsv1.DaemonSet{}); err != nil {
		t.Fatal(err)
	}
	settings := &corev1.ConfigMap{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: "aws-vpc-cni-windows-settings"}, settings); err != nil {
		t.Fatal(err)
	}
	want := `{"serviceCIDR":"10.0.0.0/24","clusterDNS":"10.0.0.10","clusterDomain":"cluster.local",` +
		`"snatExcludedCIDRs":["10.0.0.0/24","10.101.0.10/32","10.101.0.11/32","10.101.128.0/24","10.101.129.0/24"],` +
		`"hostRouteCIDRs":["10.101.0.10/32","10.101.0.11/32","10.101.128.0/24","10.101.129.0/24"],"podMTU":1420}`
	if got := settings.Data["settings.json"]; got != want {
		t.Fatalf("Windows settings:\n got %s\nwant %s", got, want)
	}
}
