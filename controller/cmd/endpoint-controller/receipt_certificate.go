package main

import (
	"crypto/tls"
	"fmt"
	"os"
	"sync"
	"syscall"
)

// certificateFiles serves the key pair at two paths, reloading it when
// either file changes. The receipt serving certificate lives in a Secret
// that cert-manager renews; Kubernetes swaps the projected files, and the
// controller must serve the renewed certificate without a restart. A
// replacement that does not load leaves the last good pair in service.
type certificateFiles struct {
	certPath, keyPath string

	mu      sync.Mutex
	current *tls.Certificate
	stamp   string
}

func loadCertificateFiles(certPath, keyPath string) (*certificateFiles, error) {
	f := &certificateFiles{certPath: certPath, keyPath: keyPath}
	stamp, err := f.fileStamp()
	if err != nil {
		return nil, err
	}
	certificate, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	f.current, f.stamp = &certificate, stamp
	return f, nil
}

// fileStamp identifies the files' current contents by inode, size and
// modification time, following the projection's symlinks.
func (f *certificateFiles) fileStamp() (string, error) {
	var stamp string
	for _, path := range []string{f.certPath, f.keyPath} {
		info, err := os.Stat(path)
		if err != nil {
			return "", err
		}
		inode := uint64(0)
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			inode = st.Ino
		}
		stamp += fmt.Sprintf("%d/%d/%d;", inode, info.Size(), info.ModTime().UnixNano())
	}
	return stamp, nil
}

// get is tls.Config.GetCertificate.
func (f *certificateFiles) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	stamp, err := f.fileStamp()
	if err != nil || stamp == f.stamp {
		return f.current, nil
	}
	certificate, err := tls.LoadX509KeyPair(f.certPath, f.keyPath)
	if err != nil {
		return f.current, nil
	}
	f.current, f.stamp = &certificate, stamp
	return f.current, nil
}
