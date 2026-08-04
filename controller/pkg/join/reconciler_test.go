package join

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// awsMachineGVK mirrors the real CAPA AWSMachine GVK. Deliberately a
// local copy rather than an import of pkg/join/aws: aws imports this
// package (for NodeRequest/MachineProvisioner), so importing it back
// from an in-package test file is an import cycle. The tests that need
// the REAL aws.Provider live in reconciler_aws_test.go (package
// join_test), where no cycle exists.
var awsMachineGVK = schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta2", Kind: "AWSMachine"}

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

// dialerPeerSecretFixture models the peer Secret AFTER the mesh side
// exists: two worker nodes' dialers have published their public keys
// (node-public-key-*, self-generated -- NO private key is ever in this
// Secret) and the endpoint-controller has allocated their tunnel
// addresses and recorded their cluster addresses.
func dialerPeerSecretFixture() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "wg-dialer-peer", Namespace: "wg-dialer"},
		Data: map[string][]byte{
			"node-public-key-worker-1":     []byte("c3BhcmsyYWIzLXB1YmxpYy1rZXktdGVzdC1vbmx5PT0="),
			"node-tunnel-address-worker-1": []byte("10.100.0.1/24"),
			"node-cluster-vips-worker-1":   []byte("10.101.0.2"),
			"node-public-key-worker-2":     []byte("c3Bhcms1ODY3LXB1YmxpYy1rZXktdGVzdC1vbmx5PT0="),
			"node-tunnel-address-worker-2": []byte("10.100.0.3/24"),
			"node-cluster-vips-worker-2":   []byte("10.101.0.3"),
		},
	}
}

func machineListGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "MachineList"}
}

func fakeMachine(name string, _ string) *unstructured.Unstructured {
	m := &unstructured.Unstructured{}
	m.SetGroupVersionKind(machineGVK)
	m.SetName(name)
	m.SetNamespace("default")
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
	return &Reconciler{Client: c, Reader: c}
}

func TestValidateDialerBinaries_UnpinnedDigestIsAlwaysFatal(t *testing.T) {
	// Rendering userdata that downloads an unverifiable binary is a
	// supply-chain hole, never a fallback.
	r := &Reconciler{DialerBinaryURLARM64: "https://example.com/dialer-arm64"}
	if err := r.validateDialerBinaries(); err == nil {
		t.Fatal("expected an error when the arm64 sha256 is unset, got nil")
	}
	r = &Reconciler{DialerBinarySHA256ARM64: "abc123"}
	if err := r.validateDialerBinaries(); err == nil {
		t.Fatal("expected an error when the arm64 URL is unset, got nil")
	}
	r = &Reconciler{}
	if err := r.validateDialerBinaries(); err == nil {
		t.Fatal("expected an error when no architecture is configured, got nil")
	}
	// One architecture is enough: a machine of the other kind fails at
	// boot with a clear message rather than at render time.
	r = &Reconciler{DialerBinaryURLARM64: "u", DialerBinarySHA256ARM64: "s"}
	if err := r.validateDialerBinaries(); err != nil {
		t.Fatalf("one configured architecture should be valid, got %v", err)
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
	m.SetGroupVersionKind(awsMachineGVK)
	m.SetName(name)
	m.SetNamespace(namespace)
	_ = unstructured.SetNestedField(m.Object, ready, "status", "ready")
	return m
}

// stubInfraProvider is a mock InfraProvider registered under a given
// GVK -- used both as the stand-in for AWS (the real aws.Provider can't
// be imported here, see awsMachineGVK's comment) and to prove provider
// selection is genuinely inferred from spec.infrastructureRef.kind.
type stubInfraProvider struct {
	gvk             schema.GroupVersionKind
	infraValues     map[string]any
	infraValueCalls int
}

func (s *stubInfraProvider) GVK() schema.GroupVersionKind { return s.gvk }
func (s *stubInfraProvider) InfraValues(ctx context.Context, machine *unstructured.Unstructured) (map[string]any, error) {
	s.infraValueCalls++
	return s.infraValues, nil
}

func awsShapedStub() *stubInfraProvider {
	return &stubInfraProvider{gvk: awsMachineGVK, infraValues: map[string]any{"arch": "arm64"}}
}

// machineWithInfraRef builds a Machine whose infrastructureRef.kind is
// "AWSMachine" -- this is the real, live signal (see fixed
// infrastructureRef: {"apiGroup":"...","kind":"AWSMachine","name":"..."}
// confirmed via kubectl against the actual example cloud-worker
// Machine) the reconciler uses to infer which registered InfraProvider
// applies, never a hardcoded assumption.
func machineWithInfraRef(name, namespace, infraRefName string) *unstructured.Unstructured {
	m := fakeMachine(name, "")
	m.SetNamespace(namespace)
	_ = unstructured.SetNestedField(m.Object, infraRefName, "spec", "infrastructureRef", "name")
	_ = unstructured.SetNestedField(m.Object, awsMachineGVK.Kind, "spec", "infrastructureRef", "kind")
	return m
}

// newFakeJoinReconciler builds a Reconciler wired for a full
// Reconcile() call: unlike newFakeReconciler (allocateNodeVIPIndex
// tests only), this registers AWSMachine too and fills in every field
// Reconcile actually reads.
func newFakeJoinReconciler(t *testing.T, joinProvider ClusterJoinProvider, objs ...client.Object) *Reconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding clientgoscheme: %v", err)
	}
	scheme.AddKnownTypeWithName(machineGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(machineListGVK(), &unstructured.UnstructuredList{})
	scheme.AddKnownTypeWithName(awsMachineGVK, &unstructured.Unstructured{})
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()

	tmplPath := filepath.Join(t.TempDir(), "test.tmpl")
	tmpl := "joinToken={{.joinToken}} k0sVersion={{.k0sVersion}} apiVIP={{.apiVIP}} kubeletExtraArgs={{.kubeletExtraArgs}} wgAddress={{.wireguardAddress}} podCIDRs={{.podCIDRs}} serviceCIDRs={{.serviceCIDRs}} iface={{.interfaceName}} binURL={{.dialerBinaryURLArm64}} binSHA={{.dialerBinarySHA256Arm64}} machine={{.machineName}} peersFileJSON={{.peersFileJSON}}"
	if err := os.WriteFile(tmplPath, []byte(tmpl), 0o644); err != nil {
		t.Fatalf("writing test template: %v", err)
	}

	return &Reconciler{
		Client:         c,
		Reader:         c,
		Join:           joinProvider,
		InfraProviders: []InfraProvider{awsShapedStub()},

		TemplatePath:     tmplPath,
		APIVIP:           "10.101.0.1",
		KubeletExtraArgs: "--node-labels=cloud-provisioning.appmana.com/role=cloud-worker",

		WireGuardAddress:    "10.100.0.128/24",
		WireGuardListenPort: "51820",

		DialerPeerSecretNamespace: "wg-dialer",
		DialerPeerSecretName:      "wg-dialer-peer",

		InterfaceName: "cldt0a1b2c3d",

		DialerBinaryURLARM64:    "https://example.com/wg-dialer-linux-arm64",
		DialerBinarySHA256ARM64: "deadbeef" + strings.Repeat("0", 56),

		BootstrapSecretNameFormat: "%s-bootstrap",
	}
}

func TestReconcile_CreatesBootstrapSecretEvenWhenInfraNotReady(t *testing.T) {
	// Regression test for a genuine deadlock caught live against the
	// real the cluster cluster: CAPA's AWSMachine controller refuses to call
	// RunInstances until this bootstrap Secret already exists (the
	// Secret IS the cloud-init user-data the instance boots from), so
	// gating its creation on the AWSMachine being "ready" (instance
	// already running) can never succeed -- ready never becomes true
	// without the Secret, and the Secret never gets created while
	// waiting for ready. Bootstrap-secret creation must proceed as soon
	// as the infrastructureRef target object merely exists.
	machine := machineWithInfraRef("cloud-worker-0", "default", "cloud-worker-0")
	awsMachine := fakeAWSMachine("cloud-worker-0", "default", false)
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
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "cloud-worker-0-bootstrap"}, secret); err != nil {
		t.Fatalf("expected a bootstrap secret even though the AWSMachine isn't ready: %v", err)
	}
}

func TestReconcile_NoPublishedTunnelEndpoints_WaitsInsteadOfBakingEmptyPeerList(t *testing.T) {
	// Userdata is immutable once the instance launches. If no local
	// dialer has published a key yet (fresh install ordering), rendering
	// now would bake an empty peer list into the bootstrap -- a node
	// that could never reach anything. The reconciler must wait, not
	// render.
	machine := machineWithInfraRef("cloud-worker-0", "default", "cloud-worker-0")
	awsMachine := fakeAWSMachine("cloud-worker-0", "default", false)
	emptyPeerSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "wg-dialer-peer", Namespace: "wg-dialer"},
	}
	join := &stubJoinProvider{values: map[string]any{}}

	r := newFakeJoinReconciler(t, join, machine, awsMachine, emptyPeerSecret)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(machine)})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Error("expected a requeue while waiting for tunnel endpoints to publish, got none")
	}
	if join.calls != 0 {
		t.Errorf("JoinValues must not be called (and a join token not minted/burned) before the peer list can be non-empty, got %d calls", join.calls)
	}
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "cloud-worker-0-bootstrap"}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Errorf("expected no bootstrap secret while waiting, Get returned: %v", err)
	}
}

func TestReconcile_ProvisionsBootstrapSecretEndToEnd(t *testing.T) {
	machine := machineWithInfraRef("cloud-worker-0", "default", "cloud-worker-0")
	awsMachine := fakeAWSMachine("cloud-worker-0", "default", true)
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
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "cloud-worker-0-bootstrap"}, bootstrapSecret); err != nil {
		t.Fatalf("expected a bootstrap secret to be created: %v", err)
	}
	if bootstrapSecret.Type != "cluster.x-k8s.io/secret" {
		t.Errorf("bootstrap secret type = %q, want cluster.x-k8s.io/secret", bootstrapSecret.Type)
	}
	// Owned by the Machine: join tokens expire, so a deleted-and-
	// recreated Machine must get a FRESH render, which only happens if
	// this Secret is garbage-collected with its Machine.
	if len(bootstrapSecret.OwnerReferences) != 1 || bootstrapSecret.OwnerReferences[0].Kind != "Machine" || bootstrapSecret.OwnerReferences[0].Name != machine.GetName() {
		t.Errorf("bootstrap secret ownerReferences = %+v, want exactly one reference to Machine %s", bootstrapSecret.OwnerReferences, machine.GetName())
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
	for _, want := range []string{
		"joinToken=fake-token",
		"k0sVersion=v1.36.2+k0s",
		"apiVIP=10.101.0.1",
		"wgAddress=10.100.0.128/24",
		"iface=cldt0a1b2c3d",
		"binURL=https://example.com/wg-dialer-linux-arm64",
		"binSHA=deadbeef",
		"machine=cloud-worker-0",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered bootstrap content missing %q; got: %s", want, rendered)
		}
	}
	// The baked peers.json must carry both published locals, and the
	// API VIP must ride exactly the designated transit local (lowest
	// tunnel address, 10.100.0.1 = worker-1): control planes carry no
	// tunnel, so the API is reached through that one worker.
	for _, want := range []string{
		"c3BhcmsyYWIzLXB1YmxpYy1rZXktdGVzdC1vbmx5PT0=",
		"c3Bhcms1ODY3LXB1YmxpYy1rZXktdGVzdC1vbmx5PT0=",
		"10.101.0.1/32",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("baked peers.json missing %q; rendered: %s", want, rendered)
		}
	}
	if strings.Count(rendered, "10.101.0.1/32") != 1 {
		t.Errorf("API VIP must appear in exactly ONE local peer's AllowedIPs (the designated transit), got %d occurrences", strings.Count(rendered, "10.101.0.1/32"))
	}

	updatedDialerSecret := &corev1.Secret{}
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "wg-dialer", Name: "wg-dialer-peer"}, updatedDialerSecret); err != nil {
		t.Fatalf("getting updated dialer secret: %v", err)
	}
	// Per-Machine keys, not flat singletons -- a second cloud Machine
	// reconciling must never clobber this one's entry.
	const machineName = "cloud-worker-0"
	if string(updatedDialerSecret.Data["peer-endpoint-"+machineName]) != "pending" {
		t.Errorf("peer-endpoint-%s = %q, want \"pending\" until the endpoint-controller learns the real external IP", machineName, updatedDialerSecret.Data["peer-endpoint-"+machineName])
	}
	if len(updatedDialerSecret.Data["peer-public-key-"+machineName]) == 0 {
		t.Error("peer-public-key-<machine> wasn't populated with the newly generated cloud-side public key")
	}
	// AllowedIPs carry tunnel address AND both node VIPs (BGP sessions
	// and kubelet traffic ride the VIPs); RouteHosts carry the same
	// three as bare hosts (each becomes exactly one /32 or /128 kernel
	// route on the on-prem side, nothing wider -- pod routing is
	// Calico's job).
	wantAllowed := "10.100.0.128/32"
	if got := string(updatedDialerSecret.Data["peer-allowed-ips-"+machineName]); got != wantAllowed {
		t.Errorf("peer-allowed-ips-%s = %q, want %q", machineName, got, wantAllowed)
	}
	wantRouteHosts := "10.100.0.128"
	if got := string(updatedDialerSecret.Data["peer-route-hosts-"+machineName]); got != wantRouteHosts {
		t.Errorf("peer-route-hosts-%s = %q, want %q", machineName, got, wantRouteHosts)
	}

	updatedMachine := &unstructured.Unstructured{}
	updatedMachine.SetGroupVersionKind(machineGVK)
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(machine), updatedMachine); err != nil {
		t.Fatalf("getting updated machine: %v", err)
	}
	if updatedMachine.GetAnnotations()[WireGuardAddrAnnotation] != "10.100.0.128/24" {
		t.Errorf("machine's %s annotation = %q, want the base address for the first allocation", WireGuardAddrAnnotation, updatedMachine.GetAnnotations()[WireGuardAddrAnnotation])
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
	// each Machine's own tunnel address (both its own RouteHosts/
	// AllowedIPs entries in the dialer Secret, and its own kernel host
	// route on the on-prem side) must be distinct -- before that fix,
	// every cloud Machine got the SAME literal r.WireGuardAddress, which
	// is invalid for WireGuard cryptokey routing (two peers can't share
	// an AllowedIPs destination) and ambiguous for the kernel route it
	// produces.
	routeHostsA := string(updatedDialerSecret.Data["peer-route-hosts-cloud-worker-a"])
	routeHostsB := string(updatedDialerSecret.Data["peer-route-hosts-cloud-worker-b"])
	if routeHostsA == "" || routeHostsB == "" {
		t.Fatalf("expected both peer-route-hosts entries to be set, got %q and %q", routeHostsA, routeHostsB)
	}
	if routeHostsA == routeHostsB {
		t.Errorf("cloud-worker-a and cloud-worker-b got identical route-hosts %q, so the tunnel address allocation collided", routeHostsA)
	}
	if routeHostsA != "10.100.0.128" || routeHostsB != "10.100.0.129" {
		t.Errorf("expected sequential tunnel addresses .128 then .129, got %q and %q", routeHostsA, routeHostsB)
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
	if updatedA.GetAnnotations()[WireGuardAddrAnnotation] == updatedB.GetAnnotations()[WireGuardAddrAnnotation] {
		t.Errorf("machineA and machineB got the same tunnel address %q, so the allocation collided", updatedA.GetAnnotations()[WireGuardAddrAnnotation])
	}

	// Machine B's baked peers.json must include machine A as a
	// remote-to-remote peer (isolated remotes share no LAN -- without
	// this edge they could never reach each other), and must NOT
	// include machine B itself.
	bootstrapB := &corev1.Secret{}
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "cloud-worker-b-bootstrap"}, bootstrapB); err != nil {
		t.Fatalf("getting machine B's bootstrap secret: %v", err)
	}
	renderedB := secretValue(bootstrapB, "value")
	if !strings.Contains(renderedB, string(pubA)) {
		t.Error("machine B's baked peers.json is missing machine A's public key -- remote-to-remote mesh edge absent")
	}
	if strings.Contains(renderedB, "10.100.0.129/32") {
		t.Error("machine B's baked peers.json contains its own address as a peer -- self-exclusion failed")
	}
}

func TestReconcile_SkipsIfBootstrapSecretAlreadyExists(t *testing.T) {
	machine := machineWithInfraRef("cloud-worker-0", "default", "cloud-worker-0")
	awsMachine := fakeAWSMachine("cloud-worker-0", "default", true)
	existingBootstrap := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cloud-worker-0-bootstrap", Namespace: "default"},
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
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "cloud-worker-0-bootstrap"}, secret); err != nil {
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
	scheme.AddKnownTypeWithName(awsMachineGVK, &unstructured.Unstructured{})

	machine := machineWithInfraRef("cloud-worker-0", "default", "cloud-worker-0")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(machine).Build()
	join := &stubJoinProvider{values: map[string]any{}}
	r := &Reconciler{
		Client:                    c,
		Reader:                    &erroringReader{Reader: c, failGVK: awsMachineGVK, err: injectedErr},
		Join:                      join,
		InfraProviders:            []InfraProvider{awsShapedStub()},
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

func TestReconcile_InfersInfraProviderFromMachineKind(t *testing.T) {
	// Two Machines referencing two different infrastructureRef kinds,
	// two registered providers -- only the matching provider for each
	// Machine may be consulted. This is the actual behavior "the
	// operator should infer which concrete implementation it uses"
	// depends on, not just a claim in a comment.
	awsProvider := awsShapedStub()
	otherProvider := &stubInfraProvider{gvk: schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "OtherMachine"}, infraValues: map[string]any{}}

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
		WireGuardAddress:          "10.100.0.128/24",
		DialerPeerSecretNamespace: "wg-dialer",
		DialerPeerSecretName:      "wg-dialer-peer",
		DialerBinaryURLARM64:      "https://example.com/wg-dialer-linux-arm64",
		DialerBinarySHA256ARM64:   "deadbeef",
		BootstrapSecretNameFormat: "%s-bootstrap",
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(machineA)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// Reconcile doesn't gate bootstrap-secret creation on infra
	// readiness (see reconciler.go: waiting for "instance running"
	// before creating the boot data it needs to run would deadlock), so
	// InfraValues is the observable proof of which provider got
	// consulted instead.
	if awsProvider.infraValueCalls != 1 {
		t.Errorf("expected the AWSMachine-kind provider to be consulted exactly once, got %d calls", awsProvider.infraValueCalls)
	}
	if otherProvider.infraValueCalls != 0 {
		t.Errorf("expected the OtherMachine-kind provider to NEVER be consulted for a Machine whose infrastructureRef.kind is AWSMachine, got %d calls", otherProvider.infraValueCalls)
	}
}

func TestReconcile_EmitsOnlyRealAddresses(t *testing.T) {
	// Every entry published for a peer must parse as an address. A
	// value assembled from a prefix and an index once produced a bare
	// "200" on a cluster with no prefix set, and every consumer then
	// treated it as one: "200/32" in the peer AllowedIPs, which the
	// dialer refuses to parse, so no tunnel was ever built.
	machine := machineWithInfraRef("cloud-worker-0", "default", "cloud-worker-0")
	awsMachine := fakeAWSMachine("cloud-worker-0", "default", true)
	join := &stubJoinProvider{values: map[string]any{"joinToken": "t", "k0sVersion": "v"}}
	r := newFakeJoinReconciler(t, join, machine, awsMachine, dialerPeerSecretFixture())

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(machine)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	secret := &corev1.Secret{}
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "wg-dialer", Name: "wg-dialer-peer"}, secret); err != nil {
		t.Fatalf("getting peer secret: %v", err)
	}
	for _, key := range []string{"peer-allowed-ips-cloud-worker-0", "peer-route-hosts-cloud-worker-0"} {
		val := string(secret.Data[key])
		for _, entry := range strings.Split(val, ",") {
			host := strings.SplitN(strings.TrimSpace(entry), "/", 2)[0]
			if net.ParseIP(host) == nil {
				t.Errorf("%s contains %q, which is not an IP address (whole value: %q)", key, entry, val)
			}
		}
	}
}
