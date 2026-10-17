package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Set by the executable, not by library callers or isolated native tests.
// Containers must explicitly select the host's sysctl.d mount.
var receiveSysctlDirectory string

func receiveSysctlPolicy(name, alias string) (string, []byte, error) {
	if name == "" || len(name) > 15 || name == "." || name == ".." || name == "all" || name == "default" || name == "lo" || strings.Trim(name, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_.-") != "" {
		return "", nil, fmt.Errorf("invalid receiver sysctl interface")
	}
	valid := false
	for _, prefix := range []string{"cloud-provisioning:receive:v1:", "cloud-provisioning:receive-client:v1:", "cloud-provisioning:root:v1:"} {
		if strings.HasPrefix(alias, prefix) {
			hash := strings.TrimPrefix(alias, prefix)
			valid = len(hash) == 64 && strings.Trim(hash, "0123456789abcdef") == ""
		}
	}
	if !valid {
		return "", nil, fmt.Errorf("invalid receiver sysctl owner")
	}
	return "90-cloud-provisioning-rpf-" + name + ".conf", []byte("# " + alias + "\nnet/ipv4/conf/" + name + "/rp_filter = 0\n"), nil
}

// Publish before link creation: systemd's per-interface udev invocation must
// see the explicit setting, which excludes this interface from wildcard rules.
// Never persist a global/default override or replace administrator content.
func persistReceiveSysctlPolicy(name, alias string) error {
	if receiveSysctlDirectory == "" {
		return nil
	}
	filename, content, err := receiveSysctlPolicy(name, alias)
	if err != nil {
		return err
	}
	path := filepath.Join(receiveSysctlDirectory, filename)
	check := func() error {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("receiver sysctl policy is not a regular file")
		}
		current, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Equal(current, content) {
			return fmt.Errorf("receiver sysctl policy has different ownership or content")
		}
		return nil
	}
	if err := check(); !os.IsNotExist(err) {
		return err
	}
	tmp, err := os.CreateTemp(receiveSysctlDirectory, ".receive-sysctl-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err := tmp.Write(content); err != nil {
		return err
	}
	if err := tmp.Chmod(0644); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := os.Link(tmp.Name(), path); err != nil && !os.IsExist(err) {
		return err
	}
	if err := check(); err != nil {
		return err
	}
	dir, err := os.Open(receiveSysctlDirectory)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// The caller has established device absence under the reconciliation lock.
func removeReceiveSysctlPolicy(name, alias string) error {
	if receiveSysctlDirectory == "" {
		return nil
	}
	filename, expected, err := receiveSysctlPolicy(name, alias)
	if err != nil {
		return err
	}
	path := filepath.Join(receiveSysctlDirectory, filename)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("receiver sysctl policy is not a regular file")
	}
	current, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, expected) {
		return fmt.Errorf("refusing to remove changed receiver sysctl policy")
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	dir, err := os.Open(receiveSysctlDirectory)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
