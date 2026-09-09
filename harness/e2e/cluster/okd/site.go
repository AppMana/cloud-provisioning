package okd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig/coreos"
)

// StartDNS supplies the API and ingress names required by the official
// bare-metal installer, using the existing site's bastion namespace.
func (a Assets) StartDNS(ctx context.Context, r *coreos.Rig) error {
	config := "no-resolv\nserver=9.9.9.9\nbind-interfaces\nlisten-address=10.10.0.2\n"
	config += "address=/api." + Domain + "/10.10.0.100\naddress=/api-int." + Domain + "/10.10.0.100\naddress=/apps." + Domain + "/10.10.0.101\n"
	for _, n := range r.Topology.NodesInRole(lab.ControlPlane) {
		config += fmt.Sprintf("host-record=%s.%s,%s,%s\n", n.Name, Domain, n.Name, n.Address(lab.LANSegment))
	}
	path, err := filepath.Abs(filepath.Join(a.WorkDir, "dnsmasq.conf"))
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		return err
	}
	// This helper belongs exclusively to the cldt OKD site.
	_ = exec.CommandContext(ctx, "docker", "rm", "-f", "cldt-okd-dns").Run()
	out, err := exec.CommandContext(ctx, "docker", "run", "-d", "--name", "cldt-okd-dns",
		"--network", "container:clab-"+r.Topology.Name+"-bastion", "--cap-add", "NET_ADMIN",
		"-v", path+":/etc/cldt-dnsmasq.conf:ro", "--entrypoint", "dnsmasq", InstallerImage,
		"--keep-in-foreground", "--conf-file=/etc/cldt-dnsmasq.conf").CombinedOutput()
	if err != nil {
		return fmt.Errorf("starting site DNS: %w: %s", err, out)
	}
	return r.Node("bastion").Put(ctx, strings.NewReader("nameserver 10.10.0.2\n"), "/etc/resolv.conf", 0644)
}

// WaitInstall observes the official installer's completion gates. The helper
// joins the bastion's namespace; no API access is added to the host or guests.
func (a Assets) WaitInstall(ctx context.Context, r *coreos.Rig, k *kube.Client) error {
	work, err := filepath.Abs(a.WorkDir)
	if err != nil {
		return err
	}
	tools, err := filepath.Abs(a.ToolsDir)
	if err != nil {
		return err
	}
	resolver := filepath.Join(work, "resolv.conf")
	if err := os.WriteFile(resolver, []byte("nameserver 10.10.0.2\n"), 0644); err != nil {
		return err
	}
	for _, gate := range []string{"bootstrap-complete", "install-complete"} {
		name := "cldt-okd-" + gate
		log, err := os.OpenFile(filepath.Join(work, gate+".log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--name", name,
			"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
			"--network", "container:clab-"+r.Topology.Name+"-bastion",
			"-v", work+":/assets", "-v", tools+":/tools:ro", "-v", resolver+":/etc/resolv.conf:ro",
			InstallerImage, "agent", "wait-for", gate, "--dir", "/assets", "--log-level", "debug")
		cmd.Stdout, cmd.Stderr = log, log
		err = cmd.Run()
		log.Close()
		removeTool(name)
		if err != nil {
			return fmt.Errorf("OKD %s: %w; see %s", gate, err, log.Name())
		}
	}
	if err := r.Installed(); err != nil {
		return err
	}
	config, err := os.ReadFile(filepath.Join(work, "auth", "kubeconfig"))
	if err != nil {
		return err
	}
	return k.Install(ctx, config)
}
