// cache-unpack prepares existing image content using the same native platform
// matcher as containerd's image readiness checks. It never contacts a registry.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/platforms"
)

func pinnedTarget(reference string) (string, error) {
	if !regexp.MustCompile(`^[^\s@]+@sha256:[a-f0-9]{64}$`).MatchString(reference) {
		return "", fmt.Errorf("require digest-pinned image reference")
	}
	return strings.Split(reference, "@")[1], nil
}

func run(references []string) error {
	if runtime.GOOS != "windows" || runtime.GOARCH != "amd64" {
		return fmt.Errorf("requires native Windows amd64 builder")
	}
	if len(references) == 0 {
		return fmt.Errorf("at least one pinned reference required")
	}
	client, err := containerd.New(`\\.\pipe\cldt-image-cache`, containerd.WithDefaultPlatform(platforms.Default()))
	if err != nil {
		return err
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(namespaces.WithNamespace(context.Background(), "k8s.io"), 30*time.Minute)
	defer cancel()
	return unpackExisting(ctx, references, client.GetImage, os.Stdout)
}

// Keep image-store I/O injectable so native readiness failures can be replayed
// without a Windows daemon. The production client retains its native matcher.
func unpackExisting(ctx context.Context, references []string, get func(context.Context, string) (containerd.Image, error), out io.Writer) error {
	if len(references) == 0 {
		return fmt.Errorf("at least one pinned reference required")
	}
	images := make([]containerd.Image, 0, len(references))
	for _, ref := range references {
		target, err := pinnedTarget(ref)
		if err != nil {
			return err
		}
		img, err := get(ctx, ref)
		if err != nil {
			return err
		}
		if img.Target().Digest.String() != target {
			return fmt.Errorf("registered target differs from pinned input")
		}
		images = append(images, img)
	}
	for _, img := range images {
		if err := img.Unpack(ctx, "windows"); err != nil {
			return fmt.Errorf("native platform unpack: %w", err)
		}
		ready, err := img.IsUnpacked(ctx, "windows")
		if err != nil {
			return err
		}
		if !ready {
			return fmt.Errorf("native platform snapshot missing after unpack")
		}
		if err := json.NewEncoder(out).Encode(map[string]any{"image": img.Name(), "target": img.Target().Digest.String(), "platform": platforms.DefaultSpec(), "unpacked": ready}); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	flag.Parse()
	if err := run(flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
