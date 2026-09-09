package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	provider "github.com/appmana/cloud-provisioning/controller/pkg/attachment/aws"
	sdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

type noCloudHTTP struct{ calls atomic.Int32 }

func (h *noCloudHTTP) Do(*http.Request) (*http.Response, error) {
	h.calls.Add(1)
	return nil, fmt.Errorf("test forbids cloud calls")
}

// This runs the registered controller, real cache/watch, API persistence and
// finalizer deletion. Missing CAPI identity must block before any AWS operation.
func TestAPIRegisteredAWSRequestRetirementAndNameReuse(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("set KUBEBUILDER_ASSETS")
	}
	preserve := true
	env := &envtest.Environment{CRDInstallOptions: envtest.CRDInstallOptions{CRDs: []*apiextv1.CustomResourceDefinition{{
		ObjectMeta: metav1.ObjectMeta{Name: "machines.cluster.x-k8s.io"},
		Spec: apiextv1.CustomResourceDefinitionSpec{Group: "cluster.x-k8s.io", Scope: apiextv1.NamespaceScoped,
			Names:    apiextv1.CustomResourceDefinitionNames{Plural: "machines", Singular: "machine", Kind: "Machine", ListKind: "MachineList"},
			Versions: []apiextv1.CustomResourceDefinitionVersion{{Name: "v1beta2", Served: true, Storage: true, Schema: &apiextv1.CustomResourceValidation{OpenAPIV3Schema: &apiextv1.JSONSchemaProps{Type: "object", XPreserveUnknownFields: &preserve}}}},
		},
	}}}}
	config, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := env.Stop(); err != nil {
			t.Error(err)
		}
	}()
	c, err := client.New(config, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err = c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test"}}); err != nil {
		t.Fatal(err)
	}
	mgr, err := ctrl.NewManager(config, ctrl.Options{Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0", LeaderElection: true, LeaderElectionNamespace: "test", LeaderElectionID: "gateway-runtime-test"})
	if err != nil {
		t.Fatal(err)
	}
	httpClient := &noCloudHTTP{}
	cfg := AWSConfig{Scope: provider.Scope{Account: "account", Region: "us-west-2", VPCID: "vpc", SubnetID: "subnet", RouteTableID: "rtb", OwnerTag: "run", OwnerValue: "test"}, ClusterName: "cluster", GroupID: "sg", MeshUID: "mesh-uid", NativeInterface: "ens5", TunnelInterface: "cldt1775f69e"}
	if err = RegisterAWS(mgr, cfg, sdk.Config{Region: "us-west-2", Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""), HTTPClient: httpClient, RetryMaxAttempts: 1}, "test", "mesh", "10.100.0.1", "6443"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("manager did not stop")
		}
	}()
	fixture, _, req := requestFixture(t)
	original := &corev1.ConfigMap{}
	if err = fixture.Client.Get(ctx, req.NamespacedName, original); err != nil {
		t.Fatal(err)
	}
	wait := func(check func() bool) {
		t.Helper()
		for !check() {
			select {
			case <-ctx.Done():
				t.Fatal("controller observation timed out")
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	ids := []string{}
	var previousUID types.UID
	for range 2 {
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: original.Name, Namespace: original.Namespace, Labels: original.Labels}, Data: original.Data}
		if err = c.Create(ctx, cm); err != nil {
			t.Fatal(err)
		}
		id := cm.Namespace + "/" + cm.Name + "/" + string(cm.UID)
		ids = append(ids, id)
		store := attachment.ConfigMapStore{Client: c, Namespace: "test"}
		wait(func() bool {
			record, e := store.Load(ctx, id)
			return e == nil && record != nil && record.Phase == attachment.Preparing
		})
		// Preparation cannot pass CAPI/EC2 validation using made-up identities.
		if httpClient.calls.Load() != 0 {
			t.Fatal("cloud call before identity validation")
		}
		active, e := store.Load(ctx, id)
		if e != nil || active == nil {
			t.Fatal("missing retirement intent", e)
		}
		if previousUID != "" {
			if _, e := RetireWorkerRequest(ctx, c, cm.Namespace, "mesh", cm.Name, previousUID, active.Plan.Worker); e == nil {
				t.Fatal("old request UID retired same-name replacement")
			}
		}
		wait(func() bool {
			done, e := RetireWorkerRequest(ctx, c, cm.Namespace, "mesh", cm.Name, cm.UID, active.Plan.Worker)
			if apierrors.IsConflict(e) {
				return false
			}
			if e != nil {
				t.Fatal(e)
			}
			return done
		})
		if !apierrors.IsNotFound(c.Get(ctx, client.ObjectKeyFromObject(cm), &corev1.ConfigMap{})) {
			t.Fatal("retirement completed while request still existed")
		}
		previousUID = cm.UID
		record, e := store.Load(ctx, id)
		if e != nil || record == nil || record.Phase != attachment.Complete {
			t.Fatalf("retirement record: %v %v", record, e)
		}
	}
	if ids[0] == ids[1] {
		t.Fatal("request name reuse retained old lease identity")
	}
	cms := &corev1.ConfigMapList{}
	if err = c.List(ctx, cms, client.InNamespace("test")); err != nil {
		t.Fatal(err)
	}
	for _, cm := range cms.Items {
		if strings.HasPrefix(cm.Name, "network-attachment-") {
			var record attachment.Record
			if err = json.Unmarshal([]byte(cm.Data["record.json"]), &record); err != nil {
				t.Fatal(err)
			}
			if record.Phase != attachment.Complete || len(cm.Finalizers) != 0 {
				t.Fatal("retired intent still finalized")
			}
		}
	}
	if httpClient.calls.Load() != 0 {
		t.Fatal("unexpected cloud request")
	}
	checkAPILifetime(t, ctx, c)
}

func checkAPILifetime(t *testing.T, ctx context.Context, c client.Client) {
	t.Helper()
	// Outside the registered controller's namespace, exercise the lifetime
	// capability against actual API UIDs, finalizers and optimistic patches.
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "lifetime"}}); err != nil {
		t.Fatal(err)
	}
	request := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "request", Namespace: "lifetime", Finalizers: []string{requestFinalizer}, Annotations: map[string]string{requestMeshOwner: "mesh"}}}
	if err := c.Create(ctx, request); err != nil {
		t.Fatal(err)
	}
	participants := []attachment.Machine{}
	objects := []*unstructured.Unstructured{}
	for _, name := range []string{"lifetime-worker", "lifetime-gateway"} {
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: corev1.NodeSpec{ProviderID: "test://" + name}}
		if err := c.Create(ctx, node); err != nil {
			t.Fatal(err)
		}
		machine := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "cluster.x-k8s.io/v1beta2", "kind": "Machine", "metadata": map[string]any{"name": name, "namespace": "lifetime", "finalizers": []any{"test/keep"}}, "spec": map[string]any{"clusterName": "cluster", "providerID": node.Spec.ProviderID}, "status": map[string]any{"nodeRef": map[string]any{"name": name}}}}
		if err := c.Create(ctx, machine); err != nil {
			t.Fatal(err)
		}
		objects = append(objects, machine)
		participants = append(participants, attachment.Machine{UID: string(machine.GetUID()), NodeUID: string(node.UID), ProviderID: node.Spec.ProviderID})
	}
	lifetime := CAPILifetime{Client: c, Namespace: "lifetime", ClusterName: "cluster", MeshName: "mesh"}
	record := attachment.Record{ID: "lifetime/request/" + string(request.UID), Lease: "lease", Digest: "digest", Plan: attachment.GatewayPlan{Worker: participants[0], Gateway: participants[1]}}
	if retiring, err := lifetime.Protect(ctx, record); err != nil || retiring {
		t.Fatal(retiring, err)
	}
	if err := c.Delete(ctx, objects[0]); err != nil {
		t.Fatal(err)
	}
	if retiring, err := lifetime.Protect(ctx, record); err != nil || !retiring {
		t.Fatal(retiring, err)
	}
	current, err := lifetime.request(ctx, record)
	if err != nil || current.DeletionTimestamp == nil {
		t.Fatal("API request did not retain withdrawal finalizer", err)
	}
	machines, err := lifetime.machines(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, machine := range machines {
		if !attachment.HasDeletionHold(machine) {
			t.Fatal("API lost lifetime hooks")
		}
	}
	if err := lifetime.Release(ctx, record); err != nil {
		t.Fatal(err)
	}
	machines, err = lifetime.machines(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, machine := range machines {
		if attachment.HasDeletionHold(machine) {
			t.Fatal("API hook release failed")
		}
	}
}
