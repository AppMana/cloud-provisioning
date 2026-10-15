package rig

import (
	"bytes"
	"context"
	"debug/elf"
	"fmt"
	"strings"
)

// LinuxArchitecture observes the machine through its management transport.
func LinuxArchitecture(ctx context.Context, node Node) (string, error) {
	raw, err := node.Exec(ctx, "uname", "-sm")
	if err != nil {
		return "", fmt.Errorf("reading %s architecture: %w", node.Name(), err)
	}
	switch strings.TrimSpace(string(raw)) {
	case "Linux x86_64":
		return "amd64", nil
	case "Linux aarch64":
		return "arm64", nil
	default:
		return "", fmt.Errorf("unsupported node platform %q on %s", raw, node.Name())
	}
}

// ValidateLinuxBinary rejects stale cache entries and wrong-architecture tools.
func ValidateLinuxBinary(body []byte, arch string) error {
	var want elf.Machine
	switch arch {
	case "amd64":
		want = elf.EM_X86_64
	case "arm64":
		want = elf.EM_AARCH64
	default:
		return fmt.Errorf("unsupported Linux architecture %q", arch)
	}
	f, err := elf.NewFile(bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("reading tool ELF: %w", err)
	}
	defer f.Close()
	if f.Class != elf.ELFCLASS64 || f.Data != elf.ELFDATA2LSB || f.Machine != want {
		return fmt.Errorf("tool ELF does not match linux/%s", arch)
	}
	return nil
}
