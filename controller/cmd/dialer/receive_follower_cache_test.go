package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func followerCacheFixture(t *testing.T) (config, receiveInventoryCheckpoint, []string, string, string) {
	t.Helper()
	key := func() string {
		k, err := wgtypes.GeneratePrivateKey()
		if err != nil {
			t.Fatal(err)
		}
		return k.PublicKey().String()
	}
	cfg := config{nodeName: "follower", iface: "owned-wg", routeTable: 518, fwmark: 518}
	expected := receiveInventoryCheckpoint{Version: 1, InventoryUID: "inventory", Inventory: tunnel.ReceiveInventory{MeshUID: "mesh"}, NodeName: cfg.nodeName, NodeUID: "node", BootID: "boot", PublicKey: key()}
	return cfg, expected, []string{"10.10.0.4", "fd10::4"}, key(), key()
}

// Filesystem intent and stale-write tests only. No API authority, kernel
// installation, actual crash, retirement completion or boot recovery is proved.
func TestFollowerRouteIntentRetainsInterruptedVariants(t *testing.T) {
	cfg, expected, own, first, second := followerCacheFixture(t)
	path := filepath.Join(t.TempDir(), "follower.json")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	original := map[string]map[string]string{
		first:  {"172.29.0.1/32": "10.10.0.1", "fd29::1/128": "fd10::1"},
		second: {"172.29.0.2/32": "10.10.0.2"},
	}
	old, err := stageReceiveFollowerRoutes(path, cfg, expected, own, receiveFollowerCache{}, original)
	must(err)
	desired := map[string]map[string]string{first: {"172.29.0.1/32": "10.10.0.3", "172.29.0.3/32": "10.10.0.3", "fd29::1/128": "fd10::3"}}
	staged, err := stageReceiveFollowerRoutes(path, cfg, expected, own, old, desired)
	must(err)
	if len(staged.Routes[first]) != 2 || len(staged.Routes[second]) != 1 || !reflect.DeepEqual(staged.Routes[first][0], original[first]) {
		t.Fatal("staging lost an interrupted gateway or omitted worker")
	}
	reloaded, err := readReceiveFollowerCache(path, cfg, expected, own)
	must(err)
	retry, err := stageReceiveFollowerRoutes(path, cfg, expected, own, reloaded, desired)
	must(err)
	if !reflect.DeepEqual(retry, staged) {
		t.Fatal("retry duplicated route evidence")
	}
	unchanged := func(action func() error) {
		t.Helper()
		before, err := os.ReadFile(path)
		must(err)
		if err := action(); err == nil {
			t.Fatal("unsafe intent replacement accepted")
		}
		after, err := os.ReadFile(path)
		must(err)
		if string(before) != string(after) {
			t.Fatal("rejected operation changed durable ownership")
		}
	}
	unchanged(func() error {
		_, err := stageReceiveFollowerRoutes(path, cfg, expected, own, receiveFollowerCache{}, desired)
		return err
	})
	unchanged(func() error {
		_, err := stageReceiveFollowerRoutes(path, cfg, expected, own, old, desired)
		return err
	})
	unchanged(func() error {
		_, err := stageReceiveFollowerRoutes(path, cfg, expected, own, staged, map[string]map[string]string{first: {"172.29.0.2/32": "10.10.0.1"}})
		return err
	})
	unchanged(func() error {
		_, err := finishReceiveFollowerRoutes(path, cfg, expected, own, staged, map[string]map[string]string{first: {"172.29.0.99/32": "10.10.0.1"}})
		return err
	})
	finished, err := finishReceiveFollowerRoutes(path, cfg, expected, own, staged, desired)
	must(err)
	if len(finished.Routes[first]) != 1 || !reflect.DeepEqual(finished.Routes[first][0], desired[first]) || !reflect.DeepEqual(finished.Routes[second], staged.Routes[second]) {
		t.Fatal("verified compaction changed an unmentioned worker")
	}
	desired[first]["172.29.0.1/32"] = "10.10.0.99"
	if finished.Routes[first][0]["172.29.0.1/32"] != "10.10.0.3" || staged.Routes[first][1]["172.29.0.1/32"] != "10.10.0.3" {
		t.Fatal("caller mutation changed recorded route evidence")
	}
}

func TestFollowerRouteCacheRejectsInvalidScopeAndOwnership(t *testing.T) {
	cfg, expected, own, first, second := followerCacheFixture(t)
	path := filepath.Join(t.TempDir(), "follower.json")
	saved, err := stageReceiveFollowerRoutes(path, cfg, expected, own, receiveFollowerCache{}, map[string]map[string]string{first: {"172.29.0.1/32": "10.10.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(saved)
	for _, fault := range []string{"none", "previous-boot", "inventory", "mesh", "node", "identity", "table", "mark", "own", "default", "family", "recursive", "overlap", "empty-variant", "nil-routes", "zero-worker", "unknown-field", "trailing"} {
		t.Run(fault, func(t *testing.T) {
			var broken receiveFollowerCache
			if err := json.Unmarshal(encoded, &broken); err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "previous-boot":
				broken.BootID = "old-boot"
			case "inventory":
				broken.InventoryUID = "replacement"
			case "mesh":
				broken.MeshUID = "replacement"
			case "node":
				broken.NodeUID = "replacement"
			case "identity":
				broken.PublicKey = second
			case "table":
				broken.Table++
			case "mark":
				broken.Mark++
			case "own":
				broken.Own = []string{"10.10.0.99"}
			case "default":
				broken.Routes[first] = []map[string]string{{"0.0.0.0/0": "10.10.0.1"}}
			case "family":
				broken.Routes[first][0]["172.29.0.1/32"] = "fd10::1"
			case "recursive":
				broken.Routes[first][0]["172.29.0.1/32"] = "172.29.0.1"
			case "overlap":
				broken.Routes[second] = []map[string]string{{"172.29.0.0/24": "10.10.0.2"}}
			case "empty-variant":
				broken.Routes[first] = []map[string]string{{}}
			case "nil-routes":
				broken.Routes = nil
			case "zero-worker":
				broken.Routes[(wgtypes.Key{}).String()] = broken.Routes[first]
				delete(broken.Routes, first)
			}
			raw, _ := json.Marshal(broken)
			if fault == "unknown-field" {
				raw = append(raw[:len(raw)-1], []byte(`,"unexpected":true}`)...)
			} else if fault == "trailing" {
				raw = append(raw, []byte(`{}`)...)
			}
			_, err := decodeReceiveFollowerCache(raw, cfg, expected, own)
			if (err == nil) != (fault == "none" || fault == "previous-boot") {
				t.Fatalf("ownership decode: %v", err)
			}
		})
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readReceiveFollowerCache(path, cfg, expected, own); err == nil {
		t.Fatal("public cache permissions accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := path + ".link"
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readReceiveFollowerCache(link, cfg, expected, own); err == nil {
		t.Fatal("symlink cache accepted")
	}
}

// A completed withdrawal must survive restart and cannot be erased by a stale
// writer or downgrade. The addresses become reusable; the withdrawn key does not.
func TestFollowerWithdrawalFenceSurvivesAddressReuse(t *testing.T) {
	cfg, expected, own, oldRoot, newRoot := followerCacheFixture(t)
	path := filepath.Join(t.TempDir(), "follower.json")
	routes := map[string]string{"172.29.0.1/32": "10.10.0.1"}
	before, err := stageReceiveFollowerRoutes(path, cfg, expected, own, receiveFollowerCache{}, map[string]map[string]string{oldRoot: routes})
	if err != nil {
		t.Fatal(err)
	}
	retired := before
	retired.Version = 2
	retired.Routes = map[string][]map[string]string{}
	retired.Retired = map[string]bool{oldRoot: true}
	if err := replaceReceiveFollowerCache(path, cfg, expected, own, before, retired); err != nil {
		t.Fatal(err)
	}
	loaded, err := readReceiveFollowerCache(path, cfg, expected, own)
	if err != nil {
		t.Fatal(err)
	}
	reused, err := stageReceiveFollowerRoutes(path, cfg, expected, own, loaded, map[string]map[string]string{newRoot: routes})
	if err != nil {
		t.Fatal(err)
	}
	reject := func(action func() error) {
		t.Helper()
		original, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if action() == nil {
			t.Fatal("withdrawn ownership restored or forgotten")
		}
		after, err := os.ReadFile(path)
		if err != nil || string(after) != string(original) {
			t.Fatal("rejected write changed durable ownership")
		}
	}
	reject(func() error {
		_, err := stageReceiveFollowerRoutes(path, cfg, expected, own, reused, map[string]map[string]string{oldRoot: {"172.29.0.9/32": "10.10.0.1"}})
		return err
	})
	reject(func() error {
		downgrade := reused
		downgrade.Version, downgrade.Retired = 1, nil
		return replaceReceiveFollowerCache(path, cfg, expected, own, reused, downgrade)
	})
	reject(func() error { return replaceReceiveFollowerCache(path, cfg, expected, own, before, before) })
	for _, fault := range []string{"v1-marker", "v2-empty", "false-marker"} {
		t.Run(fault, func(t *testing.T) {
			invalid := reused
			switch fault {
			case "v1-marker":
				invalid.Version = 1
			case "v2-empty":
				invalid.Retired = nil
			case "false-marker":
				invalid.Retired = map[string]bool{oldRoot: false}
			}
			raw, err := json.Marshal(invalid)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := decodeReceiveFollowerCache(raw, cfg, expected, own); err == nil {
				t.Fatal("invalid withdrawal marker accepted")
			}
		})
	}
}
