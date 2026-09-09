package aws

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// CAPA and the joined k0s Node reported this identity in the AWS matrix. Metadata is
// simulated here so missing IMDS cannot silently register a different identity.
func TestIdentityDiscoveryMatchesCAPAAndFailsClosed(t *testing.T) {
	for _, fail := range []bool{false, true} {
		dir := t.TempDir()
		curl := `#!/bin/sh
for arg do last="$arg"; done
case "$last" in
 */instance-id) printf '%s\n' i-0a74543a3eb1dae40 ;;
 */placement/availability-zone) printf '%s\n' us-west-2a ;;
 *) printf '%s\n' test-imds-token ;;
esac
`
		if fail {
			curl = "#!/bin/sh\nexit 22\n"
		}
		if err := os.WriteFile(filepath.Join(dir, "curl"), []byte(curl), 0700); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("sh", "-c", BootstrapIdentityScript)
		cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
		out, err := cmd.CombinedOutput()
		if (err != nil) != fail {
			t.Fatalf("failure=%v: %v", fail, err)
		}
		if !fail && strings.TrimSpace(string(out)) != "aws:///us-west-2a/i-0a74543a3eb1dae40" {
			t.Fatalf("identity mismatch: %s", out)
		}
	}
}

func TestPowerShellIdentityMatchesTheCAPAContract(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("PowerShell unavailable")
	}
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			stub := `function Invoke-RestMethod { param($Method,$Uri,$Headers,$TimeoutSec)
if ($TimeoutSec -ne 5) { throw 'unbounded metadata request' }
if ($Uri.EndsWith('/api/token')) { if ($Method -ne 'Put') { throw 'IMDSv2 required' }; return 'test-token' }
if ($Headers['X-aws-ec2-metadata-token'] -ne 'test-token') { throw 'token omitted' }
if ($Uri.EndsWith('/instance-id')) { return 'i-0a74543a3eb1dae40' }
if ($Uri.EndsWith('/availability-zone')) { return 'us-west-2a' }
if ($Uri.EndsWith('/local-ipv4')) { return '10.0.0.12' }
throw 'unexpected metadata path'
}
`
			if fail {
				stub = "function Invoke-RestMethod { throw 'IMDS unavailable' }\n"
			}
			path := filepath.Join(t.TempDir(), "identity.ps1")
			body := stub + "$result = & {\n" + BootstrapIdentityPowerShell + "\n}\n$result | ConvertTo-Json -Compress\n"
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			output, err := exec.Command(pwsh, "-NoProfile", "-File", path).CombinedOutput()
			if (err != nil) != fail {
				t.Fatalf("unexpected PowerShell result: %v %s", err, output)
			}
			if !fail {
				var identity map[string]string
				if err := json.Unmarshal(output, &identity); err != nil {
					t.Fatal(err)
				}
				if identity["providerID"] != "aws:///us-west-2a/i-0a74543a3eb1dae40" || identity["nodeAddress"] != "10.0.0.12" {
					t.Fatal("Windows identity differs from CAPI contract")
				}
			}
		})
	}
}
