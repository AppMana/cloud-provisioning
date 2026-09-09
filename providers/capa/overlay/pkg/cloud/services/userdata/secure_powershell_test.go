package userdata

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"regexp"
	"testing"
)

func TestWindowsSecretsManagerEnvelope(t *testing.T) {
	body, err := WindowsSecretsManagerUserData("aws.cluster.x-k8s.io/abc-123", 2, "us-west-2")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(body, []byte("<powershell>\n")) || len(body) > 16384 {
		t.Fatal("invalid EC2Launch envelope")
	}
	// Parse the actual envelope: SSM must be able to start while the join is
	// still running, and reboot must not replay consumed bootstrap credentials.
	var envelope struct {
		Persist string `xml:"persist"`
		Detach  string `xml:"detach"`
	}
	// EC2Launch extracts the PowerShell body literally; its '&' operators are
	// not XML entities. Only the trailing control tags are XML metadata.
	_, controls, ok := bytes.Cut(body, []byte("</powershell>"))
	if !ok {
		t.Fatal("missing PowerShell closing tag")
	}
	wrapped := append(append([]byte("<userdata>"), controls...), []byte("</userdata>")...)
	if err := xml.Unmarshal(wrapped, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Persist != "false" || envelope.Detach != "true" {
		t.Fatal("bootstrap must run once without delaying management access")
	}
	match := regexp.MustCompile(`FromBase64String\('([^']+)'\)`).FindSubmatch(body)
	if len(match) != 2 {
		t.Fatal("missing settings")
	}
	raw, err := base64.StdEncoding.DecodeString(string(match[1]))
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Prefix, Region string
		Chunks         int32
	}
	if err = json.Unmarshal(raw, &settings); err != nil {
		t.Fatal(err)
	}
	if settings.Prefix != "aws.cluster.x-k8s.io/abc-123" || settings.Region != "us-west-2" || settings.Chunks != 2 {
		t.Fatal("settings changed")
	}
	// These are native-consumer requirements observed with real binary chunks:
	// AWS Tools returns a MemoryStream; PowerShell 5 needs a UTF-8 BOM on disk.
	if !bytes.Contains(body, []byte("$response.SecretBinary.CopyTo($compressed)")) || !bytes.Contains(body, []byte("Text.UTF8Encoding($true)")) {
		t.Fatal("native binary/UTF-8 contract missing")
	}
}
func TestWindowsSecretsManagerRejectsInvalidReferences(t *testing.T) {
	for _, tc := range []struct {
		prefix string
		chunks int32
		region string
	}{
		{"aws.cluster.x-k8s.io/';exit 0;#", 1, "us-west-2"},
		{"unowned/abc", 1, "us-west-2"},
		{"aws.cluster.x-k8s.io/abc", 0, "us-west-2"},
		{"aws.cluster.x-k8s.io/abc", 1025, "us-west-2"},
		{"aws.cluster.x-k8s.io/abc", 1, "us-west-2\n</powershell>"},
	} {
		if _, err := WindowsSecretsManagerUserData(tc.prefix, tc.chunks, tc.region); err == nil {
			t.Fatal("invalid reference accepted")
		}
	}
}
