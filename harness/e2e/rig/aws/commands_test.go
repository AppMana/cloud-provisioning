package aws

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestPowerShellProcessPreservesArguments(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("PowerShell not installed")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python not installed")
	}
	args := []string{"", "spaces and 'quotes'", `a\"b`, `C:\Program Files\`, "$(throw 'injected'); & echo bad", "Ω\nline"}
	argv := append([]string{python, "-c", "import sys,json;print(json.dumps(sys.argv[1:]))"}, args...)
	out, err := exec.Command(pwsh, "-NoLogo", "-NonInteractive", "-NoProfile", "-Command", (WindowsCommands{}).Exec(argv)).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	var got []string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if len(got) != len(args) {
		t.Fatalf("got %#v", got)
	}
	for i := range args {
		if got[i] != args[i] {
			t.Fatalf("arg %d: %q != %q", i, got[i], args[i])
		}
	}
}

func TestPowerShellProcessPreservesBinaryStdinAndFailure(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("PowerShell not installed")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python not installed")
	}
	data := make([]byte, 256)
	for i := range data {
		data[i] = byte(i)
	}
	path := filepath.Join(t.TempDir(), "input ' binary")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	script := windowsProcess([]string{python, "-c", "import sys,base64;sys.stdout.write(base64.b64encode(sys.stdin.buffer.read()).decode());sys.exit(7)"}, psQuote(path))
	out, err := exec.Command(pwsh, "-NoLogo", "-NonInteractive", "-NoProfile", "-Command", script).Output()
	if err == nil {
		t.Fatal("child failure lost")
	}
	if e, ok := err.(*exec.ExitError); !ok || e.ExitCode() != 7 {
		t.Fatalf("exit status: %v", err)
	}
	if string(out) != base64.StdEncoding.EncodeToString(data) {
		t.Fatalf("binary stdin changed: %s", out)
	}
}
