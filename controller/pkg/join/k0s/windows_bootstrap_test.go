package k0s

import (
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/bootstrap"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func windowsValues(t *testing.T) map[string]any {
	t.Helper()
	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(tunnel.PeersFileDoc{PrivateKey: key.String(), LocalAddress: "10.100.0.128/24", Peers: []tunnel.PeerSpec{}})
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"machineName": "windows-worker", "interfaceName": "cldt1234abcd", "k0sVersion": "v1.36.2+k0s.0", "joinToken": "token-with-'quotes' & Ω", "apiEndpoint": "10.0.0.1:6443", "wireguardListenPort": "51820", "peersFileJSON": string(raw), "nodeAddress": "192.0.2.3", "providerID": "test:///worker"}
}

func TestWindowsBootstrapCarriesNativeIdentityWithoutShellInterpolation(t *testing.T) {
	values := windowsValues(t)
	result, err := (WindowsBootstrap{ServiceSHA256: strings.Repeat("a", 64)}).Render(values)
	if err != nil {
		t.Fatal(err)
	}
	if result.Format != bootstrap.PowerShell {
		t.Fatal("Windows received a non-native format")
	}
	if strings.Contains(string(result.Value), values["joinToken"].(string)) {
		t.Fatal("credential interpolated directly into PowerShell code")
	}
	match := regexp.MustCompile(`FromBase64String\('([^']+)'\)`).FindSubmatch(result.Value)
	if len(match) != 2 {
		t.Fatal("encoded bootstrap plan missing")
	}
	raw, err := base64.StdEncoding.DecodeString(string(match[1]))
	if err != nil {
		t.Fatal(err)
	}
	var plan windowsPlan
	if err = json.Unmarshal(raw, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.JoinToken != values["joinToken"] || plan.Peers.LocalAddress != "10.100.0.128/32" || plan.ProviderID != "test:///worker" {
		t.Fatal("native identity or credential changed")
	}
}

func TestWindowsBootstrapRejectsMissingImageOrInfrastructureContract(t *testing.T) {
	values := windowsValues(t)
	if _, err := (WindowsBootstrap{}).Render(values); err == nil {
		t.Fatal("unverified image accepted")
	}
	delete(values, "providerID")
	if _, err := (WindowsBootstrap{ServiceSHA256: strings.Repeat("a", 64)}).Render(values); err == nil {
		t.Fatal("missing provider identity accepted")
	}
	values["providerIdentityPowerShell"] = base64.StdEncoding.EncodeToString([]byte("@{providerID='test:///worker';nodeAddress='192.0.2.3'}"))
	delete(values, "nodeAddress")
	if _, err := (WindowsBootstrap{ServiceSHA256: strings.Repeat("a", 64)}).Render(values); err != nil {
		t.Fatal("runtime identity provider rejected:", err)
	}
}

func TestWindowsBootstrapPinsPatchedBuildOfSameRelease(t *testing.T) {
	w := WindowsBootstrap{ServiceSHA256: strings.Repeat("a", 64), WorkerVersion: "v1.36.2+k0s.0.appmana.1", WorkerSHA256: strings.Repeat("B", 64)}
	result, err := w.Render(windowsValues(t))
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`FromBase64String\('([^']+)'\)`).FindSubmatch(result.Value)
	raw, err := base64.StdEncoding.DecodeString(string(match[1]))
	if err != nil {
		t.Fatal(err)
	}
	var plan windowsPlan
	if err := json.Unmarshal(raw, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.K0sVersion != w.WorkerVersion || plan.WorkerSHA256 != strings.ToLower(w.WorkerSHA256) {
		t.Fatal("worker identity was not pinned in bootstrap data")
	}
	script := string(result.Value)
	guard := strings.Index(script, "Baked k0s executable checksum mismatch")
	if guard < 0 || guard > strings.Index(script, "New-Item -ItemType Directory -Path $mesh") {
		t.Fatal("worker integrity check occurs after identity creation")
	}
	for _, version := range []string{"v1.36.3+k0s.0.appmana.1", "v1.36.2+k0s.1.appmana.1", "v1.36.2+k0s.00.appmana.1", "v1.36.2+k0s.0.;bad", ""} {
		bad := w
		bad.WorkerVersion = version
		if _, err := bad.Render(windowsValues(t)); err == nil {
			t.Errorf("accepted mismatched/invalid worker %q", version)
		}
	}
	w.WorkerSHA256 = ""
	if _, err := w.Render(windowsValues(t)); err == nil {
		t.Fatal("accepted unpinned worker override")
	}
}

func TestWindowsBootstrapConsumesPublishedRemotePeers(t *testing.T) {
	values := windowsValues(t)
	peer, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	published := map[string][]byte{
		tunnel.NodePublicKeyPrefix + "w1":     []byte(peer.PublicKey().String()),
		tunnel.NodeTunnelAddressPrefix + "w1": []byte("10.100.0.1/24"),
	}
	peers, err := tunnel.RemotePeers(published, "10.100.0.128", []string{"10.10.0.10"})
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) == 0 {
		t.Fatal("production peer builder returned no endpoints")
	}
	var doc tunnel.PeersFileDoc
	if err := json.Unmarshal([]byte(values["peersFileJSON"].(string)), &doc); err != nil {
		t.Fatal(err)
	}
	doc.Peers = peers
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	values["peersFileJSON"] = string(raw)
	if _, err := (WindowsBootstrap{ServiceSHA256: strings.Repeat("a", 64)}).Render(values); err != nil {
		t.Fatal(err)
	}
}
