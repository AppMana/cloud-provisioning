package main

import "testing"

// The agent refuses to start without the identity it publishes under, or
// with a lifetime too short to join within.
func TestTheTokenAgentCommandRequiresItsIdentity(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "")
	t.Setenv("NODE_NAME", "")
	for _, args := range [][]string{
		{"--namespace=cloud-provisioning"},
		{"--node-name=cp"},
		{"--namespace=cloud-provisioning", "--node-name=cp", "--token-ttl=5m"},
		{"--unknown"},
	} {
		if code := runMicrok8sTokenAgent(args); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}
