// Package kube talks to the cluster the way an operator would: from
// the bastion, which is on the site network.
//
// Not from this host. This host has no address on the site and no
// route to it, and it must stay that way — the property the whole lab
// rests on is that the site is private, so a harness that could reach
// the API server directly would be proving something weaker than it
// claims.
//
// The bastion is not a cluster node: nothing serves its loopback and
// it runs no forwarder, so every call picks a live control plane
// first. That was a shell script written into the container; here it
// is a decision this package makes, with the reasoning attached and a
// test that pins it.
package kube

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

// Client runs kubectl on the bastion against whichever control plane
// is currently able to answer.
type Client struct {
	Bastion rig.Node
	// ControlPlanes are the real addresses of the members, which are
	// in every server certificate's SANs, so naming one needs no other
	// accommodation.
	ControlPlanes []string
	// APIPort is the distribution's serving port; zero retains 6443.
	APIPort int
}

// ServingPort is shared by API access and the infrastructure endpoint contract.
func (c *Client) ServingPort() int {
	if c.APIPort == 0 {
		return 6443
	}
	return c.APIPort
}

// Server picks a control plane that is ready to serve.
//
// The probe is an AUTHENTICATED /readyz, and nothing weaker
// distinguishes ready from not. /livez passes while a returning
// member's authorizer is still syncing, and every request then comes
// back Forbidden. An anonymous /readyz is 401 on k0s before readiness
// is ever consulted, so every member fails the probe and the caller
// falls through to the kubeconfig's default server — which is exactly
// the member that was down. That is not a hypothetical: the harness
// went blind that way and reported that a node never went NotReady.
//
// A probe failure means connectivity or a member that cannot serve
// yet, so trying the next one is right. A kubectl failure after a
// good probe is an answer, not a reason to ask someone else.
func (c *Client) Server(ctx context.Context) (string, error) {
	port := c.ServingPort()
	for _, addr := range c.ControlPlanes {
		server := "https://" + net.JoinHostPort(addr, strconv.Itoa(port))
		_, err := c.Bastion.Exec(ctx, "kubectl", "--server="+server,
			"--request-timeout=2s", "get", "--raw", "/readyz")
		if err == nil {
			return server, nil
		}
	}
	return "", fmt.Errorf("no control plane answered an authenticated /readyz: %v", c.ControlPlanes)
}

// Run executes kubectl against a ready member.
func (c *Client) Run(ctx context.Context, args ...string) ([]byte, error) {
	server, err := c.Server(ctx)
	if err != nil {
		return nil, err
	}
	return c.Bastion.Exec(ctx, append([]string{"kubectl", "--server=" + server}, args...)...)
}

// Apply sends a manifest through stdin.
//
// Through Pipe, which attaches it. Applying with an unattached stdin
// reads nothing, applies nothing, and reports success.
func (c *Client) Apply(ctx context.Context, manifest []byte) error {
	server, err := c.Server(ctx)
	if err != nil {
		return err
	}
	out, err := c.Bastion.Pipe(ctx, bytes.NewReader(manifest),
		"kubectl", "--server="+server, "apply", "-f", "-")
	if err != nil {
		return fmt.Errorf("applying: %w: %s", err, out)
	}
	return nil
}

// Get returns one field of one object, by JSONPath.
func (c *Client) Get(ctx context.Context, namespace, resource, name, jsonPath string) (string, error) {
	args := []string{"get", resource, name, "-o", "jsonpath=" + jsonPath}
	if namespace != "" {
		args = append([]string{"-n", namespace}, args...)
	}
	out, err := c.Run(ctx, args...)
	return strings.TrimSpace(string(out)), err
}

// Nodes returns the cluster's node names.
func (c *Client) Nodes(ctx context.Context) ([]string, error) {
	out, err := c.Run(ctx, "get", "nodes", "-o",
		`jsonpath={range .items[*]}{.metadata.name}{"\n"}{end}`)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, line)
		}
	}
	return names, nil
}

// Ready reports whether a node is Ready.
func (c *Client) Ready(ctx context.Context, node string) (bool, error) {
	out, err := c.Get(ctx, "", "node", node,
		`{.status.conditions[?(@.type=="Ready")].status}`)
	if err != nil {
		return false, err
	}
	return out == "True", nil
}

// Helm runs helm on the bastion against a ready member.
//
// helm reads the kubeconfig's server, which names a node-local
// endpoint nobody serves on the bastion, so it is told which member
// to use for the same reason kubectl is. The kubeconfig's credentials
// and CA still apply, and the members' real addresses are in every
// server certificate's SANs.
func (c *Client) Helm(ctx context.Context, args ...string) ([]byte, error) {
	server, err := c.Server(ctx)
	if err != nil {
		return nil, err
	}
	return c.Bastion.Exec(ctx, append([]string{"helm", "--kube-apiserver=" + server}, args...)...)
}

// Install writes the kubeconfig onto the bastion, so that a plain
// kubectl there works for anyone looking at the lab by hand.
func (c *Client) Install(ctx context.Context, kubeconfig []byte) error {
	return c.Bastion.Put(ctx, bytes.NewReader(kubeconfig), "/root/.kube/config", 0o600)
}
