package aws

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
)

// S3Input stages bounded-lifetime input in an already configured private bucket.
// Its identity needs only Put/Get/DeleteObject on that bucket's transfers prefix.
type S3Input struct {
	CLI    *CLI
	Bucket string
}

func (s *S3Input) Stage(ctx context.Context, src io.Reader) (string, func(context.Context) error, error) {
	if s.Bucket == "" {
		return "", nil, fmt.Errorf("private input bucket is required")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", nil, err
	}
	key := "transfers/" + hex.EncodeToString(nonce[:])
	f, err := os.CreateTemp("", "cldt-aws-input-*")
	if err != nil {
		return "", nil, err
	}
	defer os.Remove(f.Name())
	if _, err := io.Copy(f, src); err != nil {
		f.Close()
		return "", nil, err
	}
	if err := f.Close(); err != nil {
		return "", nil, err
	}
	remove := func(ctx context.Context) error {
		_, err := s.CLI.Call(ctx, "s3api", "delete-object", map[string]any{"Bucket": s.Bucket, "Key": key})
		return err
	}
	if _, err := s.CLI.run(ctx, "s3api", "put-object", "--bucket", s.Bucket, "--key", key, "--body", f.Name(), "--server-side-encryption", "AES256"); err != nil {
		return "", nil, err
	}
	url, err := s.CLI.run(ctx, "s3", "presign", "s3://"+s.Bucket+"/"+key, "--expires-in", "900")
	if err != nil {
		_ = remove(ctx)
		return "", nil, err
	}
	return strings.TrimSpace(string(url)), remove, nil
}
