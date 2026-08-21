package cloudinit

import (
	"strings"
	"testing"
)

// The container rig has no cloud-init, so it interprets the rendered
// userdata itself. That interpretation is the harness standing in for
// a platform, and every way it can differ from a real one is a way a
// row can pass on something production would reject. These pin the
// differences that have actually bitten.
func TestParseReadsWhatCloudInitWouldRun(t *testing.T) {
	doc := `#cloud-config
write_files:
  - path: /etc/wg-dialer/peers.json
    permissions: "0600"
    content: |
      {"peers":[]}
  - path: /usr/local/bin/hello
    permissions: "0755"
    content: |
      #!/bin/sh
      echo hi
runcmd:
  - [systemctl, daemon-reload]
  - systemctl enable --now wg-dialer
`
	got, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}

	if len(got.WriteFiles) != 2 {
		t.Fatalf("parsed %d files, want 2", len(got.WriteFiles))
	}
	first := got.WriteFiles[0]
	if first.Path != "/etc/wg-dialer/peers.json" {
		t.Errorf("path %q", first.Path)
	}
	// A block scalar keeps its trailing newline. A file written
	// without it is a shell script whose last line never runs.
	if first.Content != "{\"peers\":[]}\n" {
		t.Errorf("content %q: a block scalar keeps its trailing newline", first.Content)
	}
	// Permissions are octal and quoted in YAML. Reading "0600" as
	// decimal 600 yields 0o1130, which is world-writable: the private
	// key of a tunnel would be readable by anything on the node.
	if first.Mode != 0o600 {
		t.Errorf("mode %o, want 600: permissions are octal", first.Mode)
	}
	if got.WriteFiles[1].Mode != 0o755 {
		t.Errorf("mode %o, want 755", got.WriteFiles[1].Mode)
	}

	// Both forms of runcmd are real cloud-init: a list is argv and
	// runs without a shell, a string goes through one. Collapsing
	// them loses the distinction, and a command with a pipe or a
	// redirect in it silently stops working.
	if len(got.RunCmd) != 2 {
		t.Fatalf("parsed %d commands, want 2", len(got.RunCmd))
	}
	if got.RunCmd[0].Shell {
		t.Error("a list form command was marked as needing a shell")
	}
	if want := []string{"systemctl", "daemon-reload"}; !equal(got.RunCmd[0].Argv, want) {
		t.Errorf("argv %q, want %q", got.RunCmd[0].Argv, want)
	}
	if !got.RunCmd[1].Shell {
		t.Error("a string form command was not marked as needing a shell")
	}
}

// The templates constrain themselves to write_files and runcmd
// because that is all this interpreter implements. A pattern that
// grows a third key would be silently half-applied, and the row would
// pass on a node that never received it, so the key is a failure
// rather than a shrug.
func TestAKeyThisCannotRunIsAFailure(t *testing.T) {
	doc := `#cloud-config
packages:
  - wireguard
runcmd:
  - [true]
`
	_, err := Parse([]byte(doc))
	if err == nil {
		t.Fatal("a document using a key this cannot apply was accepted")
	}
	if !strings.Contains(err.Error(), "packages") {
		t.Errorf("the error does not name the key it cannot apply: %v", err)
	}
}

// Anything that is not a cloud-config document would be handed to a
// real cloud-init as a script or a MIME part and do something else
// entirely. Refuse rather than guess.
func TestSomethingThatIsNotCloudConfigIsRefused(t *testing.T) {
	if _, err := Parse([]byte("#!/bin/sh\necho hi\n")); err == nil {
		t.Error("a shell script was accepted as cloud-config")
	}
}

// A directory that does not exist yet is the ordinary case:
// /etc/wg-dialer holds the first file written to it. cloud-init
// creates the parent; a shell redirect does not, which is why this is
// reported per file rather than assumed.
func TestParentDirectoriesAreReported(t *testing.T) {
	got, err := Parse([]byte("#cloud-config\nwrite_files:\n  - path: /etc/wg-dialer/peers.json\n    content: x\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got.WriteFiles[0].Dir() != "/etc/wg-dialer" {
		t.Errorf("Dir() = %q", got.WriteFiles[0].Dir())
	}
	// An unstated mode is cloud-init's default, not zero: a file
	// created mode 000 cannot be read by the thing that needs it.
	if got.WriteFiles[0].Mode != 0o644 {
		t.Errorf("mode %o, want the 644 cloud-init defaults to", got.WriteFiles[0].Mode)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
