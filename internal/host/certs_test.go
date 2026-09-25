package host

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
	"strings"
	"testing"
	"time"
)

func writeCert(t *testing.T, notAfter time.Time) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "t"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "c.crt")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCertsValidFor(t *testing.T) {
	day := 24 * time.Hour
	long := writeCert(t, time.Now().Add(3650*day))
	soon := writeCert(t, time.Now().Add(10*day))
	if err := CertsValidFor(90*day, long, ""); err != nil {
		t.Fatalf("a certificate valid for 10 years: %v", err)
	}
	if err := CertsValidFor(90*day, long, soon); err == nil || !strings.Contains(err.Error(), soon) {
		t.Fatalf("a certificate expiring in 10 days: err = %v, want it named", err)
	}
	if err := CertsValidFor(90*day, filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("a missing file passed")
	}
	junk := filepath.Join(t.TempDir(), "junk.pem")
	_ = os.WriteFile(junk, []byte("-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"), 0o600)
	if err := CertsValidFor(90*day, junk); err == nil || !strings.Contains(err.Error(), "no certificate") {
		t.Fatalf("a file without a certificate: err = %v", err)
	}
}
