package main

import (
	"encoding/json"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// TestNativeSiteReceiveExchange adapts the isolated packet experiment to the
// site preparation runtime. The parent supplies cloud bindings and traffic.
func TestNativeSiteReceiveExchange(t *testing.T) {
	requestPath := os.Getenv("CLDT_NATIVE_SITE_RECEIVE_EXCHANGE")
	if requestPath == "" {
		t.Skip("requires isolated site packet experiment")
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
		if link.Attrs().Name != "lo" && link.Attrs().Name != "outer" && link.Attrs().Name != "inner" {
			t.Fatal("unexpected site experiment link")
		}
	}
	var request struct {
		MeshID     string                `json:"meshID"`
		CloudRoot  string                `json:"cloudRoot"`
		Binding    tunnel.ReceiveBinding `json:"binding"`
		Previous   tunnel.PeerListDoc    `json:"previous"`
		Desired    tunnel.PeerListDoc    `json:"desired"`
		KeyFile    string                `json:"keyFile"`
		ResultFile string                `json:"resultFile"`
	}
	file, err := os.Open(requestPath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		t.Fatal("invalid site experiment request")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatal("trailing site experiment input")
	}
	raw, err := os.ReadFile(request.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	key, err := wgtypes.ParseKey(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal("invalid site experiment identity")
	}
	devices, err := wgctrl.New()
	if err != nil {
		t.Fatal(err)
	}
	defer devices.Close()
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := netlink.RuleList(netlink.FAMILY_ALL)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := prepareSiteReceiveClient(devices, request.MeshID, key, request.CloudRoot, request.Binding, request.Previous, request.Desired, 1400, 518)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := prepareSiteReceiveClient(devices, request.MeshID, key, request.CloudRoot, request.Binding, request.Previous, request.Desired, 1400, 518)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan, repeated) {
		t.Fatal("repeated site preparation changed binding")
	}
	changed := request.Binding
	changed.ListenPort++
	if _, err := prepareSiteReceiveClient(devices, request.MeshID, key, request.CloudRoot, changed, request.Previous, request.Desired, 1400, 518); err == nil {
		t.Fatal("live site endpoint changed without transition")
	}
	afterRoutes, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	afterRules, err := netlink.RuleList(netlink.FAMILY_ALL)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(routes, afterRoutes) || !reflect.DeepEqual(rules, afterRules) {
		t.Fatal("site preparation changed routing")
	}
	result, err := os.OpenFile(request.ResultFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(result).Encode(plan); err != nil {
		result.Close()
		t.Fatal(err)
	}
	if err := result.Close(); err != nil {
		t.Fatal(err)
	}
}

// Verify the transport observation after the parent packet experiment has
// exchanged traffic, without changing any device or routing state.
func TestNativeSiteReceiveHandshake(t *testing.T) {
	path := os.Getenv("CLDT_NATIVE_SITE_RECEIVE_HANDSHAKE")
	if path == "" {
		t.Skip("requires isolated site packet experiment")
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
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		KeyFile    string `json:"keyFile"`
		ResultFile string `json:"resultFile"`
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(request.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	key, err := wgtypes.ParseKey(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal("invalid site identity")
	}
	raw, err = os.ReadFile(request.ResultFile)
	if err != nil {
		t.Fatal(err)
	}
	var plan tunnel.SiteReceiveClient
	if err := json.Unmarshal(raw, &plan); err != nil {
		t.Fatal(err)
	}
	devices, err := wgctrl.New()
	if err != nil {
		t.Fatal(err)
	}
	defer devices.Close()
	handshake, err := siteReceiveHandshake(devices, key, plan, 518, time.Now())
	if err != nil || handshake.IsZero() {
		t.Fatalf("prepared transport has no current handshake: %v", err)
	}
}
