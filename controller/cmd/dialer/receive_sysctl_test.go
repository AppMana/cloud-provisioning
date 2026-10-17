package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
)

func TestReceiveSysctlPolicyOwnership(t *testing.T) {
	previous := receiveSysctlDirectory
	receiveSysctlDirectory = t.TempDir()
	t.Cleanup(func() { receiveSysctlDirectory = previous })
	alias := receiverAlias("mesh", "peer")
	name, expected, err := receiveSysctlPolicy("cldrtest", alias)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(receiveSysctlDirectory, name)
	for _, invalid := range []string{"../ens2", "ens*", "all", "default", "lo", "ens2\n"} {
		// all/default are valid kernel pseudo-interface names but must never
		// become persistent global policy through this scoped writer.
		if err := persistReceiveSysctlPolicy(invalid, alias); err == nil {
			t.Fatalf("accepted unsafe interface %q", invalid)
		}
	}
	for i := 0; i < 2; i++ {
		if err := persistReceiveSysctlPolicy("cldrtest", alias); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != string(expected) || strings.Contains(string(raw), "conf/all/") {
		t.Fatalf("policy %q: %v", raw, err)
	}
	if err := persistReceiveSysctlPolicy("cldrtest", receiverAlias("other", "peer")); err == nil {
		t.Fatal("changed owner accepted")
	}
	if err := removeReceiveSysctlPolicy("cldrtest", receiverAlias("other", "peer")); err == nil {
		t.Fatal("removed another owner's policy")
	}
	if err := os.WriteFile(path, []byte("administrator policy\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := persistReceiveSysctlPolicy("cldrtest", alias); err == nil {
		t.Fatal("overwrote administrator policy")
	}
	if err := removeReceiveSysctlPolicy("cldrtest", alias); err == nil {
		t.Fatal("removed administrator policy")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, expected, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := persistReceiveSysctlPolicy("cldrtest", alias); err == nil {
		t.Fatal("accepted symlink")
	}
	if err := removeReceiveSysctlPolicy("cldrtest", alias); err == nil {
		t.Fatal("removed symlink")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := persistReceiveSysctlPolicy("cldrtest", alias); err != nil {
		t.Fatal(err)
	}
	if err := removeReceiveSysctlPolicy("cldrtest", alias); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("retired policy remains: %v", err)
	}
}

func TestNativeReceiveSysctlPolicy(t *testing.T) {
	if os.Getenv("CLDT_NATIVE_RECEIVE_RPF") != "1" {
		t.Skip("requires isolated VM mount and network namespaces")
	}
	requireStableAddressNamespace(t)
	previous := receiveSysctlDirectory
	receiveSysctlDirectory = t.TempDir()
	t.Cleanup(func() { receiveSysctlDirectory = previous })
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	name, alias := "cldrsysctl", receiverAlias("mesh", "peer")
	must(persistReceiveSysctlPolicy(name, alias))
	link := &netlink.GenericLink{LinkAttrs: netlink.LinkAttrs{Name: name, Alias: alias}, LinkType: "wireguard"}
	must(netlink.LinkAdd(link))
	defer netlink.LinkDel(link)
	must(netlink.LinkSetAlias(link, alias))
	foreign := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "foreignrpf"}}
	must(netlink.LinkAdd(foreign))
	defer netlink.LinkDel(foreign)
	must(setSysctl("ipv4/conf/all/rp_filter", "0"))
	must(setSysctl("ipv4/conf/foreignrpf/rp_filter", "1"))
	wildcard := filepath.Join(t.TempDir(), "50-default.conf")
	must(os.WriteFile(wildcard, []byte("net.ipv4.conf.*.rp_filter = 2\n"), 0644))
	filename, _, err := receiveSysctlPolicy(name, alias)
	must(err)
	apply := func(files ...string) {
		t.Helper()
		args := append([]string{"--prefix=/net/ipv4/conf/" + name}, files...)
		out, err := exec.Command("/lib/systemd/systemd-sysctl", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("host sysctl loader: %v: %s", err, out)
		}
	}
	must(ensureReceiveReversePathFiltering(name, alias))
	apply(wildcard)
	if err := verifyReceiveReversePathFiltering(name); err == nil {
		t.Fatal("wildcard did not reproduce late udev overwrite")
	}
	// Use the actual host loader after device creation, like udev's add hook.
	apply(wildcard, filepath.Join(receiveSysctlDirectory, filename))
	must(verifyReceiveReversePathFiltering(name))
	value, err := readSysctl("ipv4/conf/foreignrpf/rp_filter")
	must(err)
	if value != "1" {
		t.Fatalf("unrelated interface filtering changed: %s", value)
	}
}
