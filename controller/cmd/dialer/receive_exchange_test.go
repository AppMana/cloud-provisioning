package main

import (
	"context"
	"encoding/json"
	"io"
	"net/netip"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// TestNativeReceiveExchange is a test-binary adapter for the isolated packet
// experiment. Only public bindings leave the process; root material stays in
// the experiment's private temporary directory. The parent owns topology and
// traffic assertions. Kubernetes receipt publication uses an in-memory API;
// API-server concurrency and authorization are outside this test. This adapter
// is never part of the shipped dialer binary.
func TestNativeReceiveExchange(t *testing.T) {
	requestPath := os.Getenv("CLDT_NATIVE_RECEIVE_EXCHANGE")
	if requestPath == "" {
		t.Skip("requires isolated packet experiment")
	}
	for _, kind := range []string{"net", "mnt"} {
		self, err := os.Readlink("/proc/self/ns/" + kind)
		if err != nil {
			t.Fatal(err)
		}
		init, err := os.Readlink("/proc/1/ns/" + kind)
		if err != nil {
			t.Fatal(err)
		}
		if self == init {
			t.Fatal("refusing guest host namespace")
		}
	}
	links, err := netlink.LinkList()
	if err != nil {
		t.Fatal(err)
	}
	for _, link := range links {
		if link.Attrs().Name != "lo" && link.Attrs().Name != "outer" {
			t.Fatal("unexpected link in packet experiment namespace")
		}
	}
	var request struct {
		MeshID      string             `json:"meshID"`
		Previous    tunnel.PeerListDoc `json:"previous"`
		Desired     tunnel.PeerListDoc `json:"desired"`
		Native      []netip.Prefix     `json:"native"`
		RootKeyFile string             `json:"rootKeyFile"`
		ResultFile  string             `json:"resultFile"`
	}
	f, err := os.Open(requestPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		t.Fatal("invalid experiment request")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatal("trailing experiment input")
	}
	raw, err := os.ReadFile(request.RootKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	root, err := wgtypes.ParseKey(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal("invalid experiment root identity")
	}
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := netlink.RuleList(netlink.FAMILY_ALL)
	if err != nil {
		t.Fatal(err)
	}
	devices, err := wgctrl.New()
	if err != nil {
		t.Fatal(err)
	}
	defer devices.Close()
	base, err := json.Marshal(request.Previous)
	if err != nil {
		t.Fatal(err)
	}
	bootID, err := localReceiveBootID()
	if err != nil {
		t.Fatal(err)
	}
	intent := tunnel.ReceivePrepare{Version: 1, MeshUID: request.MeshID, NodeName: "worker", NodeUID: "node-uid", BootID: bootID, RootPublicKey: root.PublicKey().String(), BaseHash: tunnel.HashPeerList(base), Desired: request.Desired, Native: request.Native}
	plan, err := tunnel.PlanReceiveHandover(intent.MeshUID, request.Previous, intent.Desired, intent.Native)
	if err != nil {
		t.Fatal(err)
	}
	intent.Ports, _, err = tunnel.AssignReceivePorts(plan.Receivers, map[string]int{}, []int{51821, 51822})
	if err != nil {
		t.Fatal(err)
	}
	intentRaw, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "adoption", Namespace: "experiment", UID: "secret-uid", ResourceVersion: "1"}, Data: map[string][]byte{tunnel.CloudPeersKey: base, tunnel.ReceivePrepareKey: intentRaw}}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", UID: "node-uid"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{BootID: bootID}}}
	api := fake.NewClientset(secret, node)
	prepare := func(intent tunnel.ReceivePrepare, previous tunnel.PeerListDoc) ([]receiveBinding, error) {
		return prepareReceivePaths(devices, intent.MeshUID, root, previous, intent.Desired, intent.Native, 1400, 518, intent.Ports)
	}
	var receipt tunnel.ReceiveReady
	for attempt := 0; attempt < 2; attempt++ {
		current, err := api.CoreV1().Secrets(secret.Namespace).Get(context.Background(), secret.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := reconcileReceivePreparation(context.Background(), api, current, "worker", root, prepare); err != nil {
			t.Fatal(err)
		}
		published, err := api.CoreV1().Secrets(secret.Namespace).Get(context.Background(), secret.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var observed tunnel.ReceiveReady
		if err := json.Unmarshal(published.Data[tunnel.ReceiveReadyKey], &observed); err != nil {
			t.Fatal(err)
		}
		if observed.RequestHash != tunnel.HashPeerList(intentRaw) {
			t.Fatal("runtime published readiness for another request")
		}
		if attempt == 1 && !reflect.DeepEqual(receipt, observed) {
			t.Fatal("repeated preparation changed readiness")
		}
		receipt = observed
	}
	bindings := receipt.Bindings
	afterRoutes, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	afterRules, err := netlink.RuleList(netlink.FAMILY_ALL)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(routes, afterRoutes) || !reflect.DeepEqual(rules, afterRules) {
		t.Fatal("preparation changed experiment routing")
	}
	result, err := os.OpenFile(request.ResultFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(result).Encode(bindings); err != nil {
		result.Close()
		t.Fatal(err)
	}
	if err := result.Close(); err != nil {
		t.Fatal(err)
	}
}
