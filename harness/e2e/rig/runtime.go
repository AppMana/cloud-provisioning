package rig

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	labv1 "github.com/appmana/labcontainers/api/v1"
	labclient "github.com/appmana/labcontainers/pkg/client"
)

// Runtime owns topology deployment independently of the product-specific node
// configuration performed by this harness.
type Runtime interface {
	Deploy(context.Context, string, string, string, string) error
	Destroy(context.Context, string, string) (bool, error)
}

// LabcontainersRuntime persists a kept labd session so separate `lab up` and
// `lab down` processes operate on the same owned topology.
type LabcontainersRuntime struct{}

func (LabcontainersRuntime) Deploy(ctx context.Context, workDir, kind, name, topologyPath string) error {
	opts := persistentOptions(workDir, kind)
	labd, err := ensureLabd(ctx, workDir)
	if err != nil {
		return err
	}
	opts.LabdPath = labd
	_, err = labclient.DeployPersistent(ctx, opts, &labv1.LabSpec{
		Name:              name,
		Topology:          &labv1.TopologySource{Source: &labv1.TopologySource_Path{Path: topologyPath}},
		ArtifactDirectory: filepath.Join(workDir, "artifacts", kind),
	})
	return err
}

func (LabcontainersRuntime) Destroy(ctx context.Context, workDir, kind string) (bool, error) {
	opts := persistentOptions(workDir, kind)
	labd, err := ensureLabd(ctx, workDir)
	if err != nil {
		return false, err
	}
	opts.LabdPath = labd
	err = labclient.DestroyPersistent(ctx, opts)
	if errors.Is(err, labclient.ErrNoPersistentSession) {
		return false, nil
	}
	return err == nil, err
}

func ensureLabd(ctx context.Context, workDir string) (string, error) {
	if configured := os.Getenv("LABCONTAINERS_LABD"); configured != "" {
		return configured, nil
	}
	if installed, err := exec.LookPath("labd"); err == nil {
		return installed, nil
	}
	target := filepath.Join(workDir, ".labcontainers", "bin", "labd")
	if info, err := os.Stat(target); err == nil && info.Mode().IsRegular() {
		return target, nil
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return "", err
	}
	command := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", target, "github.com/appmana/labcontainers/cmd/labd")
	if output, err := command.CombinedOutput(); err != nil {
		return "", fmt.Errorf("building labd: %w: %s", err, output)
	}
	return target, nil
}

func persistentOptions(workDir, kind string) labclient.PersistentOptions {
	return labclient.PersistentOptions{
		StateDir: filepath.Join(workDir, ".labcontainers", "state"),
		RefPath:  filepath.Join(workDir, ".labcontainers", kind+"-session.json"),
		TTL:      7 * 24 * time.Hour,
	}
}
