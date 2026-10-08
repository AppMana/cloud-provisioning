package aws

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A failed call says which AWS error it was, without the rest of the
// message, which can echo the command's parameters.
func TestAFailedCallNamesOnlyItsErrorCode(t *testing.T) {
	for stderr, want := range map[string]string{
		"\nAn error occurred (ThrottlingException) when calling the SendCommand operation (reached max retries: 2): Rate exceeded\n": "ThrottlingException",
		"An error occurred (InvalidInstanceId) when calling the SendCommand operation: Instances [[i-0]] not in a valid state\n":     "InvalidInstanceId",
		"Parameter validation failed: commands=[secret]": "",
	} {
		if got := errorCode(stderr); got != want {
			t.Errorf("errorCode(%q) = %q, want %q", stderr, got, want)
		}
	}
}

// SSM throttles SendCommand per account. A row runs hundreds of probes
// through it, and on the dual-stack AWS row the CLI's own retries ran
// out: checks failed with ThrottlingException and the matrix gave up
// although the network had converged. A throttled call is retried until
// it goes through or the caller's context ends; anything else fails at
// once.
func TestAThrottledCallIsRetried(t *testing.T) {
	dir := t.TempDir()
	count := filepath.Join(dir, "count")
	script := "#!/bin/sh\nn=$(cat " + count + " 2>/dev/null || echo 0)\nn=$((n+1))\necho $n > " + count + "\n" +
		"case \"$*\" in *fail-hard*) echo 'An error occurred (InvalidInstanceId) when calling the SendCommand operation: x' >&2; exit 254;; esac\n" +
		"if [ $n -lt 3 ]; then echo 'An error occurred (ThrottlingException) when calling the SendCommand operation (reached max retries: 2): Rate exceeded' >&2; exit 254; fi\necho '{}'\n"
	if err := os.WriteFile(filepath.Join(dir, "aws"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	session := filepath.Join(dir, "session.json")
	if err := os.WriteFile(session, []byte(`{"Credentials":{"AccessKeyId":"a","SecretAccessKey":"b","SessionToken":"c"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &CLI{Region: "us-west-2", SessionPath: session, throttleBackoff: time.Millisecond}
	if _, err := c.Call(context.Background(), "ssm", "send-command", map[string]any{}); err != nil {
		t.Fatalf("a call throttled twice: %v", err)
	}
	if raw, _ := os.ReadFile(count); strings.TrimSpace(string(raw)) != "3" {
		t.Fatalf("aws ran %s times, want 3", raw)
	}
	_ = os.Remove(count)
	if _, err := c.Call(context.Background(), "ssm", "send-command", map[string]any{"fail-hard": true}); err == nil || !strings.Contains(err.Error(), "InvalidInstanceId") {
		t.Fatalf("a failing call: %v", err)
	}
	if raw, _ := os.ReadFile(count); strings.TrimSpace(string(raw)) != "1" {
		t.Fatalf("a non-throttling failure ran %s times, want once", raw)
	}
}
