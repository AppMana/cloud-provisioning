package main

import (
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
)

// TestNativeReceiveExchange is a test-binary adapter for the isolated packet
// experiment. Only public bindings leave the process; root material stays in
// the experiment's private temporary directory. The parent owns topology and
// traffic assertions. This adapter is never part of the shipped dialer binary.
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
	bindings, err := prepareReceivePaths(devices, request.MeshID, root, request.Previous, request.Desired, request.Native, 1400, 518)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := prepareReceivePaths(devices, request.MeshID, root, request.Previous, request.Desired, request.Native, 1400, 518)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(bindings, repeated) {
		t.Fatal("repeated preparation changed live bindings")
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
