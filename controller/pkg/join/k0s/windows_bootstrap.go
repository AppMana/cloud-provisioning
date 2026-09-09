package k0s

import (
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/bootstrap"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunneldevice"
)

//go:embed windows_bootstrap.ps1
var windowsScript string

// WindowsBootstrap consumes prepared image assets. Platform identity comes
// from infrastructure values; this distribution renderer has no cloud API code.
type WindowsBootstrap struct {
	ServiceSHA256 string
	WorkerVersion string
	WorkerSHA256  string
}

type windowsPlan struct {
	MachineName        string              `json:"machineName"`
	Interface          string              `json:"interface"`
	K0sVersion         string              `json:"k0sVersion"`
	WorkerSHA256       string              `json:"workerSHA256"`
	JoinToken          string              `json:"joinToken"`
	KubeletExtraArgs   string              `json:"kubeletExtraArgs"`
	APIEndpoint        string              `json:"apiEndpoint"`
	NodeAddress        string              `json:"nodeAddress"`
	ProviderID         string              `json:"providerID"`
	IdentityPowerShell string              `json:"identityPowerShell"`
	ServiceSHA256      string              `json:"serviceSHA256"`
	ListenPort         uint16              `json:"listenPort"`
	Peers              tunnel.PeersFileDoc `json:"peers"`
}

func (w WindowsBootstrap) Render(values map[string]any) (bootstrap.Data, error) {
	str := func(key string) string { v, _ := values[key].(string); return v }
	p := windowsPlan{MachineName: str("machineName"), Interface: str("interfaceName"), K0sVersion: str("k0sVersion"), JoinToken: str("joinToken"), KubeletExtraArgs: str("kubeletExtraArgs"), APIEndpoint: str("apiEndpoint"), NodeAddress: str("nodeAddress"), ProviderID: str("providerID"), IdentityPowerShell: str("providerIdentityPowerShell"), ServiceSHA256: w.ServiceSHA256}
	if !regexp.MustCompile(`^[a-fA-F0-9]{64}$`).MatchString(w.ServiceSHA256) {
		return bootstrap.Data{}, fmt.Errorf("Windows image requires a pinned tunnel service SHA256")
	}
	if !regexp.MustCompile(`^cldt[0-9a-f]{8}$`).MatchString(p.Interface) {
		return bootstrap.Data{}, fmt.Errorf("invalid Windows mesh interface")
	}
	if p.MachineName == "" || p.JoinToken == "" || p.K0sVersion == "" || p.APIEndpoint == "" {
		return bootstrap.Data{}, fmt.Errorf("missing Windows k0s join values")
	}
	if w.WorkerVersion != "" || w.WorkerSHA256 != "" {
		if !regexp.MustCompile(`^[a-fA-F0-9]{64}$`).MatchString(w.WorkerSHA256) ||
			!regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+\+k0s\.[0-9]+(?:\.[A-Za-z0-9-]+)*$`).MatchString(w.WorkerVersion) {
			return bootstrap.Data{}, fmt.Errorf("Windows worker override requires an exact version and SHA256")
		}
		if w.WorkerVersion != p.K0sVersion && !strings.HasPrefix(w.WorkerVersion, p.K0sVersion+".") {
			return bootstrap.Data{}, fmt.Errorf("Windows worker build must retain the cluster's base k0s release")
		}
		p.K0sVersion = w.WorkerVersion
		p.WorkerSHA256 = strings.ToLower(w.WorkerSHA256)
	}
	if p.ProviderID == "" && p.IdentityPowerShell == "" {
		return bootstrap.Data{}, fmt.Errorf("Windows bootstrap requires infrastructure identity")
	}
	if p.NodeAddress == "" && p.IdentityPowerShell == "" {
		return bootstrap.Data{}, fmt.Errorf("Windows bootstrap requires infrastructure node address")
	}
	if p.IdentityPowerShell != "" {
		if _, err := base64.StdEncoding.DecodeString(p.IdentityPowerShell); err != nil {
			return bootstrap.Data{}, fmt.Errorf("invalid infrastructure PowerShell encoding")
		}
	}
	port, err := strconv.ParseUint(str("wireguardListenPort"), 10, 16)
	if err != nil || port == 0 {
		return bootstrap.Data{}, fmt.Errorf("invalid Windows WireGuard port")
	}
	p.ListenPort = uint16(port)
	if err = json.Unmarshal([]byte(str("peersFileJSON")), &p.Peers); err != nil {
		return bootstrap.Data{}, fmt.Errorf("invalid Windows peers document")
	}
	address, err := netip.ParsePrefix(p.Peers.LocalAddress)
	if err != nil {
		return bootstrap.Data{}, err
	}
	// Windows installs only explicit host routes; a shared tunnel /24 must not
	// install a connected subnet route on every remote.
	p.Peers.LocalAddress = netip.PrefixFrom(address.Addr(), address.Addr().BitLen()).String()
	if _, err = tunneldevice.Compile(p.Peers); err != nil {
		return bootstrap.Data{}, err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return bootstrap.Data{}, err
	}
	script := strings.ReplaceAll(windowsScript, "__PLAN_BASE64__", base64.StdEncoding.EncodeToString(raw))
	return bootstrap.Data{Format: bootstrap.PowerShell, Value: []byte(script)}, nil
}
