package main

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestGrowImageWithQEMU(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img is required for native disk validation")
	}
	ctx := context.Background()
	qemu := func(ctx context.Context, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, "qemu-img", args...).Output()
	}
	for _, size := range []string{"16G", "48G"} {
		t.Run(size, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "disk.qcow2")
			if _, err := qemu(ctx, "create", "-f", "qcow2", path, size); err != nil {
				t.Fatal(err)
			}
			if err := growImage(ctx, path, 32, qemu); err != nil {
				t.Fatal(err)
			}
			raw, err := qemu(ctx, "info", "--output=json", path)
			if err != nil {
				t.Fatal(err)
			}
			var info struct {
				Size int64 `json:"virtual-size"`
			}
			if err := json.Unmarshal(raw, &info); err != nil {
				t.Fatal(err)
			}
			wanted := int64(32) << 30
			if size == "48G" {
				wanted = int64(48) << 30
			}
			if info.Size != wanted {
				t.Fatalf("got %d bytes, expected %d", info.Size, wanted)
			}
			// An equal or smaller requested minimum must never shrink the export.
			noResize := func(ctx context.Context, args ...string) ([]byte, error) {
				if args[0] == "resize" {
					t.Fatal("resized an already sufficient disk")
				}
				return qemu(ctx, args...)
			}
			if err := growImage(ctx, path, 20, noResize); err != nil {
				t.Fatal(err)
			}
			if err := growImage(ctx, path, 32, noResize); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestGrowImageRejectsUnverifiedExports(t *testing.T) {
	for _, raw := range []string{`bad json`, `{}`, `{"virtual-size":100,"format":"raw"}`, `{"virtual-size":100,"format":"qcow2","backing-filename":"base.qcow2"}`} {
		t.Run(raw, func(t *testing.T) {
			qemu := func(_ context.Context, args ...string) ([]byte, error) {
				if args[0] != "info" {
					t.Fatal("modified an unverified image")
				}
				return []byte(raw), nil
			}
			if err := growImage(context.Background(), "export", 32, qemu); err == nil {
				t.Fatal("accepted invalid export")
			}
		})
	}
	for _, size := range []int{-1, 0, 19, 2049} {
		qemu := func(context.Context, ...string) ([]byte, error) {
			t.Fatal("executed qemu for invalid size")
			return nil, nil
		}
		if err := growImage(context.Background(), "export", size, qemu); err == nil {
			t.Fatal("accepted invalid size")
		}
	}
}

func TestGrowImagePropagatesFailuresAndVerifiesResize(t *testing.T) {
	for _, failure := range []int{1, 2, 3, 4} {
		calls := 0
		qemu := func(_ context.Context, args ...string) ([]byte, error) {
			calls++
			if calls == failure {
				return nil, errors.New("qemu failure")
			}
			// Failure 4 models a successful resize command that leaves the old size.
			return []byte(`{"virtual-size":17179869184,"format":"qcow2"}`), nil
		}
		if err := growImage(context.Background(), "export", 32, qemu); err == nil {
			t.Fatalf("accepted failure mode %d", failure)
		}
	}
}
