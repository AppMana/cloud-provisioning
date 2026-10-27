package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeKeyPair(t *testing.T, dir, name string, serial int64) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "receipts"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"receipts"}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	// Kubernetes projects a Secret through a symlinked directory it swaps;
	// writing beside and renaming over has the same effect for a reader.
	for file, block := range map[string]*pem.Block{
		"tls.crt": {Type: "CERTIFICATE", Bytes: der},
		"tls.key": {Type: "EC PRIVATE KEY", Bytes: keyDER},
	} {
		tmp := filepath.Join(dir, name+"-"+file)
		if err := os.WriteFile(tmp, pem.EncodeToMemory(block), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, filepath.Join(dir, file)); err != nil {
			t.Fatal(err)
		}
	}
}

func servedSerial(t *testing.T, files *certificateFiles) int64 {
	t.Helper()
	certificate, err := files.get(nil)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf.SerialNumber.Int64()
}

// cert-manager renews the receipt serving certificate in its Secret; the
// controller serves the renewed one without a restart, and keeps serving
// the last good one through a half-written or broken replacement.
func TestTheReceiptServerServesARenewedCertificate(t *testing.T) {
	dir := t.TempDir()
	writeKeyPair(t, dir, "first", 1)
	files, err := loadCertificateFiles(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"))
	if err != nil {
		t.Fatal(err)
	}
	if servedSerial(t, files) != 1 {
		t.Fatal("not the first certificate")
	}
	writeKeyPair(t, dir, "renewed", 2)
	if servedSerial(t, files) != 2 {
		t.Fatal("the renewed certificate was not served")
	}
	if err := os.WriteFile(filepath.Join(dir, "tls.crt"), []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if servedSerial(t, files) != 2 {
		t.Fatal("a broken replacement displaced the last good certificate")
	}
}
