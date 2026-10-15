package qualification

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestVerifyFile(t *testing.T) {
	for _, tc := range []string{"valid", "tampered", "missing", "escape", "absolute", "symlink-escape", "directory-symlink-escape", "local-symlink", "directory", "fifo", "unknown-field", "trailing-json", "failed-measurement"} {
		t.Run(tc, func(t *testing.T) {
			base := t.TempDir()
			dir := filepath.Join(base, "evidence")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			data := []byte("measured evidence\n")
			sum := sha256.Sum256(data)
			write := func(path string, data []byte) {
				t.Helper()
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			link := func(target, path string) {
				t.Helper()
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			write(filepath.Join(dir, "result.json"), data)
			write(filepath.Join(base, "outside"), data)
			e := complete()
			for i := range e.Rows {
				e.Rows[i].SHA256 = hex.EncodeToString(sum[:])
			}
			switch tc {
			case "tampered":
				write(filepath.Join(dir, "result.json"), []byte("changed"))
			case "missing":
				e.Rows[0].Artifact = "missing"
			case "escape":
				e.Rows[0].Artifact = "../outside"
			case "absolute":
				e.Rows[0].Artifact = filepath.Join(base, "outside")
			case "symlink-escape":
				link("../outside", filepath.Join(dir, "link"))
				e.Rows[0].Artifact = "link"
			case "directory-symlink-escape":
				link(base, filepath.Join(dir, "link"))
				e.Rows[0].Artifact = "link/outside"
			case "local-symlink":
				link("result.json", filepath.Join(dir, "link"))
				e.Rows[0].Artifact = "link"
			case "directory":
				e.Rows[0].Artifact = "."
			case "fifo":
				if err := syscall.Mkfifo(filepath.Join(dir, "fifo"), 0600); err != nil {
					t.Fatal(err)
				}
				e.Rows[0].Artifact = "fifo"
			case "failed-measurement":
				e.Rows[0].ManagementFailures = 1
			}
			raw, err := json.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			switch tc {
			case "unknown-field":
				raw = append([]byte(`{"unrecognized":true,`), raw[1:]...)
			case "trailing-json":
				raw = append(raw, []byte("\n{}")...)
			}
			path := filepath.Join(dir, "qualification.json")
			write(path, raw)
			got, failures, err := VerifyFile(path)
			if tc == "unknown-field" || tc == "trailing-json" {
				if err == nil {
					t.Fatal("accepted invalid JSON")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Candidate != e.Candidate {
				t.Fatal("candidate changed")
			}
			wantPass := tc == "valid" || tc == "local-symlink"
			if (len(failures) == 0) != wantPass {
				t.Fatalf("failures: %v", failures)
			}
			if tc == "tampered" && !strings.Contains(strings.Join(failures, "\n"), "digest mismatch") {
				t.Fatal(failures)
			}
		})
	}
}

func TestVerifyRejectsEvidenceSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "nested")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(complete())
	if err := os.WriteFile(filepath.Join(dir, "outside.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(nested, "evidence.json")
	if err := os.Symlink("../outside.json", path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := VerifyFile(path); err == nil {
		t.Fatal("accepted evidence symlink outside root")
	}
}
