// Package scos carries the lab's serial-management configuration for SCOS.
// It does not install Kubernetes or modify the distribution's CNI.
package scos

import (
	"crypto/sha256"
	_ "embed"
	"fmt"
	"io"
	"os"
)

// ManagementIgnition enables the observed QGA RPCs and the distribution's
// SELinux transition for the lab command wrapper. SELinux remains enforcing.
// This is platform metadata, separate from product-owned worker bootstrap.
//
//go:embed management.ign
var ManagementIgnition []byte

const ExecWrapper = "/etc/qemu-ga/fsfreeze-hook.d/cldt-exec"

// The installer stream for this release supplies this initial SCOS disk;
// the release's machine OS image updates it during cluster installation.
const Release = "4.21.0-okd-scos.11"
const ReleaseImage = "quay.io/okd/scos-release@sha256:dd553282fb655f2461278af54c6eacda022b105664236ce4ad57091b5f6439c4"
const DiskSHA256 = "70609c79412a7853f0634f34afdd0e0a2492d4e188d45b78d4c9ead41ff571c8"

func ValidateDisk(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if fmt.Sprintf("%x", h.Sum(nil)) != DiskSHA256 {
		return fmt.Errorf("SCOS disk checksum differs from pinned installer stream")
	}
	return nil
}
