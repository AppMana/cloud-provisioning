package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
)

func validateDiskGiB(size int) error {
	if size < 20 || size > 2048 {
		return fmt.Errorf("disk-gib must be between 20 and 2048")
	}
	return nil
}

type qemuRunner func(context.Context, ...string) ([]byte, error)

func growImage(ctx context.Context, path string, sizeGiB int, qemu qemuRunner) error {
	if err := validateDiskGiB(sizeGiB); err != nil {
		return err
	}
	readSize := func() (int64, error) {
		raw, err := qemu(ctx, "info", "--output=json", path)
		if err != nil {
			return 0, err
		}
		var info struct {
			Size    int64  `json:"virtual-size"`
			Format  string `json:"format"`
			Backing string `json:"backing-filename"`
		}
		if err := json.Unmarshal(raw, &info); err != nil {
			return 0, fmt.Errorf("invalid image metadata: %w", err)
		}
		if info.Format != "qcow2" || info.Backing != "" || info.Size <= 0 {
			return 0, fmt.Errorf("expected a nonempty standalone qcow2 export")
		}
		return info.Size, nil
	}
	current, err := readSize()
	if err != nil {
		return err
	}
	wanted := int64(sizeGiB) << 30
	if current >= wanted {
		return nil
	}
	if _, err := qemu(ctx, "resize", path, strconv.FormatInt(wanted, 10)); err != nil {
		return err
	}
	actual, err := readSize()
	if err != nil {
		return err
	}
	if actual != wanted {
		return fmt.Errorf("resized image has %d bytes, expected %d", actual, wanted)
	}
	return nil
}
