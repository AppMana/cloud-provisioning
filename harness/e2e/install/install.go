// Package install puts the product on the lab.
//
// The real chart, with helm, the way an operator installs it. Not a
// hand-assembled set of manifests: a harness that applies the pieces
// it happens to know about proves those pieces work and silently
// skips whatever the chart does that nobody remembered — an RBAC rule,
// a CRD, a default. The install path is part of what is under test.
//
// Nothing on the site pulls from a registry, because the site has no
// route to one. Every image and binary is built here and carried in.
package install

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/appmana/cloud-provisioning/harness/e2e/cluster"
	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

// Namespace is where the product lives.
const Namespace = "cloud-provisioning"

// Images the chart runs, built from this repository.
const (
	ControllerImage = "cldt-controller:e2e"
	DialerImage     = "cldt-dialer:e2e"
)

// DialerDistDir is where a node finds the first-boot binary. A remote
// has no image puller before it joins, so the chart hands it a URL and
// a digest; in the lab that URL is a file the node already has.
const DialerDistDir = "/opt/dialer-dist"

// Product installs this repository's chart onto the lab.
type Product struct {
	Kube     *kube.Client
	Rig      rig.Rig
	Images   cluster.Images
	Topology lab.Topology
	// RepoDir is the repository root, which is where the chart and
	// the Dockerfile are.
	RepoDir string
	// WorkDir is where built binaries land on this host.
	WorkDir string

	// binary is where Build left the first-boot binary.
	binary          string
	controllerImage string
	dialerImage     string
}

// binaryPath is where the first-boot binary is, whether or not Build
// ran in this process.
func (p *Product) BinaryPath() string {
	if p.binary != "" {
		return p.binary
	}
	abs, err := filepath.Abs(filepath.Join(p.WorkDir, "binaries", "wg-dialer-linux-amd64"))
	if err != nil {
		return filepath.Join(p.WorkDir, "binaries", "wg-dialer-linux-amd64")
	}
	return abs
}

// Options are the values a row varies.
type Options struct {
	// TunnelEndpoints is the selector deciding which site nodes
	// terminate tunnels. This is the one value a placement row
	// changes.
	TunnelEndpoints string
	// JoinProvider is the distribution a remote joins as.
	JoinProvider string
	// DialerURL is where a node fetches the first-boot binary, which
	// it has to be able to do before it has joined anything.
	DialerURL string
}

// Build compiles the images and the first-boot binary.
func (p *Product) Build(ctx context.Context) (dialerSHA string, err error) {
	for target, tag := range map[string]string{
		"dialer":              DialerImage,
		"endpoint-controller": ControllerImage,
	} {
		cmd := exec.CommandContext(ctx, "docker", "build", "-q",
			"--target", target, "-t", tag, "-f", "controller/Dockerfile", ".")
		cmd.Dir = p.RepoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("building %s: %w: %s", target, err, out)
		}
		id, err := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", tag).Output()
		if err != nil {
			return "", err
		}
		ref := strings.Split(tag, ":")[0] + ":" + strings.TrimPrefix(strings.TrimSpace(string(id)), "sha256:")
		if out, err := exec.CommandContext(ctx, "docker", "tag", tag, ref).CombinedOutput(); err != nil {
			return "", fmt.Errorf("tagging image: %w: %s", err, out)
		}
		if target == "endpoint-controller" {
			p.controllerImage = ref
		} else {
			p.dialerImage = ref
		}
	}

	// Absolute, because the build runs with its working directory in
	// the controller module: a relative output path would land
	// wherever that is rather than where the caller asked.
	binDir, err := filepath.Abs(filepath.Join(p.WorkDir, "binaries"))
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", err
	}
	binary := filepath.Join(binDir, "wg-dialer-linux-amd64")
	cmd := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/dialer")
	cmd.Dir = filepath.Join(p.RepoDir, "controller")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("building the dialer: %w: %s", err, out)
	}

	body, err := os.ReadFile(binary)
	if err != nil {
		return "", err
	}
	p.binary = binary
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

// Distribute carries the images and the first-boot binary onto the
// nodes that need them.
//
// The binary goes to every node that could ever be a remote, because
// which one is claimed is a row's choice and a node that is handed a
// URL it cannot read fails at first boot with nothing useful to say.
func (p *Product) Distribute(ctx context.Context, dialerSHA string) error {
	var site []string
	for _, n := range cluster.SiteNodes(p.Topology) {
		site = append(site, n.Name)
	}
	// The site's nodes only. A remote has no container runtime of its
	// own until it joins, and the one it then has belongs to the
	// distribution — so its images arrive after the join, through
	// LoadOnto.
	for _, image := range []string{p.controllerRef(), p.dialerRef()} {
		if err := p.Images.Load(ctx, image, site, nil); err != nil {
			return fmt.Errorf("carrying %s in: %w", image, err)
		}
	}

	// The first-boot binary is not staged here.
	//
	// A remote is launched as a new instance to read its userdata, so
	// anything put on the old instance's disk goes with it. The node
	// fetches the binary from the URL the chart gives it, which is
	// what that value is for and what the lab's own segments already
	// prove it can reach.
	return nil
}

// LoadOnto carries the product's own images onto nodes that have
// only just acquired a runtime.
//
// The dialer runs on a remote as its own DaemonSet, and that copy is
// what keeps the remote's peer list current from the adoption cache
// once it has joined. A remote without the image joins, goes Ready,
// and then reaches nothing across the tunnel, because the pod that
// would maintain its routes is stuck pulling from a registry its
// cloud cannot reach.
func (p *Product) LoadOnto(ctx context.Context, nodes []string) error {
	if err := p.Images.Load(ctx, p.dialerRef(), nodes, nil); err != nil {
		return fmt.Errorf("carrying %s in: %w", DialerImage, err)
	}
	return nil
}

// Install runs helm from the bastion.
//
// From the bastion because this host cannot reach the API server and
// should not be able to. helm is carried in rather than downloaded,
// for the same reason nothing else is downloaded there.
func (p *Product) Install(ctx context.Context, opts Options, dialerSHA string) error {
	if err := p.carryHelm(ctx); err != nil {
		return err
	}
	if err := p.carryChart(ctx); err != nil {
		return err
	}

	// The real values an operator sets, and nothing else. imagePullPolicy
	// is Never because the site has no registry; the rest is what the
	// row varies.
	args := []string{
		"upgrade", "--install", "cloud-provisioning", "/tmp/chart",
		"--namespace", Namespace, "--create-namespace", "--wait", "--timeout", "6m",
		"--set", "image.repository=" + strings.Split(p.controllerRef(), ":")[0],
		"--set", "image.tag=" + strings.Split(p.controllerRef(), ":")[1],
		"--set", "image.pullPolicy=Never",
		"--set", "dialerImage.pullPolicy=Never",
		"--set", "dialerImage.repository=" + strings.Split(p.dialerRef(), ":")[0],
		"--set", "dialerImage.tag=" + strings.Split(p.dialerRef(), ":")[1],
		"--set-string", "tunnel.endpoints=" + escapeHelmValue(opts.TunnelEndpoints),
		"--set", "joinProvider=" + opts.JoinProvider,
		"--set", "dialerBinary.amd64.url=" + opts.DialerURL,
		"--set", "dialerBinary.amd64.sha256=" + dialerSHA,
	}
	out, err := p.Kube.Helm(ctx, args...)
	if err != nil {
		return fmt.Errorf("installing the chart: %w: %s", err, out)
	}
	return nil
}

// escapeHelmValue protects the commas inside a set-based selector.
//
// helm reads a comma in --set as the separator between values, so
// `kubernetes.io/hostname in (w1,w2)` becomes two values and the
// release fails to install. The chart's own README says the same.
func escapeHelmValue(v string) string {
	return strings.ReplaceAll(v, ",", `\,`)
}

func (p *Product) carryHelm(ctx context.Context) error {
	path, err := exec.LookPath("helm")
	if err != nil {
		return fmt.Errorf("no helm on this host to carry in: %w", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return p.Kube.Bastion.Put(ctx, bytes.NewReader(body), "/usr/local/bin/helm", 0o755)
}

// carryChart copies this repository's chart onto the bastion.
//
// The chart as it ships, not a rendering of it: what is under test
// includes whether the chart installs.
func (p *Product) carryChart(ctx context.Context) error {
	root, err := p.stageChart(ctx)
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	chart := filepath.Join(root, "cloud-provisioning")
	var buf bytes.Buffer
	cmd := exec.CommandContext(ctx, "tar", "-C", filepath.Dir(chart), "-cf", "-", filepath.Base(chart))
	cmd.Stdout = &buf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("packing the chart: %w", err)
	}
	if _, err := p.Kube.Bastion.Exec(ctx, "mkdir", "-p", "/tmp/chartsrc"); err != nil {
		return err
	}
	if _, err := p.Kube.Bastion.Pipe(ctx, &buf, "tar", "-C", "/tmp/chartsrc", "-xf", "-"); err != nil {
		return fmt.Errorf("unpacking the chart: %w", err)
	}
	if _, err := p.Kube.Bastion.Exec(ctx, "sh", "-c",
		"rm -rf /tmp/chart && mv /tmp/chartsrc/cloud-provisioning /tmp/chart"); err != nil {
		return err
	}
	return nil
}

func (p *Product) controllerRef() string {
	if p.controllerImage != "" {
		return p.controllerImage
	}
	return ControllerImage
}
func (p *Product) dialerRef() string {
	if p.dialerImage != "" {
		return p.dialerImage
	}
	return DialerImage
}
