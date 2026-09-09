package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Called only after the parent canary verified namespace isolation and withdrew
// a real peer. HTTP responses are controlled test input, not a Kubernetes server.
func nativeAdoptionRecovery(t *testing.T, cfg config, deviceClient *wgctrl.Client, bootstrap tunnel.PeersFileDoc) {
	t.Helper()
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		t.Fatal(err)
	}
	if err = netlink.LinkSetUp(lo); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(cachePath(cfg), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	raw := []byte(`{"peers":[]}`)
	available := true
	ack := ""
	patches := 0
	expectedPeers := 0
	version := 1
	race := false
	conflicts := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if req.URL.Path != "/api/v1/namespaces/canary/secrets/adoption" {
			http.NotFound(w, req)
			return
		}
		if !available {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		if req.Method == http.MethodPatch {
			var patch struct {
				Metadata struct {
					Annotations     map[string]string `json:"annotations"`
					UID             string            `json:"uid"`
					ResourceVersion string            `json:"resourceVersion"`
				} `json:"metadata"`
			}
			if err := json.NewDecoder(req.Body).Decode(&patch); err != nil {
				http.Error(w, "bad patch", 400)
				return
			}
			if race {
				version++
				race = false
			}
			if patch.Metadata.UID != "canary-secret" || patch.Metadata.ResourceVersion != strconv.Itoa(version) {
				conflicts++
				http.Error(w, "Secret changed since read", http.StatusConflict)
				return
			}
			device, err := deviceClient.Device(cfg.iface)
			if err != nil || len(device.Peers) != expectedPeers {
				http.Error(w, "ack preceded native application", 409)
				return
			}
			ack = patch.Metadata.Annotations[tunnel.AppliedListAnnotation]
			patches++
		} else if req.Method != http.MethodGet {
			http.Error(w, "unexpected method", 405)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(corev1.Secret{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}, ObjectMeta: metav1.ObjectMeta{Name: "adoption", Namespace: "canary", UID: "canary-secret", ResourceVersion: strconv.Itoa(version), Annotations: map[string]string{tunnel.AppliedListAnnotation: ack}}, Data: map[string][]byte{tunnel.CloudPeersKey: raw}})
	}))
	defer api.Close()
	client, err := kubernetes.NewForConfig(&rest.Config{Host: api.URL})
	if err != nil {
		t.Fatal(err)
	}
	cfg.peersSecretNamespace = "canary"
	cfg.peersSecretName = "adoption"
	if err = reconcile(context.Background(), client, deviceClient, cfg); err != nil {
		t.Fatal(err)
	}
	cached, err := readCachedPeers(cachePath(cfg))
	if err != nil || cached.Peers == nil || len(cached.Peers) != 0 {
		t.Fatal("current update did not repair corrupt cache")
	}
	mu.Lock()
	validAck := ack == tunnel.HashPeerList(raw) && patches == 1
	available = false
	mu.Unlock()
	if !validAck {
		t.Fatal("empty update was not acknowledged after native apply")
	}
	if err = reconcile(context.Background(), client, deviceClient, cfg); err != nil {
		t.Fatal(err)
	}
	device, err := deviceClient.Device(cfg.iface)
	if err != nil || len(device.Peers) != 0 {
		t.Fatal("API outage resurrected bootstrap peer")
	}
	mu.Lock()
	raw, err = json.Marshal(tunnel.PeerListDoc{Peers: bootstrap.Peers})
	available = true
	expectedPeers = len(bootstrap.Peers)
	mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err = reconcile(context.Background(), client, deviceClient, cfg); err != nil {
		t.Fatal(err)
	}
	device, err = deviceClient.Device(cfg.iface)
	if err != nil {
		t.Fatal(err)
	}
	key, err := wgtypes.ParseKey(bootstrap.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(device.Peers) != 1 || device.PrivateKey != key {
		t.Fatal("re-addition changed identity or lost peer")
	}
	mu.Lock()
	validAck = ack == tunnel.HashPeerList(raw) && patches == 2
	mu.Unlock()
	if !validAck {
		t.Fatal("re-added peer was not acknowledged after native apply")
	}
	// Change the Secret version between GET and PATCH; the old acknowledgement
	// must be rejected, then a fresh reconciliation may acknowledge it.
	mu.Lock()
	raw = []byte(`{"peers":[]}`)
	expectedPeers = 0
	race = true
	ack = ""
	mu.Unlock()
	if err = reconcile(context.Background(), client, deviceClient, cfg); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	rejected := conflicts == 1 && ack == "" && patches == 2
	mu.Unlock()
	if !rejected {
		t.Fatal("stale Secret acknowledgement was accepted")
	}
	if err = reconcile(context.Background(), client, deviceClient, cfg); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	recovered := ack == tunnel.HashPeerList(raw) && patches == 3
	mu.Unlock()
	if !recovered {
		t.Fatal("current Secret version was not acknowledged on next reconciliation")
	}

}
