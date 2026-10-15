// Package awscredentials provides rotating, expiring AWS credentials for
// installations where workload identity is not available.
package awscredentials

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// FileProvider reads a trusted, read-only projected Secret on each SDK refresh.
// Mount the directory, not a subPath: Kubernetes rotates the ..data symlink.
// The producer must publish replacement credentials before expiration. Missing
// or invalid replacements fail closed; this provider never falls back to ambient
// credentials and never includes file contents in errors.
type FileProvider struct{ Path string }

func (p FileProvider) Retrieve(ctx context.Context) (aws.Credentials, error) {
	if err := ctx.Err(); err != nil {
		return aws.Credentials{}, err
	}
	fail := func() (aws.Credentials, error) {
		return aws.Credentials{}, fmt.Errorf("rotating AWS credential file unavailable or invalid")
	}
	info, err := os.Stat(p.Path)
	if err != nil || !info.Mode().IsRegular() {
		return fail()
	}
	f, err := os.Open(p.Path)
	if err != nil {
		return fail()
	}
	defer f.Close()
	const limit = 64 << 10
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || len(raw) > limit {
		return fail()
	}
	var value struct {
		AccessKeyID     string    `json:"AccessKeyId"`
		SecretAccessKey string    `json:"SecretAccessKey"`
		SessionToken    string    `json:"SessionToken"`
		Expiration      time.Time `json:"Expiration"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return fail()
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fail()
	}
	if strings.TrimSpace(value.AccessKeyID) == "" || strings.TrimSpace(value.SecretAccessKey) == "" || strings.TrimSpace(value.SessionToken) == "" || !value.Expiration.After(time.Now()) {
		return fail()
	}
	if err := ctx.Err(); err != nil {
		return aws.Credentials{}, err
	}
	return aws.Credentials{AccessKeyID: value.AccessKeyID, SecretAccessKey: value.SecretAccessKey, SessionToken: value.SessionToken, Source: "RotatingCredentialFile", CanExpire: true, Expires: value.Expiration}, nil
}
