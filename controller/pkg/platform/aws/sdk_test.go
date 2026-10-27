package aws

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// stsEndpoint answers GetCallerIdentity, and records the access key that
// signed each request.
func stsEndpoint(t *testing.T) (*httptest.Server, *atomic.Value, *atomic.Int32) {
	t.Helper()
	signer, calls := &atomic.Value{}, &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		auth := r.Header.Get("Authorization")
		if i := strings.Index(auth, "Credential="); i >= 0 {
			signer.Store(strings.SplitN(auth[i+len("Credential="):], "/", 2)[0])
		}
		w.Header().Set("Content-Type", "text/xml")
		if err := r.ParseForm(); err == nil && r.Form.Get("Action") == "AssumeRole" {
			_, _ = w.Write([]byte(`<AssumeRoleResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><AssumeRoleResult>` +
				`<Credentials><AccessKeyId>ASIAROLE</AccessKeyId><SecretAccessKey>secret</SecretAccessKey><SessionToken>token</SessionToken>` +
				`<Expiration>2099-01-01T00:00:00Z</Expiration></Credentials>` +
				`<AssumedRoleUser><Arn>arn:aws:sts::123456789012:assumed-role/controller/s</Arn><AssumedRoleId>AROA:s</AssumedRoleId></AssumedRoleUser>` +
				`</AssumeRoleResult><ResponseMetadata><RequestId>2</RequestId></ResponseMetadata></AssumeRoleResponse>`))
			return
		}
		_, _ = w.Write([]byte(`<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult>` +
			`<Arn>arn:aws:iam::123456789012:user/controller</Arn><UserId>AIDA</UserId><Account>123456789012</Account>` +
			`</GetCallerIdentityResult><ResponseMetadata><RequestId>1</RequestId></ResponseMetadata></GetCallerIdentityResponse>`))
	}))
	t.Cleanup(server.Close)
	return server, signer, calls
}

func isolateAWSEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_PROFILE", "AWS_REGION"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	dir := t.TempDir()
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "absent-config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "absent-credentials"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
}

// The account comes from STS with whatever the standard chain finds: here
// environment keys, as aws.credentials mount: env provides them. It is asked
// once, and EC2 clients are kept per region.
func TestTheSDKUsesTheStandardChainFromTheEnvironment(t *testing.T) {
	isolateAWSEnvironment(t)
	server, signer, calls := stsEndpoint(t)
	t.Setenv("AWS_ENDPOINT_URL", server.URL)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAENVIRONMENT")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	sdk := &SDK{}
	for i := 0; i < 2; i++ {
		account, err := sdk.Account(context.Background(), "us-west-2")
		if err != nil || account != "123456789012" {
			t.Fatalf("account %q, %v", account, err)
		}
	}
	if calls.Load() != 1 || signer.Load() != "AKIAENVIRONMENT" {
		t.Fatalf("STS calls %d signed by %v", calls.Load(), signer.Load())
	}
	first, err := sdk.API(context.Background(), "us-west-2")
	if err != nil {
		t.Fatal(err)
	}
	again, _ := sdk.API(context.Background(), "us-west-2")
	other, _ := sdk.API(context.Background(), "eu-west-1")
	if first != again || first == other {
		t.Fatal("EC2 clients are not kept per region")
	}
}

// With mount: files the chain reads the shared credentials file named by
// AWS_SHARED_CREDENTIALS_FILE.
func TestTheSDKReadsTheSharedCredentialsFile(t *testing.T) {
	isolateAWSEnvironment(t)
	server, signer, _ := stsEndpoint(t)
	t.Setenv("AWS_ENDPOINT_URL", server.URL)
	path := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(path, []byte("[default]\naws_access_key_id = AKIAFILE\naws_secret_access_key = secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", path)
	if _, err := (&SDK{}).Account(context.Background(), "us-west-2"); err != nil {
		t.Fatal(err)
	}
	if signer.Load() != "AKIAFILE" {
		t.Fatalf("signed by %v", signer.Load())
	}
}

// A profile with role_arn and source_profile has the SDK assume the role
// with the source keys and sign with the role's credentials, which it
// refreshes itself: nothing outside the controller renews them.
func TestTheSDKAssumesTheProfilesRole(t *testing.T) {
	isolateAWSEnvironment(t)
	server, signer, _ := stsEndpoint(t)
	t.Setenv("AWS_ENDPOINT_URL", server.URL)
	t.Setenv("AWS_ENDPOINT_URL_STS", server.URL)
	dir := t.TempDir()
	config := filepath.Join(dir, "config")
	credentials := filepath.Join(dir, "credentials")
	if err := os.WriteFile(config, []byte("[default]\nrole_arn = arn:aws:iam::123456789012:role/controller\nsource_profile = base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credentials, []byte("[base]\naws_access_key_id = AKIABASE\naws_secret_access_key = secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", config)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", credentials)
	if _, err := (&SDK{}).Account(context.Background(), "us-west-2"); err != nil {
		t.Fatal(err)
	}
	if signer.Load() != "ASIAROLE" {
		t.Fatalf("signed by %v, want the assumed role", signer.Load())
	}
}

// An s3:// source becomes a URL signed with the controller's credentials,
// valid for the join token's lifetime.
func TestS3SourcesAreSignedForTheJoinTokensLifetime(t *testing.T) {
	isolateAWSEnvironment(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIASIGNER")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	sign := (&SDK{}).SignS3("us-west-2")
	signed, err := sign(context.Background(), "s3://assets/downloads/wg-dialer-linux-amd64")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"assets", "downloads/wg-dialer-linux-amd64", "X-Amz-Expires=7200", "AKIASIGNER", "X-Amz-Signature="} {
		if !strings.Contains(signed, want) {
			t.Errorf("signed URL %q lacks %q", signed, want)
		}
	}
	for _, bad := range []string{"https://assets/x", "s3://assets", "s3://assets/key?versionId=1"} {
		if _, err := sign(context.Background(), bad); err == nil {
			t.Errorf("%q was signed", bad)
		}
	}
}
