package join

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var templateActions = regexp.MustCompile(`{{.*?}}`)

// The dialer's systemd unit is written by a cloud-init template, as a
// shell-style continued command. A stray backslash in that continuation
// does not fail to render and does not fail to boot: systemd passes the
// backslash through as its own argument, Go's flag package stops
// parsing at the first argument that is not a flag, and every flag
// after it is silently discarded. The unit runs, the tunnel comes up,
// and the node quietly uses defaults for whatever the template thought
// it was setting.
//
// That shipped: both templates ended --listen-port with "\ \ \", so
// --keepalive-seconds, --mtu and --poll-interval never reached the
// dialer on any bootstrapped node.
func TestUnitExecStartPassesOnlyFlags(t *testing.T) {
	patterns, err := filepath.Glob(filepath.Join("..", "..", "..", "join-patterns", "*.cloud-config.tmpl"))
	if err != nil || len(patterns) == 0 {
		t.Fatalf("no join patterns found: %v", err)
	}
	for _, path := range patterns {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		for _, cmd := range execStartCommands(string(raw)) {
			// A template action renders to one word but contains
			// spaces, so it has to be collapsed before the command is
			// split the way systemd splits it.
			fields := strings.Fields(templateActions.ReplaceAllString(cmd, "VALUE"))
			for _, arg := range fields[1:] {
				if !strings.HasPrefix(arg, "--") {
					t.Errorf("%s: ExecStart argument %q is not a flag, so Go stops parsing here and every later flag is dropped:\n  %s",
						filepath.Base(path), arg, cmd)
				}
			}
		}
	}
}

// execStartCommands returns each ExecStart= value with its line
// continuations joined, as systemd would assemble them.
func execStartCommands(tmpl string) []string {
	var cmds []string
	lines := strings.Split(tmpl, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "ExecStart=") {
			continue
		}
		cmd := strings.TrimPrefix(line, "ExecStart=")
		for strings.HasSuffix(strings.TrimSpace(cmd), `\`) && i+1 < len(lines) {
			cmd = strings.TrimSuffix(strings.TrimSpace(cmd), `\`)
			i++
			cmd += " " + strings.TrimSpace(lines[i])
		}
		cmds = append(cmds, cmd)
	}
	return cmds
}

// The bootstrap unit is deliberately never disabled, so that a node
// whose DaemonSet cannot schedule stays reachable. That makes the
// machine-name file load-bearing: it is how the DaemonSet's shared pod
// spec finds this machine's own adoption Secret, which is the only
// source of a peer list that is not frozen at render time.
func TestPatternsWriteTheMachineNameForAdoption(t *testing.T) {
	patterns, _ := filepath.Glob(filepath.Join("..", "..", "..", "join-patterns", "*.cloud-config.tmpl"))
	for _, path := range patterns {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		if !strings.Contains(string(raw), "/etc/wg-dialer/machine-name") {
			t.Errorf("%s writes no machine-name file, so the DaemonSet cannot resolve this machine's adoption Secret", filepath.Base(path))
		}
	}
}
