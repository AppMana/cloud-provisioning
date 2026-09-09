package bringup

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
)

// LocalHost is the machine this process runs on.
type LocalHost struct{ Lab string }

func (h LocalHost) Run(ctx context.Context, argv ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("%v: %w: %s", argv, err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

// InNamespace enters a node's network namespace and runs a command
// with this host's tools.
func (h LocalHost) InNamespace(ctx context.Context, node string, argv ...string) ([]byte, error) {
	pid, err := h.Run(ctx, "docker", "inspect", "-f", "{{.State.Pid}}", "clab-"+h.Lab+"-"+node)
	if err != nil {
		return nil, err
	}
	full := append([]string{"sudo", "nsenter", "-t", strings.TrimSpace(string(pid)), "-n"}, argv...)
	return h.Run(ctx, full...)
}

// PrepareHost does the parts of the lab that are not inside any node.
//
// The bridges are given no address on purpose: a host holding one on
// two of them would route between them, and the isolation the
// topology exists to model would be the assertion being wrong rather
// than the lab being right. The wan is the exception, because it is
// where this lab meets the real world and this host is its last hop.
func PrepareHost(ctx context.Context, t lab.Topology, h Host) error {
	for _, seg := range t.Segments {
		if _, err := h.Run(ctx, "sudo", "ip", "link", "add", "name", seg, "type", "bridge"); err != nil {
			// Already there is the ordinary case.
			_ = err
		}
		if _, err := h.Run(ctx, "sudo", "ip", "link", "set", seg, "up"); err != nil {
			return fmt.Errorf("raising %s: %w", seg, err)
		}
		if _, err := h.Run(ctx, "sudo", "ip", "addr", "flush", "dev", seg); err != nil {
			return fmt.Errorf("flushing %s: %w", seg, err)
		}
	}

	// A veth's host side can outlive its container: the outage rows
	// re-plumb NICs on reboot, and a pair created that way is not torn
	// down when the container is removed. With every lab container
	// gone, a link still enslaved to a lab bridge is by definition
	// stale, and containerlab refuses to deploy over a name that
	// already exists.
	if err := sweepStaleLinks(ctx, t, h); err != nil {
		return err
	}

	// br_netfilter has to be loaded here because a container cannot
	// load modules and the nodes need the files to exist: flannel
	// refuses to start when bridge-nf-call-iptables is absent. The
	// sysctls are per-namespace on this kernel, so the host's own
	// bridges keep their behaviour by pinning its value to 0 while
	// each node sets its own inside its namespace.
	if _, err := h.Run(ctx, "sudo", "modprobe", "br_netfilter"); err != nil {
		return fmt.Errorf("loading br_netfilter, which the nodes' networks require: %w", err)
	}
	if _, err := h.Run(ctx, "sudo", "sysctl", "-qw",
		"net.bridge.bridge-nf-call-iptables=0", "net.bridge.bridge-nf-call-ip6tables=0"); err != nil {
		return err
	}
	if _, err := h.Run(ctx, "sudo", "sysctl", "-qw", "net.ipv4.ip_forward=1"); err != nil {
		return err
	}

	// This host holds the wan's last address, masquerades what leaves,
	// and knows the way back into each cloud. That gives the site the
	// path it would really have (out through its router, translated,
	// and out again here) and a cloud node the one it would really
	// have (straight out from a public address). Neither creates a way
	// in: this host has no address on the site and no route to it.
	if _, err := h.Run(ctx, "sudo", "ip", "addr", "replace", lab.WANPrefix+".254/24", "dev", lab.WANSegment); err != nil {
		return err
	}

	uplink, err := defaultUplink(ctx, h)
	if err != nil {
		return err
	}
	for _, net := range []string{lab.WANPrefix + ".0/24", lab.CloudAPrefix + ".0/24", lab.CloudBPrefix + ".0/24"} {
		if _, err := h.Run(ctx, "sudo", "iptables", "-t", "nat", "-C", "POSTROUTING",
			"-s", net, "-o", uplink, "-j", "MASQUERADE"); err == nil {
			continue
		}
		if _, err := h.Run(ctx, "sudo", "iptables", "-t", "nat", "-A", "POSTROUTING",
			"-s", net, "-o", uplink, "-j", "MASQUERADE"); err != nil {
			return fmt.Errorf("masquerading %s: %w", net, err)
		}
	}
	// Replies to a cloud node have to find their way back to its cloud.
	for cidr, via := range map[string]string{
		lab.CloudAPrefix + ".0/24": lab.WANPrefix + ".2",
		lab.CloudBPrefix + ".0/24": lab.WANPrefix + ".3",
	} {
		if _, err := h.Run(ctx, "sudo", "ip", "route", "replace", cidr, "via", via, "dev", lab.WANSegment); err != nil {
			return fmt.Errorf("route to %s: %w", cidr, err)
		}
	}
	return nil
}

func sweepStaleLinks(ctx context.Context, t lab.Topology, h Host) error {
	for _, seg := range t.Segments {
		out, err := h.Run(ctx, "ip", "-o", "link", "show", "master", seg)
		if err != nil {
			continue // no bridge, nothing enslaved
		}
		for _, line := range strings.Split(string(out), "\n") {
			_, rest, ok := strings.Cut(line, ": ")
			if !ok {
				continue
			}
			name, _, _ := strings.Cut(rest, "@")
			name, _, _ = strings.Cut(name, ":")
			if name = strings.TrimSpace(name); name != "" {
				_, _ = h.Run(ctx, "sudo", "ip", "link", "del", name)
			}
		}
	}
	return nil
}

func defaultUplink(ctx context.Context, h Host) (string, error) {
	out, err := h.Run(ctx, "ip", "route", "show", "default")
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(out))
	for i, f := range fields {
		if f == "dev" && i+1 < len(fields) {
			return fields[i+1], nil
		}
	}
	return "", fmt.Errorf("this host has no default route, so the lab has no internet to reach")
}
