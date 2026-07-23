package join

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	joinaws "github.com/appmana/cloud-provisioning/controller/pkg/join/aws"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// secretValue reads key from a Secret's Data (real API server
// behavior: StringData is write-only, converted to Data on write) or
// falls back to StringData (this fake client's behavior: preserves
// StringData across Get, never populates Data) -- correct regardless
// of which backend a test happens to run against.
func secretValue(s *corev1.Secret, key string) string {
	if v, ok := s.Data[key]; ok {
		return string(v)
	}
	return s.StringData[key]
}

func dialerPeerSecretFixture() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "wg-dialer-peer", Namespace: "wg-dialer"},
		Data: map[string][]byte{
			"dialer-private-key-jarvis":     []byte("8ALeO136hzNidiHffMf40dM/SKi7iJHBvfy+mplpKVY="),
			"local-address-jarvis":          []byte("10.100.0.1/24"),
			"cluster-vip-jarvis":            []byte("10.101.0.1"),
			"dialer-private-key-spark-2ab3": []byte("6Opkhqk2sKOCnwLzGkjvk+4SxB8fuDH0XreH3RefOUo="),
			"local-address-spark-2ab3":      []byte("10.100.0.3/24"),
			"cluster-vip-spark-2ab3":        []byte("10.101.0.2"),
			"peer-public-key":               []byte("unrelated"), // must NOT be treated as a node peer
			"peer-endpoint":                 []byte("unrelated"),
		},
	}
}

func TestOnPremPeers(t *testing.T) {
	peers, err := onPremPeers(dialerPeerSecretFixture())
	if err != nil {
		t.Fatalf("onPremPeers: %v", err)
	}
	if len(peers) != 2 {
		t.Fatalf("expected exactly 2 peers (not the peer-public-key/peer-endpoint keys treated as nodes), got %d: %+v", len(peers), peers)
	}
	// jarvis's derived public key, confirmed against the real value
	// used this session (private key above is the real jarvis dialer key).
	const jarvisPub = "WpxHfxQmbN8EZZVHeFVhV5uhLWhw2vbY0Rgb0Wnn5Sg="
	found := false
	for _, p := range peers {
		if p.PublicKey == jarvisPub {
			found = true
			want := []string{"10.100.0.1/32", "10.101.0.1/32"}
			if !equalStrings(p.WGAllowedIPs, want) {
				t.Errorf("jarvis WGAllowedIPs = %v, want both tunnel address AND cluster VIP: %v", p.WGAllowedIPs, want)
			}
			if p.RouteHost != "10.100.0.1" {
				t.Errorf("jarvis RouteHost = %q, want just the tunnel address, no cluster VIPs", p.RouteHost)
			}
		}
	}
	if !found {
		t.Errorf("jarvis's derived public key %s not found in peer list", jarvisPub)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestOnPremPeers_DualStackClusterVIPsGetCorrectPrefixLengths(t *testing.T) {
	// Every real node in this cluster is genuinely dual-stack (confirmed
	// against live Node.status.addresses: each has both a 10.101.0.x and
	// an fd8f:cf26:522a::x InternalIP), so cluster-vip-<node> must be
	// able to carry both, and each must get the correct single-host
	// prefix length for its family -- /32 for IPv4, /128 for IPv6.
	// Hardcoding /32 across the board either fails to parse against an
	// IPv6 literal or silently matches the wrong host count.
	secret := dialerPeerSecretFixture()
	secret.Data["cluster-vip-jarvis"] = []byte("10.101.0.1,fd8f:cf26:522a::1")

	peers, err := onPremPeers(secret)
	if err != nil {
		t.Fatalf("onPremPeers: %v", err)
	}
	const jarvisPub = "WpxHfxQmbN8EZZVHeFVhV5uhLWhw2vbY0Rgb0Wnn5Sg="
	for _, p := range peers {
		if p.PublicKey != jarvisPub {
			continue
		}
		want := []string{"10.100.0.1/32", "10.101.0.1/32", "fd8f:cf26:522a::1/128"}
		if !equalStrings(p.WGAllowedIPs, want) {
			t.Errorf("jarvis WGAllowedIPs = %v, want %v", p.WGAllowedIPs, want)
		}
		return
	}
	t.Fatal("jarvis not found in peer list")
}

func TestOnPremPeers_MissingClusterVIP(t *testing.T) {
	secret := dialerPeerSecretFixture()
	delete(secret.Data, "cluster-vip-jarvis")
	if _, err := onPremPeers(secret); err == nil {
		t.Fatal("expected an error when cluster-vip-<node> is missing, got nil -- this must fail loudly, not silently omit the BGP-required VIP")
	}
}

func TestOnPremPeers_MissingLocalAddress(t *testing.T) {
	secret := dialerPeerSecretFixture()
	delete(secret.Data, "local-address-jarvis")
	if _, err := onPremPeers(secret); err == nil {
		t.Fatal("expected an error when local-address-<node> is missing, got nil")
	}
}

func machineListGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "MachineList"}
}

func fakeMachine(name string, nodeVIPAnnotation string) *unstructured.Unstructured {
	m := &unstructured.Unstructured{}
	m.SetGroupVersionKind(schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Machine"})
	m.SetName(name)
	m.SetNamespace("default")
	if nodeVIPAnnotation != "" {
		m.SetAnnotations(map[string]string{NodeVIPAnnotation: nodeVIPAnnotation})
	}
	return m
}

func newFakeReconciler(t *testing.T, objs ...client.Object) *Reconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding clientgoscheme: %v", err)
	}
	// Machine/MachineList are only ever used as unstructured here (no
	// generated Go types for CAPI's Machine in this module) -- register
	// them with the scheme as unstructured so the fake client's List()
	// knows what GVK a MachineList corresponds to.
	scheme.AddKnownTypeWithName(machineGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(machineListGVK(), &unstructured.UnstructuredList{})
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &Reconciler{Client: c, Reader: c, NodeVIPStart: 4}
}

func TestAllocateNodeVIPIndex_NoExisting(t *testing.T) {
	r := newFakeReconciler(t)
	idx, err := r.allocateNodeVIPIndex(context.Background(), nil)
	if err != nil {
		t.Fatalf("allocateNodeVIPIndex: %v", err)
	}
	if idx != 4 {
		t.Errorf("expected first allocation to start at NodeVIPStart=4, got %d", idx)
	}
}

// stubJoinProvider is a mock ClusterJoinProvider -- these Reconcile-level
// tests care about what the reconciler DOES with a JoinProvider's
// output (render it, create the Secret), not about any real cluster
// technology's token-minting logic (that's k0s_test.go's job).
type stubJoinProvider struct {
	values map[string]any
	err    error
	calls  int
}

func (s *stubJoinProvider) JoinValues(ctx context.Context) (map[string]any, error) {
	s.calls++
	return s.values, s.err
}

func fakeAWSMachine(name, namespace string, ready bool) *unstructured.Unstructured {
	m := &unstructured.Unstructured{}
	m.SetGroupVersionKind(joinaws.Provider{}.GVK())
	m.SetName(name)
	m.SetNamespace(namespace)
	_ = unstructured.SetNestedField(m.Object, ready, "status", "ready")
	return m
}

// machineWithInfraRef builds a Machine whose infrastructureRef.kind is
// "AWSMachine" -- this is the real, live signal (see fixed
// infrastructureRef: {"apiGroup":"...","kind":"AWSMachine","name":"..."}
// confirmed via kubectl against the actual jarvistam cloud-worker
// Machine) the reconciler uses to infer which registered InfraProvider
// applies, never a hardcoded assumption.
func machineWithInfraRef(name, namespace, infraRefName string) *unstructured.Unstructured {
	m := fakeMachine(name, "")
	m.SetNamespace(namespace)
	_ = unstructured.SetNestedField(m.Object, infraRefName, "spec", "infrastructureRef", "name")
	_ = unstructured.SetNestedField(m.Object, joinaws.Provider{}.GVK().Kind, "spec", "infrastructureRef", "kind")
	return m
}

// newFakeJoinReconciler builds a Reconciler wired for a full
// Reconcile() call: unlike newFakeReconciler (onPremPeers/
// allocateNodeVIPIndex tests only), this registers AWSMachine too and
// fills in every field Reconcile actually reads.
func newFakeJoinReconciler(t *testing.T, joinProvider ClusterJoinProvider, objs ...client.Object) *Reconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding clientgoscheme: %v", err)
	}
	scheme.AddKnownTypeWithName(machineGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(machineListGVK(), &unstructured.UnstructuredList{})
	scheme.AddKnownTypeWithName(joinaws.Provider{}.GVK(), &unstructured.Unstructured{})
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()

	tmplPath := filepath.Join(t.TempDir(), "test.tmpl")
	tmpl := "joinToken={{.joinToken}} k0sVersion={{.k0sVersion}} apiVIP={{.apiVIP}} nodeVIP4={{.nodeVIP4}} nodeVIP6={{.nodeVIP6}} kubeletExtraArgs={{.kubeletExtraArgs}} wgAddress={{.wireguardAddress}} podCIDRs={{.podCIDRs}} serviceCIDRs={{.serviceCIDRs}} peersFileJSON={{.peersFileJSON}}"
	if err := os.WriteFile(tmplPath, []byte(tmpl), 0o644); err != nil {
		t.Fatalf("writing test template: %v", err)
	}

	return &Reconciler{
		Client:         c,
		Reader:         c,
		Join:           joinProvider,
		InfraProviders: []InfraProvider{joinaws.Provider{}},

		TemplatePath:     tmplPath,
		APIVIP:           "10.101.0.1",
		KubeletExtraArgs: "--node-labels=cloud-provisioning.appmana.com/role=cloud-worker",

		WireGuardAddress:    "10.100.0.2/24",
		WireGuardListenPort: "51820",

		NodeVIP4Prefix: "10.101.0.",
		NodeVIP6Prefix: "fd8f:cf26:522a::",
		NodeVIPStart:   4,

		DialerPeerSecretNamespace: "wg-dialer",
		DialerPeerSecretName:      "wg-dialer-peer",

		BootstrapSecretNameFormat: "%s-bootstrap",
	}
}

func TestReconcile_CreatesBootstrapSecretEvenWhenInfraNotReady(t *testing.T) {
	// Regression test for a genuine deadlock caught live against the
	// real hilton cluster: CAPA's AWSMachine controller refuses to call
	// RunInstances until this bootstrap Secret already exists (the
	// Secret IS the cloud-init user-data the instance boots from), so
	// gating its creation on the AWSMachine being "ready" (instance
	// already running) can never succeed -- ready never becomes true
	// without the Secret, and the Secret never gets created while
	// waiting for ready. Bootstrap-secret creation must proceed as soon
	// as the infrastructureRef target object merely exists.
	machine := machineWithInfraRef("hilton-cloud-worker-jarvistam-0", "default", "hilton-cloud-worker-jarvistam-0")
	awsMachine := fakeAWSMachine("hilton-cloud-worker-jarvistam-0", "default", false)
	dialerSecret := dialerPeerSecretFixture()
	join := &stubJoinProvider{values: map[string]any{"joinToken": "fake-token", "k0sVersion": "v1.36.2+k0s"}}

	r := newFakeJoinReconciler(t, join, machine, awsMachine, dialerSecret)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(machine)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if join.calls != 1 {
		t.Errorf("expected JoinValues to be called despite the AWSMachine not being ready yet, got %d calls", join.calls)
	}
	secret := &corev1.Secret{}
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "hilton-cloud-worker-jarvistam-0-bootstrap"}, secret); err != nil {
		t.Fatalf("expected a bootstrap secret even though the AWSMachine isn't ready: %v", err)
	}
}

func TestReconcile_ProvisionsBootstrapSecretEndToEnd(t *testing.T) {
	machine := machineWithInfraRef("hilton-cloud-worker-jarvistam-0", "default", "hilton-cloud-worker-jarvistam-0")
	awsMachine := fakeAWSMachine("hilton-cloud-worker-jarvistam-0", "default", true)
	dialerSecret := dialerPeerSecretFixture()
	join := &stubJoinProvider{values: map[string]any{"joinToken": "fake-token", "k0sVersion": "v1.36.2+k0s"}}

	r := newFakeJoinReconciler(t, join, machine, awsMachine, dialerSecret)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(machine)})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected no requeue on success, got RequeueAfter=%v", res.RequeueAfter)
	}
	if join.calls != 1 {
		t.Errorf("expected JoinValues to be called exactly once, got %d calls", join.calls)
	}

	bootstrapSecret := &corev1.Secret{}
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "hilton-cloud-worker-jarvistam-0-bootstrap"}, bootstrapSecret); err != nil {
		t.Fatalf("expected a bootstrap secret to be created: %v", err)
	}
	if bootstrapSecret.Type != "cluster.x-k8s.io/secret" {
		t.Errorf("bootstrap secret type = %q, want cluster.x-k8s.io/secret", bootstrapSecret.Type)
	}
	// A real API server converts StringData to Data on write and never
	// returns StringData on a subsequent Get (it's write-only) --
	// confirmed against a real kind cluster while building this test
	// further. This fake client does the opposite: it preserves
	// StringData across Get and never populates Data at all. Neither
	// behavior alone is safe to assert on, so secretValue reads
	// whichever is actually populated -- correct against the fake here
	// AND against anything real.
	if secretValue(bootstrapSecret, "format") != "cloud-config" {
		t.Errorf("bootstrap secret format = %q, want cloud-config", secretValue(bootstrapSecret, "format"))
	}
	rendered := secretValue(bootstrapSecret, "value")
	for _, want := range []string{"joinToken=fake-token", "k0sVersion=v1.36.2+k0s", "apiVIP=10.101.0.1", "nodeVIP4=10.101.0.4", "nodeVIP6=fd8f:cf26:522a::4"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered bootstrap content missing %q; got: %s", want, rendered)
		}
	}

	updatedDialerSecret := &corev1.Secret{}
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "wg-dialer", Name: "wg-dialer-peer"}, updatedDialerSecret); err != nil {
		t.Fatalf("getting updated dialer secret: %v", err)
	}
	// Per-Machine keys, not flat singletons -- a second cloud Machine
	// reconciling must never clobber this one's entry.
	const machineName = "hilton-cloud-worker-jarvistam-0"
	if string(updatedDialerSecret.Data["peer-endpoint-"+machineName]) != "pending" {
		t.Errorf("peer-endpoint-%s = %q, want \"pending\" until the endpoint-controller learns the real external IP", machineName, updatedDialerSecret.Data["peer-endpoint-"+machineName])
	}
	if len(updatedDialerSecret.Data["peer-public-key-"+machineName]) == 0 {
		t.Error("peer-public-key-<machine> wasn't populated with the newly generated cloud-side public key")
	}

	updatedMachine := &unstructured.Unstructured{}
	updatedMachine.SetGroupVersionKind(machineGVK)
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(machine), updatedMachine); err != nil {
		t.Fatalf("getting updated machine: %v", err)
	}
	if updatedMachine.GetAnnotations()[NodeVIPAnnotation] != "4" {
		t.Errorf("machine's %s annotation = %q, want \"4\" (NodeVIPStart, first allocation)", NodeVIPAnnotation, updatedMachine.GetAnnotations()[NodeVIPAnnotation])
	}
}

// TestReconcile_TwoCloudMachinesDoNotClobberEachOther is the direct
// regression test for the actual architectural gap this redesign
// closes: before per-Machine keys, the dialer Secret's flat
// peer-public-key/peer-endpoint keys meant a second cloud Machine
// reconciling would silently overwrite the first's entry -- the
// on-prem dialer would then only ever know about whichever Machine
// reconciled last. Two Machines reconciling in sequence must each get
// their own independent, surviving entry.
func TestReconcile_TwoCloudMachinesDoNotClobberEachOther(t *testing.T) {
	machineA := machineWithInfraRef("cloud-worker-a", "default", "cloud-worker-a")
	awsMachineA := fakeAWSMachine("cloud-worker-a", "default", true)
	machineB := machineWithInfraRef("cloud-worker-b", "default", "cloud-worker-b")
	awsMachineB := fakeAWSMachine("cloud-worker-b", "default", true)
	dialerSecret := dialerPeerSecretFixture()
	join := &stubJoinProvider{values: map[string]any{"joinToken": "fake-token", "k0sVersion": "v1.36.2+k0s"}}

	r := newFakeJoinReconciler(t, join, machineA, awsMachineA, machineB, awsMachineB, dialerSecret)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(machineA)}); err != nil {
		t.Fatalf("Reconcile(machineA): %v", err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(machineB)}); err != nil {
		t.Fatalf("Reconcile(machineB): %v", err)
	}

	updatedDialerSecret := &corev1.Secret{}
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "wg-dialer", Name: "wg-dialer-peer"}, updatedDialerSecret); err != nil {
		t.Fatalf("getting updated dialer secret: %v", err)
	}

	pubA := updatedDialerSecret.Data["peer-public-key-cloud-worker-a"]
	pubB := updatedDialerSecret.Data["peer-public-key-cloud-worker-b"]
	if len(pubA) == 0 {
		t.Fatal("cloud-worker-a's peer-public-key entry is missing -- clobbered by cloud-worker-b's reconcile")
	}
	if len(pubB) == 0 {
		t.Fatal("cloud-worker-b's peer-public-key entry is missing")
	}
	if string(pubA) == string(pubB) {
		t.Error("cloud-worker-a and cloud-worker-b ended up with the same public key -- one clobbered the other")
	}

	// The actual regression case for the WireGuardAddrAnnotation fix:
	// each Machine's own tunnel address (both its own RouteHost/
	// AllowedIPs entry in the dialer Secret, and its own peer-route-host)
	// must be distinct -- before that fix, every cloud Machine got the
	// SAME literal r.WireGuardAddress, which is invalid for WireGuard
	// cryptokey routing (two peers can't share an AllowedIPs
	// destination) and ambiguous for the kernel route it produces.
	routeHostA := string(updatedDialerSecret.Data["peer-route-host-cloud-worker-a"])
	routeHostB := string(updatedDialerSecret.Data["peer-route-host-cloud-worker-b"])
	if routeHostA == "" || routeHostB == "" {
		t.Fatalf("expected both peer-route-host entries to be set, got %q and %q", routeHostA, routeHostB)
	}
	if routeHostA == routeHostB {
		t.Errorf("cloud-worker-a and cloud-worker-b got the same tunnel address %q -- WireGuard address allocation collided", routeHostA)
	}

	if string(updatedDialerSecret.Data["peer-endpoint-cloud-worker-a"]) != "pending" {
		t.Errorf("cloud-worker-a's peer-endpoint = %q, want \"pending\"", updatedDialerSecret.Data["peer-endpoint-cloud-worker-a"])
	}
	if string(updatedDialerSecret.Data["peer-endpoint-cloud-worker-b"]) != "pending" {
		t.Errorf("cloud-worker-b's peer-endpoint = %q, want \"pending\"", updatedDialerSecret.Data["peer-endpoint-cloud-worker-b"])
	}

	// Both Machines must also get their own, non-colliding node-VIP
	// allocation -- allocateNodeVIPIndex already scans all Machines, but
	// confirm it actually holds end to end through two real Reconcile
	// calls, not just in isolation.
	updatedA := &unstructured.Unstructured{}
	updatedA.SetGroupVersionKind(machineGVK)
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(machineA), updatedA); err != nil {
		t.Fatalf("getting updated machineA: %v", err)
	}
	updatedB := &unstructured.Unstructured{}
	updatedB.SetGroupVersionKind(machineGVK)
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(machineB), updatedB); err != nil {
		t.Fatalf("getting updated machineB: %v", err)
	}
	if updatedA.GetAnnotations()[NodeVIPAnnotation] == updatedB.GetAnnotations()[NodeVIPAnnotation] {
		t.Errorf("machineA and machineB got the same node-VIP index %q -- allocation collided", updatedA.GetAnnotations()[NodeVIPAnnotation])
	}
}

func TestReconcile_SkipsIfBootstrapSecretAlreadyExists(t *testing.T) {
	machine := machineWithInfraRef("hilton-cloud-worker-jarvistam-0", "default", "hilton-cloud-worker-jarvistam-0")
	awsMachine := fakeAWSMachine("hilton-cloud-worker-jarvistam-0", "default", true)
	existingBootstrap := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "hilton-cloud-worker-jarvistam-0-bootstrap", Namespace: "default"},
		Data:       map[string][]byte{"value": []byte("already provisioned")},
	}
	join := &stubJoinProvider{values: map[string]any{"joinToken": "fake-token", "k0sVersion": "v1.36.2+k0s"}}

	r := newFakeJoinReconciler(t, join, machine, awsMachine, existingBootstrap)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(machine)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if join.calls != 0 {
		t.Errorf("JoinValues must not be called when a bootstrap secret already exists (idempotency), got %d calls", join.calls)
	}

	secret := &corev1.Secret{}
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "hilton-cloud-worker-jarvistam-0-bootstrap"}, secret); err != nil {
		t.Fatalf("getting bootstrap secret: %v", err)
	}
	if string(secret.Data["value"]) != "already provisioned" {
		t.Error("existing bootstrap secret was overwritten -- Reconcile must never touch an already-provisioned secret")
	}
}

// erroringReader wraps a real client.Reader but returns a fixed error
// for Get calls matching one GVK -- used to inject the *exact* error
// shape a real API server's RESTMapper produces when a CRD isn't
// installed (meta.NoKindMatchError, or its string-only equivalent).
// The fake controller-runtime client does NOT reproduce this: an
// unstructured Get for a GVK it doesn't know about comes back as a
// plain NotFound (confirmed empirically), which would make a naive
// "just don't register the scheme type" test pass for the wrong
// reason -- it would never actually reach isMissingCRD's branch at
// all. This makes the test honest about which branch it exercises.
type erroringReader struct {
	client.Reader
	failGVK schema.GroupVersionKind
	err     error
}

func (r *erroringReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if u, ok := obj.(*unstructured.Unstructured); ok && u.GroupVersionKind() == r.failGVK {
		return r.err
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

func testMissingCRDReconciler(t *testing.T, injectedErr error) (*Reconciler, *unstructured.Unstructured, *stubJoinProvider) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding clientgoscheme: %v", err)
	}
	scheme.AddKnownTypeWithName(machineGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(machineListGVK(), &unstructured.UnstructuredList{})
	scheme.AddKnownTypeWithName(joinaws.Provider{}.GVK(), &unstructured.Unstructured{})

	machine := machineWithInfraRef("hilton-cloud-worker-jarvistam-0", "default", "hilton-cloud-worker-jarvistam-0")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(machine).Build()
	join := &stubJoinProvider{values: map[string]any{}}
	r := &Reconciler{
		Client:                    c,
		Reader:                    &erroringReader{Reader: c, failGVK: joinaws.Provider{}.GVK(), err: injectedErr},
		Join:                      join,
		InfraProviders:            []InfraProvider{joinaws.Provider{}},
		BootstrapSecretNameFormat: "%s-bootstrap",
	}
	return r, machine, join
}

func TestReconcile_MissingInfrastructureCRD_RequeuesGracefully(t *testing.T) {
	// The typed error path: meta.IsNoMatchError recognizes this
	// directly, matching a RESTMapper genuinely failing to resolve a
	// Kind whose CRD isn't installed.
	r, machine, join := testMissingCRDReconciler(t, &meta.NoKindMatchError{
		GroupKind:        schema.GroupKind{Group: "infrastructure.cluster.x-k8s.io", Kind: "AWSMachine"},
		SearchedVersions: []string{"v1beta2"},
	})

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(machine)})
	if err != nil {
		t.Fatalf("Reconcile must not return a hard error when the infra CRD is missing, got: %v", err)
	}
	if res.RequeueAfter != crdRecheckInterval {
		t.Errorf("RequeueAfter = %v, want the CRD recheck interval %v", res.RequeueAfter, crdRecheckInterval)
	}
	if join.calls != 0 {
		t.Errorf("JoinValues must not be called before infra is confirmed ready, got %d calls", join.calls)
	}
}

func TestReconcile_MissingInfrastructureCRD_StringFallback_RequeuesGracefully(t *testing.T) {
	// The string-matching fallback path: some server versions surface a
	// missing CRD as a plain error message rather than a typed
	// meta.NoKindMatchError (see isMissingCRD's comment) -- this proves
	// that fallback is actually reachable through the full Reconcile
	// path, not just a pure-function unit test of isMissingCRD itself.
	r, machine, join := testMissingCRDReconciler(t, fmt.Errorf(`the server could not find the requested resource`))

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(machine)})
	if err != nil {
		t.Fatalf("Reconcile must not return a hard error when the infra CRD is missing, got: %v", err)
	}
	if res.RequeueAfter != crdRecheckInterval {
		t.Errorf("RequeueAfter = %v, want the CRD recheck interval %v", res.RequeueAfter, crdRecheckInterval)
	}
	if join.calls != 0 {
		t.Errorf("JoinValues must not be called before infra is confirmed ready, got %d calls", join.calls)
	}
}

// stubInfraProvider is a mock InfraProvider registered under a
// distinct GVK, used to prove the reconciler's provider selection is
// genuinely inferred from spec.infrastructureRef.kind rather than
// only ever exercising the one AWS provider that happens to be
// registered.
type stubInfraProvider struct {
	gvk             schema.GroupVersionKind
	ready           bool
	infraValues     map[string]any
	infraValueCalls int
}

func (s *stubInfraProvider) GVK() schema.GroupVersionKind { return s.gvk }
func (s *stubInfraProvider) Ready(ctx context.Context, machine *unstructured.Unstructured) (bool, error) {
	return s.ready, nil
}
func (s *stubInfraProvider) InfraValues(ctx context.Context, machine *unstructured.Unstructured) (map[string]any, error) {
	s.infraValueCalls++
	return s.infraValues, nil
}

func TestReconcile_InfersInfraProviderFromMachineKind(t *testing.T) {
	// Two Machines referencing two different infrastructureRef kinds,
	// two registered providers -- only the matching provider for each
	// Machine may be consulted. This is the actual behavior "the
	// operator should infer which concrete implementation it uses"
	// depends on, not just a claim in a comment.
	awsProvider := &stubInfraProvider{gvk: schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta2", Kind: "AWSMachine"}, ready: true, infraValues: map[string]any{}}
	otherProvider := &stubInfraProvider{gvk: schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "OtherMachine"}, ready: true, infraValues: map[string]any{}}

	awsMachine := &unstructured.Unstructured{}
	awsMachine.SetGroupVersionKind(awsProvider.GVK())
	awsMachine.SetName("aws-infra")
	awsMachine.SetNamespace("default")

	otherMachine := &unstructured.Unstructured{}
	otherMachine.SetGroupVersionKind(otherProvider.GVK())
	otherMachine.SetName("other-infra")
	otherMachine.SetNamespace("default")

	machineA := fakeMachine("machine-a", "")
	machineA.SetNamespace("default")
	_ = unstructured.SetNestedField(machineA.Object, "aws-infra", "spec", "infrastructureRef", "name")
	_ = unstructured.SetNestedField(machineA.Object, "AWSMachine", "spec", "infrastructureRef", "kind")

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding clientgoscheme: %v", err)
	}
	scheme.AddKnownTypeWithName(machineGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(machineListGVK(), &unstructured.UnstructuredList{})
	scheme.AddKnownTypeWithName(awsProvider.GVK(), &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(otherProvider.GVK(), &unstructured.Unstructured{})
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(machineA, awsMachine, otherMachine, dialerPeerSecretFixture()).Build()

	tmplPath := filepath.Join(t.TempDir(), "test.tmpl")
	if err := os.WriteFile(tmplPath, []byte("ok"), 0o644); err != nil {
		t.Fatalf("writing test template: %v", err)
	}

	r := &Reconciler{
		Client:                    c,
		Reader:                    c,
		Join:                      &stubJoinProvider{values: map[string]any{}},
		InfraProviders:            []InfraProvider{awsProvider, otherProvider},
		TemplatePath:              tmplPath,
		NodeVIP4Prefix:            "10.101.0.",
		NodeVIP6Prefix:            "fd8f:cf26:522a::",
		NodeVIPStart:              4,
		WireGuardAddress:          "10.100.0.2/24",
		DialerPeerSecretNamespace: "wg-dialer",
		DialerPeerSecretName:      "wg-dialer-peer",
		BootstrapSecretNameFormat: "%s-bootstrap",
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(machineA)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// Reconcile doesn't gate bootstrap-secret creation on Ready() (see
	// reconciler.go: waiting for "instance running" before creating the
	// boot data it needs to run would deadlock), so InfraValues is the
	// observable proof of which provider got consulted instead.
	if awsProvider.infraValueCalls != 1 {
		t.Errorf("expected the AWSMachine-kind provider to be consulted exactly once, got %d calls", awsProvider.infraValueCalls)
	}
	if otherProvider.infraValueCalls != 0 {
		t.Errorf("expected the OtherMachine-kind provider to NEVER be consulted for a Machine whose infrastructureRef.kind is AWSMachine, got %d calls", otherProvider.infraValueCalls)
	}
}

func TestReconcile_InfraProviderValidateErrorBlocksBootstrapSecretCreation(t *testing.T) {
	// Full Reconcile-level proof of the aws.Validator wiring, using the
	// real aws.Provider (not a stub): reproduces the exact live bug
	// caught against the hilton cluster (AWSClusterStaticIdentity's
	// secretRef Secret in the wrong namespace) and confirms Reconcile
	// surfaces it as an immediate error instead of proceeding to create
	// a bootstrap secret that CAPA could never actually use.
	clusterGVK := schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Cluster"}
	awsClusterGVK := schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta2", Kind: "AWSCluster"}
	awsClusterStaticIdentityGVK := schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta2", Kind: "AWSClusterStaticIdentity"}

	machine := machineWithInfraRef("hilton-cloud-worker-jarvistam-0", "default", "hilton-cloud-worker-jarvistam-0")
	machine.SetLabels(map[string]string{"cluster.x-k8s.io/cluster-name": "hilton-jarvistam"})
	awsMachine := fakeAWSMachine("hilton-cloud-worker-jarvistam-0", "default", false)
	awsMachine.SetLabels(map[string]string{"cluster.x-k8s.io/cluster-name": "hilton-jarvistam"})

	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(clusterGVK)
	cluster.SetName("hilton-jarvistam")
	cluster.SetNamespace("default")
	_ = unstructured.SetNestedField(cluster.Object, "AWSCluster", "spec", "infrastructureRef", "kind")
	_ = unstructured.SetNestedField(cluster.Object, "hilton-jarvistam", "spec", "infrastructureRef", "name")

	awsCluster := &unstructured.Unstructured{}
	awsCluster.SetGroupVersionKind(awsClusterGVK)
	awsCluster.SetName("hilton-jarvistam")
	awsCluster.SetNamespace("default")
	_ = unstructured.SetNestedField(awsCluster.Object, "AWSClusterStaticIdentity", "spec", "identityRef", "kind")
	_ = unstructured.SetNestedField(awsCluster.Object, "jarvistam-cloud-worker", "spec", "identityRef", "name")

	identity := &unstructured.Unstructured{}
	identity.SetGroupVersionKind(awsClusterStaticIdentityGVK)
	identity.SetName("jarvistam-cloud-worker")
	_ = unstructured.SetNestedField(identity.Object, "jarvistam-cloud-worker-credentials", "spec", "secretRef")

	// The bug, reproduced: credentials Secret only in "default", never
	// in "capa-system" (CAPA's actual manager namespace).
	wrongNamespaceSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "jarvistam-cloud-worker-credentials", Namespace: "default"},
	}

	dialerSecret := dialerPeerSecretFixture()
	join := &stubJoinProvider{values: map[string]any{"joinToken": "fake-token", "k0sVersion": "v1.36.2+k0s"}}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding clientgoscheme: %v", err)
	}
	scheme.AddKnownTypeWithName(machineGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(machineListGVK(), &unstructured.UnstructuredList{})
	scheme.AddKnownTypeWithName(joinaws.Provider{}.GVK(), &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(clusterGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(awsClusterGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(awsClusterStaticIdentityGVK, &unstructured.Unstructured{})
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(machine, awsMachine, dialerSecret, cluster, awsCluster, identity, wrongNamespaceSecret).
		Build()

	tmplPath := filepath.Join(t.TempDir(), "test.tmpl")
	if err := os.WriteFile(tmplPath, []byte("ok"), 0o644); err != nil {
		t.Fatalf("writing test template: %v", err)
	}

	r := &Reconciler{
		Client:                    c,
		Reader:                    c,
		Join:                      join,
		InfraProviders:            []InfraProvider{joinaws.Provider{}},
		TemplatePath:              tmplPath,
		NodeVIP4Prefix:            "10.101.0.",
		NodeVIP6Prefix:            "fd8f:cf26:522a::",
		NodeVIPStart:              4,
		DialerPeerSecretNamespace: "wg-dialer",
		DialerPeerSecretName:      "wg-dialer-peer",
		BootstrapSecretNameFormat: "%s-bootstrap",
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(machine)})
	if err == nil {
		t.Fatal("expected Reconcile to surface the misplaced-identity-secret error, got nil")
	}
	if !strings.Contains(err.Error(), "capa-system") {
		t.Errorf("error %q doesn't mention the correct namespace -- not actionable", err.Error())
	}
	if join.calls != 0 {
		t.Errorf("JoinValues must not be called when infra validation fails, got %d calls", join.calls)
	}
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "hilton-cloud-worker-jarvistam-0-bootstrap"}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Errorf("expected no bootstrap secret to be created when infra validation fails, Get returned: %v", err)
	}
}
