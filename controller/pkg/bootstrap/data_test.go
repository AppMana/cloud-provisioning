package bootstrap

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// EC2Launch v2 on both Windows versions extracts the literal tag body. It
// does not XML-unescape it; a general XML parser hid that observed failure.
func TestEC2LaunchPreservesPowerShell(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("PowerShell not installed")
	}
	want := `quote & <tag> " Ω </powershell>`
	script := "$value = '" + want + "'\r\n[Console]::Write($value)\r\n"
	raw, err := (Data{Format: PowerShell, Value: []byte(script)}).EC2Launch()
	if err != nil {
		t.Fatal(err)
	}
	body := regexp.MustCompile(`(?s)<powershell>(.*?)</powershell>`).FindSubmatch(raw)
	if len(body) != 2 || !strings.Contains(string(raw), "<persist>false</persist>") {
		t.Fatalf("bad envelope: %s", raw)
	}
	out, err := exec.Command(pwsh, "-NoLogo", "-NonInteractive", "-NoProfile", "-Command", string(body[1])).CombinedOutput()
	if err != nil || string(out) != want {
		t.Fatalf("PowerShell roundtrip: %q, %v", out, err)
	}
}

func TestEC2LaunchPreservesFailure(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("PowerShell not installed")
	}
	raw, err := (Data{Format: PowerShell, Value: []byte("exit 7")}).EC2Launch()
	if err != nil {
		t.Fatal(err)
	}
	body := regexp.MustCompile(`(?s)<powershell>(.*?)</powershell>`).FindSubmatch(raw)
	err = exec.Command(pwsh, "-NoLogo", "-NonInteractive", "-NoProfile", "-Command", string(body[1])).Run()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 7 {
		t.Fatalf("lost bootstrap exit status: %v", err)
	}
}

func TestEC2LaunchRejectsNonWindowsAndEmptyPayloads(t *testing.T) {
	for _, d := range []Data{{Format: PowerShell}, {Format: CloudConfig, Value: []byte("#cloud-config")}, {Format: "unknown", Value: []byte("x")}} {
		if _, err := d.EC2Launch(); err == nil {
			t.Fatalf("accepted %#v", d)
		}
	}
}
