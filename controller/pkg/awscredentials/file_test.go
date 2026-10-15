package awscredentials

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

func credentialJSON(t *testing.T, key string, expiry time.Time) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"AccessKeyId": key, "SecretAccessKey": "private-test-secret", "SessionToken": "private-test-token", "Expiration": expiry})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestProjectedSecretRotationThroughSDKCache(t *testing.T) {
	dir := t.TempDir()
	publish := func(version, key string) {
		t.Helper()
		target := filepath.Join(dir, version)
		if err := os.Mkdir(target, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(target, "credentials.json"), credentialJSON(t, key, time.Now().Add(30*time.Minute)), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(version, filepath.Join(dir, "..data-next")); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(dir, "..data-next"), filepath.Join(dir, "..data")); err != nil {
			t.Fatal(err)
		}
	}
	publish("first", "first-key")
	path := filepath.Join(dir, "credentials.json")
	if err := os.Symlink("..data/credentials.json", path); err != nil {
		t.Fatal(err)
	}
	// An expiry window longer than the fixture lifetime forces the next cache
	// retrieval to refresh without sleeping or manually invalidating the cache.
	cache := aws.NewCredentialsCache(FileProvider{Path: path}, func(o *aws.CredentialsCacheOptions) { o.ExpiryWindow = time.Hour })
	first, err := cache.Retrieve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.AccessKeyID != "first-key" || !first.CanExpire {
		t.Fatal("missing expiring first credentials")
	}
	publish("second", "second-key")
	second, err := cache.Retrieve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second.AccessKeyID != "second-key" {
		t.Fatal("SDK did not reload rotated Secret")
	}
	if err := os.Remove(filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Retrieve(context.Background()); err == nil {
		t.Fatal("used cached credentials after failed refresh")
	}
}

func TestInvalidCredentialFiles(t *testing.T) {
	for _, tc := range []string{"missing", "directory", "expired", "missing-token", "invalid-json", "trailing-json", "unknown-field", "oversized", "canceled"} {
		t.Run(tc, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "credentials.json")
			raw := credentialJSON(t, "key", time.Now().Add(time.Hour))
			switch tc {
			case "expired":
				raw = credentialJSON(t, "key", time.Now().Add(-time.Second))
			case "missing-token":
				raw = []byte(`{"AccessKeyId":"key","SecretAccessKey":"private-test-secret","Expiration":"2099-01-01T00:00:00Z"}`)
			case "invalid-json":
				raw = []byte("private-test-secret")
			case "trailing-json":
				raw = append(raw, []byte(" {}")...)
			case "unknown-field":
				raw = append([]byte(`{"unexpected":"private-test-secret",`), raw[1:]...)
			case "oversized":
				raw = []byte(strings.Repeat("private-test-secret", 10000))
			}
			if tc == "directory" {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			} else if tc != "missing" {
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc == "canceled" {
				cancel()
			}
			credentials, err := (FileProvider{Path: path}).Retrieve(ctx)
			if err == nil || credentials.AccessKeyID != "" {
				t.Fatal("invalid credentials accepted")
			}
			if strings.Contains(err.Error(), "private-test-secret") || strings.Contains(err.Error(), "private-test-token") {
				t.Fatal("credential material leaked in error")
			}
		})
	}
}
