package flintlock

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	mvmv1 "github.com/liquidmetal-dev/flintlock/api/services/microvm/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"

	"github.com/ismoilovdevml/firerunner/internal/config"
	"github.com/ismoilovdevml/firerunner/internal/flintlock/flintlocktest"
)

// testCA signs test certificates.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// leaf returns a certificate and key in PEM, for a server (with the IP) or a client.
func (ca *testCA) leaf(t *testing.T, server bool) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "leaf"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	if server {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	kd, _ := x509.MarshalECPrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd})
}

func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// tlsFlintlockd serves the fake flintlock API over TLS on loopback, asking
// for a client certificate from clientCA like flintlockd with
// tls-client-validate. tokens reports the authorization metadata it got.
func tlsFlintlockd(t *testing.T, serverCA, clientCA *testCA) (endpoint string, tokens func() int) {
	t.Helper()
	certPEM, keyPEM := serverCA.leaf(t, true)
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(clientCA.pem)
	seen := make(chan struct{}, 100)
	gs := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{pair},
		ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert})),
		grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
			if md, ok := metadata.FromIncomingContext(ctx); ok && len(md.Get("authorization")) > 0 {
				seen <- struct{}{}
			}
			return h(ctx, req)
		}))
	mvmv1.RegisterMicroVMServer(gs, flintlocktest.NewServer(""))
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = gs.Serve(l) }()
	t.Cleanup(gs.Stop)
	return l.Addr().String(), func() int { return len(seen) }
}

// With flintlock.tls_ca_file the client checks flintlockd's certificate and
// presents its own; the token only goes to a verified flintlockd.
func TestDialOverMutualTLS(t *testing.T) {
	flintlockCA, clientCA, strangerCA := newTestCA(t), newTestCA(t), newTestCA(t)
	dir := t.TempDir()
	certPEM, keyPEM := clientCA.leaf(t, false)
	base := config.Flintlock{
		TokenFile:   writeFile(t, dir, "token", []byte(flintlocktest.Token)),
		Namespace:   "ns1",
		TLSCAFile:   writeFile(t, dir, "ca.pem", flintlockCA.pem),
		TLSCertFile: writeFile(t, dir, "client.pem", certPEM),
		TLSKeyFile:  writeFile(t, dir, "client.key", keyPEM),
	}
	list := func(cfg config.Flintlock) error {
		c, err := Dial(cfg)
		if err != nil {
			return err
		}
		defer c.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err = c.ListOnce(ctx)
		return err
	}

	t.Run("verified both ways", func(t *testing.T) {
		cfg := base
		cfg.Endpoint, _ = tlsFlintlockd(t, flintlockCA, clientCA)
		if err := list(cfg); err != nil {
			t.Fatalf("List over mTLS = %v", err)
		}
	})
	t.Run("a server with another CA gets nothing", func(t *testing.T) {
		cfg := base
		var tokens func() int
		cfg.Endpoint, tokens = tlsFlintlockd(t, strangerCA, clientCA)
		if err := list(cfg); err == nil {
			t.Fatal("the client accepted a flintlockd certificate from another CA")
		}
		if n := tokens(); n != 0 {
			t.Fatalf("the token reached an unverified server %d times", n)
		}
	})
	t.Run("no client certificate is refused", func(t *testing.T) {
		cfg := base
		cfg.TLSCertFile, cfg.TLSKeyFile = "", ""
		cfg.Endpoint, _ = tlsFlintlockd(t, flintlockCA, clientCA)
		if err := list(cfg); err == nil {
			t.Fatal("flintlockd asking for client certificates served a client without one")
		}
	})
	t.Run("a plaintext client never reaches a TLS flintlockd", func(t *testing.T) {
		cfg := base
		cfg.TLSCAFile, cfg.TLSCertFile, cfg.TLSKeyFile = "", "", ""
		cfg.Endpoint, _ = tlsFlintlockd(t, flintlockCA, clientCA)
		if err := list(cfg); err == nil {
			t.Fatal("plaintext call answered by a TLS server")
		}
	})
	t.Run("unreadable CA fails the dial", func(t *testing.T) {
		cfg := base
		cfg.Endpoint = "127.0.0.1:1"
		cfg.TLSCAFile = writeFile(t, dir, "empty.pem", []byte("no certificate here"))
		if _, err := Dial(cfg); err == nil {
			t.Fatal("dial with a CA file that holds no certificate")
		}
	})
}
