package qualification

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// VerifyFile checks qualification measurements and hashes artifacts beneath the
// evidence file's directory. Artifacts are streamed, including large captures.
// The directory must remain immutable during verification and subsequent use;
// hashing does not authenticate the producer or validate artifact semantics.
func VerifyFile(path string) (Evidence, []string, error) {
	var e Evidence
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return e, nil, err
	}
	defer root.Close()
	f, err := openRegular(root, filepath.Base(path))
	if err != nil {
		return e, nil, err
	}
	defer f.Close()
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	if err := d.Decode(&e); err != nil {
		return e, nil, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return e, nil, fmt.Errorf("unexpected trailing JSON")
	}
	failures := Check(e)
	for _, r := range e.Rows {
		if r.Artifact == "" {
			continue
		}
		if !filepath.IsLocal(r.Artifact) {
			failures = append(failures, r.ID+": artifact must be a local relative path")
			continue
		}
		sum, err := artifactDigest(root, r.Artifact)
		if err != nil {
			failures = append(failures, r.ID+": artifact unreadable: "+err.Error())
			continue
		}
		if sum != r.SHA256 {
			failures = append(failures, r.ID+": artifact digest mismatch")
		}
	}
	sort.Strings(failures)
	return e, failures, nil
}

func artifactDigest(root *os.Root, path string) (string, error) {
	f, err := openRegular(root, path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func openRegular(root *os.Root, path string) (*os.File, error) {
	// Check before opening to avoid ordinary device files, then check the opened
	// descriptor too. Nonblocking open prevents a FIFO swap from hanging the gate.
	info, err := root.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	f, err := openNonblocking(root, path)
	if err != nil {
		return nil, err
	}
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	return f, nil
}
