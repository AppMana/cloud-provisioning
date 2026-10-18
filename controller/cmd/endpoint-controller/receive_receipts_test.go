package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestReceiptServerStopsWithLeadership(t *testing.T) {
	identity := httptest.NewTLSServer(nil)
	certificate := identity.TLS.Certificates[0]
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: identity.Certificate().Raw})
	identity.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	admitted := make(chan struct{}, 1)
	handler, _ := handover.NewReceiptHandler(func(context.Context, string) (handover.Store, map[string]handover.PodReceiptAuthenticator, error) {
		admitted <- struct{}{}
		return handover.Store{}, nil, fmt.Errorf("fixture has no journal")
	}, 1)
	server := &receiveReceiptServer{handler: handler}
	if !server.NeedLeaderElection() {
		t.Fatal("receipt server can run outside leader scope")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.serve(ctx, listener, certificate) }()
	client, err := handover.NewReceiptClient("https://"+listener.Addr().String()+"/v1/receipts", ca)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	hash := strings.Repeat("a", 64)
	request := handover.ReceiptRequest{MachineUID: "machine", RoundHash: hash, Receipts: []handover.PodReceipt{{BootID: "boot", Receipt: handover.Receipt{RoundHash: hash, NodeUID: "node"}}}}
	if err := client.Submit(ctx, "fixture-token", request); err == nil {
		t.Fatal("missing journal was accepted")
	}
	select {
	case <-admitted:
	case <-time.After(time.Second):
		t.Fatal("TLS request never reached controller admission")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("server did not stop after leadership ended")
	}
	if err := client.Submit(context.Background(), "fixture-token", request); err == nil {
		t.Fatal("server accepted request after leadership ended")
	}
}

func TestReceiptChartServingConfiguration(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("requires Helm")
	}
	policy := map[string]any{"nativeCIDRs": []string{"192.0.2.0/24"}, "ports": []int{51821}, "journal": map[string]any{"roundLifetime": "5m", "maxReceiptAge": "1m", "drainInterval": "1m", "receipts": receiveReceiptPolicy{BindAddress: ":9444", CertFile: "/etc/cloud-provisioning/receipts/tls.crt", KeyFile: "/etc/cloud-provisioning/receipts/tls.key", ImageIDs: map[string]string{"amd64": "sha256:" + strings.Repeat("a", 64)}}}}
	raw, _ := json.Marshal(policy)
	for _, scenario := range []string{"receipts", "observer", "gateway"} {
		t.Run(scenario, func(t *testing.T) {
			args := []string{"template", "receipt-test", "../../../charts/cloud-provisioning", "--namespace", "receipt-test", "--set-json", "tunnel.receivePreparation=" + string(raw), "--set", "tunnel.receiptTLSSecret=serving-identity"}
			if scenario == "observer" {
				args = append(args, "--set", "awsPodObserver.configMap=scope", "--set", "awsPodObserver.credentialsFileSecret=credentials")
			}
			if scenario == "gateway" {
				args = append(args, "--set", "awsGateway.configMap=scope")
			}
			out, err := exec.Command("helm", args...).CombinedOutput()
			if err != nil {
				t.Fatalf("render: %v: %s", err, out)
			}
			decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096)
			service, deployment, tokenReview := false, false, false
			for {
				object := &unstructured.Unstructured{}
				if err := decoder.Decode(&object.Object); err == io.EOF {
					break
				} else if err != nil {
					t.Fatal(err)
				}
				switch object.GetKind() {
				case "Service":
					var serviceObject corev1.Service
					if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &serviceObject); err != nil {
						t.Fatal(err)
					}
					if len(serviceObject.Spec.Ports) != 1 || serviceObject.Spec.Ports[0].Port != 9444 || serviceObject.Spec.Ports[0].TargetPort.IntVal != 9444 {
						t.Fatalf("incorrect receipt service: %v", serviceObject.Spec.Ports)
					}
					service = true
				case "Deployment":
					var d appsv1.Deployment
					if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &d); err != nil {
						t.Fatal(err)
					}
					mounts := map[string]bool{}
					for _, m := range d.Spec.Template.Spec.Containers[0].VolumeMounts {
						mounts[m.Name] = true
					}
					if !mounts["receipt-tls"] || scenario == "observer" && (!mounts["aws-pod-scope"] || !mounts["aws-pod-credentials"]) || scenario == "gateway" && !mounts["aws-gateway-scope"] {
						t.Fatalf("missing combined volume mounts: %v", mounts)
					}
					deployment = true
				case "ClusterRole":
					rules, _, _ := unstructured.NestedSlice(object.Object, "rules")
					for _, raw := range rules {
						rule := raw.(map[string]any)
						resources, _, _ := unstructured.NestedStringSlice(rule, "resources")
						for _, resource := range resources {
							tokenReview = tokenReview || resource == "tokenreviews"
						}
					}
				}
			}
			if !service || !deployment || !tokenReview {
				t.Fatal("incomplete receipt deployment")
			}
		})
	}
}

func TestReceiptChartAuthorizationRealAPI(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("requires real API assets")
	}
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("requires Helm")
	}
	ctx := context.Background()
	policy := `{"nativeCIDRs":["192.0.2.0/24"],"ports":[51821],"journal":{"roundLifetime":"5m","maxReceiptAge":"1m","drainInterval":"1m","receipts":{"bindAddress":":9444","certFile":"/etc/cloud-provisioning/receipts/tls.crt","keyFile":"/etc/cloud-provisioning/receipts/tls.key","imageIDs":{"amd64":"sha256:` + strings.Repeat("a", 64) + `"}}}}`
	rendered, err := exec.Command("helm", "template", "receipt-test", "../../../charts/cloud-provisioning", "--namespace", "receipt-test", "--set-json", "tunnel.receivePreparation="+policy, "--set", "tunnel.receiptTLSSecret=serving-identity", "--show-only", "templates/rbac.yaml").CombinedOutput()
	if err != nil {
		t.Fatalf("render: %v: %s", err, rendered)
	}
	env := &envtest.Environment{}
	env.ControlPlane.GetAPIServer().Configure().Set("authorization-mode", "RBAC")
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Error(err)
		}
	})
	admin, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "receipt-test"}}); err != nil {
		t.Fatal(err)
	}
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(rendered), 4096)
	for {
		object := &unstructured.Unstructured{}
		if err := decoder.Decode(&object.Object); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if object.GetNamespace() != "" && object.GetNamespace() != "receipt-test" {
			continue
		}
		switch object.GetKind() {
		case "ServiceAccount", "Role", "RoleBinding", "ClusterRole", "ClusterRoleBinding":
			if err := admin.Create(ctx, object); err != nil {
				t.Fatal(err)
			}
		}
	}
	api, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		group, resource, namespace, name, verb string
		allowed                                bool
	}{
		{"authentication.k8s.io", "tokenreviews", "", "", "create", true},
		{"", "pods", "receipt-test", "agent-pod", "get", true},
		{"", "serviceaccounts", "receipt-test", "cloud-provisioning", "get", true},
		{"", "serviceaccounts", "receipt-test", "unrelated", "get", false},
		{"", "pods", "unrelated", "agent-pod", "get", false},
		{"authentication.k8s.io", "tokenreviews", "", "", "list", false},
	} {
		deadline := time.Now().Add(5 * time.Second)
		for {
			review, err := api.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{User: "system:serviceaccount:receipt-test:cloud-provisioning-endpoint-controller", Groups: []string{"system:authenticated", "system:serviceaccounts", "system:serviceaccounts:receipt-test"}, ResourceAttributes: &authorizationv1.ResourceAttributes{Group: test.group, Resource: test.resource, Namespace: test.namespace, Name: test.name, Verb: test.verb}}}, metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if review.Status.Allowed == test.allowed {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("unexpected authorization for %s %s/%s", test.verb, test.namespace, test.resource)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}

func TestReceiveReceiptPolicy(t *testing.T) {
	valid := receiveReceiptPolicy{BindAddress: ":9444", CertFile: "/tls/tls.crt", KeyFile: "/tls/tls.key", ImageIDs: map[string]string{"amd64": "sha256:" + strings.Repeat("a", 64)}}
	if err := valid.validate(); err != nil {
		t.Fatal(err)
	}
	for _, fault := range []string{"address", "certificate", "key", "tag", "architecture", "missing-images"} {
		t.Run(fault, func(t *testing.T) {
			p := valid
			switch fault {
			case "address":
				p.BindAddress = "no-port"
			case "certificate":
				p.CertFile = ""
			case "key":
				p.KeyFile = ""
			case "tag":
				p.ImageIDs = map[string]string{"amd64": "agent:latest"}
			case "architecture":
				p.ImageIDs = map[string]string{"unknown": valid.ImageIDs["amd64"]}
			case "missing-images":
				p.ImageIDs = nil
			}
			if p.validate() == nil {
				t.Fatal("incomplete serving policy accepted")
			}
		})
	}
}

// Reuses the committed controller plan and real API-backed reservation, while
// varying live participant objects without modifying the preparation fixture.
func verifyReceiveReceiptAdmission(t *testing.T, original *meshReconciler, snapshot handover.Snapshot, plan tunnel.ReceiveTransitionPlan) {
	t.Helper()
	ctx := context.Background()
	for _, fault := range []string{"valid", "unreserved", "cloud-command", "node-uid", "provider", "platform", "architecture", "missing-account", "missing-daemon"} {
		t.Run("receipt-admission-"+fault, func(t *testing.T) {
			r := *original
			r.dialerServiceAccount = "receipt-agent"
			r.dialerDaemonSetName = "receipt-site"
			r.dialerCloudDaemonSetName = "receipt-cloud"
			r.dialerImage = "example.invalid/agent@sha256:" + strings.Repeat("a", 64)
			policy := receiveReceiptPolicy{ImageIDs: map[string]string{"amd64": "sha256:" + strings.Repeat("b", 64)}}
			objects := []runtime.Object{}
			if fault != "missing-account" {
				objects = append(objects, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: r.secretNamespace, Name: r.dialerServiceAccount, UID: "account"}})
			}
			for _, name := range []string{r.dialerDaemonSetName, r.dialerCloudDaemonSetName} {
				if fault == "missing-daemon" && name == r.dialerCloudDaemonSetName {
					continue
				}
				objects = append(objects, &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: r.secretNamespace, Name: name, UID: "daemon"}})
			}
			names := []string{plan.CloudNodeName}
			for _, site := range plan.Sites {
				names = append(names, site.NodeName)
			}
			for _, name := range names {
				node := &corev1.Node{}
				if err := r.reader.Get(ctx, client.ObjectKey{Name: name}, node); err != nil {
					t.Fatal(err)
				}
				node.Status.NodeInfo.OperatingSystem = "linux"
				node.Status.NodeInfo.Architecture = "amd64"
				if name == plan.CloudNodeName {
					switch fault {
					case "node-uid":
						node.UID = "replacement"
					case "provider":
						node.Spec.ProviderID = "other-instance"
					case "platform":
						node.Status.NodeInfo.OperatingSystem = "windows"
					case "architecture":
						node.Status.NodeInfo.Architecture = "arm64"
					}
				}
				objects = append(objects, node)
			}
			uid := plan.MachineUID
			if fault == "unreserved" {
				uid = "unreserved"
			}
			if fault == "cloud-command" {
				r.dialerCloudImage = "fallback-image"
			}
			api := fake.NewClientset(objects...)
			store, policies, err := r.resolveReceiveReceipts(ctx, api, policy, uid)
			if fault != "valid" {
				if err == nil {
					t.Fatal("unsafe admission accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if store.ExpectedUID != string(snapshot.Metadata().UID) || store.Name != tunnel.ReceiveTransitionName(uid) || len(policies) != len(names) {
				t.Fatal("admission lost journal or participant scope")
			}
			for uid, p := range policies {
				expected := r.dialerDaemonSetName
				if uid == plan.CloudNodeUID {
					expected = r.dialerCloudDaemonSetName
				}
				if p.DaemonSetName != expected || p.ServiceAccountUID != "account" || p.Image != r.dialerImage || p.ImageID != policy.ImageIDs["amd64"] || p.Audience != receiveReceiptAudience {
					t.Fatal("policy did not use controller-owned release identity")
				}
			}
		})
	}
}

// Invalid release configuration must fail before API discovery or journal
// publication. This checks configuration only, not live image availability or
// authenticated phase completion; an existing transition still needs recovery.
func TestReceiptAgentReleaseValidation(t *testing.T) {
	p := &receivePreparationPolicy{Journal: &receiveJournalPolicy{Receipts: &receiveReceiptPolicy{ImageIDs: map[string]string{"amd64": "sha256:" + strings.Repeat("a", 64), "arm64": "sha256:" + strings.Repeat("b", 64)}}}}
	pinned := "example.invalid/dialer@sha256:" + strings.Repeat("c", 64)
	for _, image := range []string{"", "example.invalid/dialer:revision", "sha256:" + strings.Repeat("c", 64), "example.invalid/dialer@sha256:abc", pinned + "\n"} {
		if err := validateReceiptAgentRelease(p, image, ""); err == nil {
			t.Fatalf("mutable or malformed agent release accepted: %q", image)
		}
	}
	if err := validateReceiptAgentRelease(p, pinned, ""); err != nil {
		t.Fatal(err)
	}
	if err := validateReceiptAgentRelease(p, pinned, "public-base:tag"); err == nil {
		t.Fatal("host-binary entrypoint accepted for receipt agents")
	}
	if err := validateReceiptAgentRelease(nil, "local-dev:tag", ""); err != nil {
		t.Fatal("receipt-free development configuration rejected", err)
	}
}

// This checks the generated idle cadence, not native execution or API latency.
// Slow reconciliations can still miss freshness or round deadlines; those must
// remain failures rather than be accepted through older receipts.
func TestReceiptAgentRefreshesBeforeFreshnessDeadline(t *testing.T) {
	for _, age := range []time.Duration{30 * time.Second, time.Minute, 5 * time.Minute} {
		r := &meshReconciler{receivePreparation: &receivePreparationPolicy{Journal: &receiveJournalPolicy{MaxReceiptAge: metav1.Duration{Duration: age}, Receipts: &receiveReceiptPolicy{Endpoint: "https://receipts.invalid/v1/receipts", TrustSecret: "trust"}}}}
		daemon := &appsv1.DaemonSet{Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dialer", Args: []string{"--poll-interval=30s", "--iface=test"}}}}}}}
		r.configureReceiptAgent(daemon)
		count := 0
		for _, arg := range daemon.Spec.Template.Spec.Containers[0].Args {
			if value, ok := strings.CutPrefix(arg, "--poll-interval="); ok {
				count++
				interval, err := time.ParseDuration(value)
				if err != nil || interval <= 0 || interval > 30*time.Second || interval*3 > age {
					t.Fatalf("receipt age %v has unsuitable refresh interval %q", age, value)
				}
			}
		}
		if count != 1 {
			t.Fatalf("poll interval appears %d times", count)
		}
	}
}
