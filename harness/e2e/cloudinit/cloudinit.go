// Package cloudinit reads the subset of cloud-config the join
// patterns use.
//
// It exists for one rig only. A virtual machine boots a real
// cloud-init and this package never touches its userdata; a container
// has no boot and no cloud-init, so the harness stands in for the
// platform and applies the document itself. That standing-in is the
// container rig's largest fidelity gap, and every way this can differ
// from a real cloud-init is a way a row passes on something
// production would reject. So it is narrow on purpose and refuses
// what it cannot do rather than skipping it: a pattern that grows a
// key this does not implement fails loudly here instead of being
// half-applied to a node that then looks healthy.
package cloudinit

import (
	"fmt"
	"path"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"
)

// Doc is a parsed cloud-config document.
type Doc struct {
	WriteFiles []File
	RunCmd     []Cmd
	// Skipped names keys that were present, are real cloud-config,
	// and do not apply to a rig that reaches nodes with docker exec.
	// They are reported rather than ignored: silence is how the bash
	// harness left a difference between what a pattern says and what
	// a row proves, and nobody could see it.
	Skipped []string
}

// File is one write_files entry.
type File struct {
	Path    string
	Content string
	Mode    uint32
}

// Dir is the directory the file goes in. cloud-init creates it; a
// shell redirect does not, and the first file written to
// /etc/wg-dialer is always creating it.
func (f File) Dir() string { return path.Dir(f.Path) }

// Cmd is one runcmd entry.
//
// cloud-init accepts two forms and they are not equivalent: a list is
// argv and runs without a shell, a string is handed to one. A command
// carrying a pipe or a redirect only works in the second, so the
// distinction is kept rather than collapsed.
type Cmd struct {
	Argv  []string
	Shell bool
}

// String renders the command for a log line.
func (c Cmd) String() string { return strings.Join(c.Argv, " ") }

// raw mirrors the document's shape, including the keys this refuses,
// so that refusing is a decision made on what was there rather than
// an accident of what was parsed.
type raw struct {
	WriteFiles []rawFile `json:"write_files"`
	RunCmd     []any     `json:"runcmd"`

	// Everything below is real cloud-config this does not implement.
	// Named explicitly so the error can say which key stopped it.
	Packages    []any `json:"packages"`
	Users       []any `json:"users"`
	Bootcmd     []any `json:"bootcmd"`
	AptSources  []any `json:"apt_sources"`
	SSHKeys     any   `json:"ssh_authorized_keys"`
	Mounts      []any `json:"mounts"`
	DiskSetup   any   `json:"disk_setup"`
	PowerState  any   `json:"power_state"`
	YumRepos    any   `json:"yum_repos"`
	SnapPkgs    any   `json:"snap"`
	WriteConfig any   `json:"write_config"`
}

type rawFile struct {
	Path        string `json:"path"`
	Content     string `json:"content"`
	Permissions string `json:"permissions"`
	Encoding    string `json:"encoding"`
}

// DefaultMode is what cloud-init creates a file with when the entry
// states no permissions. Not zero: a file created mode 000 cannot be
// read by the thing that needs it, and the failure appears far from
// here.
const DefaultMode = 0o644

// Parse reads a cloud-config document.
func Parse(data []byte) (Doc, error) {
	// The header is not decoration. Without it a real cloud-init
	// treats the payload as a script or a MIME part and does
	// something else entirely, so accepting a document that lacks it
	// would be interpreting bytes no platform would interpret this
	// way.
	if !strings.HasPrefix(strings.TrimLeft(string(data), " \t\r\n"), "#cloud-config") {
		return Doc{}, fmt.Errorf("not a cloud-config document: no #cloud-config header")
	}

	var r raw
	if err := yaml.Unmarshal(data, &r); err != nil {
		return Doc{}, fmt.Errorf("parsing cloud-config: %w", err)
	}

	for _, unsupported := range []struct {
		key     string
		present bool
	}{
		{"packages", len(r.Packages) > 0},
		{"users", len(r.Users) > 0},
		{"bootcmd", len(r.Bootcmd) > 0},
		{"apt_sources", len(r.AptSources) > 0},
		{"mounts", len(r.Mounts) > 0},
		{"disk_setup", r.DiskSetup != nil},
		{"power_state", r.PowerState != nil},
		{"yum_repos", r.YumRepos != nil},
		{"snap", r.SnapPkgs != nil},
		{"write_config", r.WriteConfig != nil},
	} {
		if unsupported.present {
			return Doc{}, fmt.Errorf("cloud-config uses %q, which this rig cannot apply: "+
				"run the row on the VM rig, where a real cloud-init reads it", unsupported.key)
		}
	}

	var doc Doc

	// Who may log in changes nothing about what the node runs, and
	// this rig reaches nodes with docker exec rather than SSH, so a
	// key list is genuinely inapplicable here rather than unimplemented.
	// It is still reported, because it is a real difference between
	// what the pattern says and what a container row proves: on the VM
	// rig a real cloud-init installs these and the difference closes.
	if r.SSHKeys != nil {
		doc.Skipped = append(doc.Skipped, "ssh_authorized_keys")
	}

	for i, f := range r.WriteFiles {
		if f.Encoding != "" && f.Encoding != "text/plain" {
			return Doc{}, fmt.Errorf("write_files[%d] (%s) is %s-encoded, which this rig does not decode",
				i, f.Path, f.Encoding)
		}
		mode := uint32(DefaultMode)
		if f.Permissions != "" {
			// Octal, and quoted in YAML so it arrives as a string.
			// Read as decimal, "0600" becomes 0o1130, which is
			// world-writable: a tunnel's private key readable by
			// anything on the node.
			parsed, err := strconv.ParseUint(strings.TrimSpace(f.Permissions), 8, 32)
			if err != nil {
				return Doc{}, fmt.Errorf("write_files[%d] (%s) has permissions %q, which is not octal: %w",
					i, f.Path, f.Permissions, err)
			}
			mode = uint32(parsed)
		}
		doc.WriteFiles = append(doc.WriteFiles, File{Path: f.Path, Content: f.Content, Mode: mode})
	}

	for i, entry := range r.RunCmd {
		switch v := entry.(type) {
		case string:
			doc.RunCmd = append(doc.RunCmd, Cmd{Argv: []string{v}, Shell: true})
		case []any:
			var argv []string
			for _, word := range v {
				s, ok := word.(string)
				if !ok {
					return Doc{}, fmt.Errorf("runcmd[%d] has a non-string word %v", i, word)
				}
				argv = append(argv, s)
			}
			doc.RunCmd = append(doc.RunCmd, Cmd{Argv: argv})
		default:
			return Doc{}, fmt.Errorf("runcmd[%d] is neither a string nor a list", i)
		}
	}
	return doc, nil
}
