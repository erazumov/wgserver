package web

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/erazumov/wgserver/internal/config"
)

// writeSelfSignedCert writes a fresh self-signed cert+key pair for
// 127.0.0.1 to a temp dir and returns the paths.
func writeSelfSignedCert(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "wgserver-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	cf, err := os.Create(certFile)
	if err != nil {
		t.Fatalf("create cert file: %v", err)
	}
	if err := pem.Encode(cf, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatalf("encode cert: %v", err)
	}
	_ = cf.Close()

	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	kf, err := os.Create(keyFile)
	if err != nil {
		t.Fatalf("create key file: %v", err)
	}
	if err := pem.Encode(kf, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}); err != nil {
		t.Fatalf("encode key: %v", err)
	}
	_ = kf.Close()
	return certFile, keyFile
}

func TestListenAdmin_TCPPlain(t *testing.T) {
	al, err := ListenAdmin(httpConfig("127.0.0.1:0"))
	if err != nil {
		t.Fatalf("ListenAdmin: %v", err)
	}
	defer al.Listener.Close()
	if al.TLSServer != nil {
		t.Error("TLSServer set for plain HTTP")
	}
	if _, ok := al.Listener.(*net.TCPListener); !ok {
		t.Errorf("Listener type = %T, want *net.TCPListener", al.Listener)
	}
}

func TestListenAdmin_TCPTLS(t *testing.T) {
	cert, key := writeSelfSignedCert(t)
	al, err := ListenAdmin(httpConfig("127.0.0.1:0", withTLS(cert, key)))
	if err != nil {
		t.Fatalf("ListenAdmin: %v", err)
	}
	defer al.Listener.Close()
	if al.TLSServer == nil {
		t.Fatal("TLSServer nil; want non-nil for TLS config")
	}
	if len(al.TLSServer.Certificates) != 1 {
		t.Errorf("Certificates len = %d, want 1", len(al.TLSServer.Certificates))
	}
}

func TestListenAdmin_TLSPartialRejected(t *testing.T) {
	_, key := writeSelfSignedCert(t)
	_, err := ListenAdmin(httpConfig("127.0.0.1:0", withTLS("", key)))
	if err == nil {
		t.Fatal("want error on partial TLS config, got nil")
	}
}

func TestListenAdmin_UnixSocket(t *testing.T) {
	// macOS caps UNIX socket paths at 104 bytes; use a short path.
	dir, err := os.MkdirTemp("/tmp", "wgsock")
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "s")
	al, err := ListenAdmin(httpConfig("", withUnix(sock)))
	if err != nil {
		t.Fatalf("ListenAdmin: %v", err)
	}
	defer al.Listener.Close()
	defer os.Remove(sock)

	if al.TLSServer != nil {
		t.Error("TLSServer set for UNIX socket")
	}
	if _, ok := al.Listener.(*net.UnixListener); !ok {
		t.Errorf("Listener type = %T, want *net.UnixListener", al.Listener)
	}
	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if fi.Mode().Perm() != 0o660 {
		t.Errorf("socket mode = %v, want 0660", fi.Mode().Perm())
	}
}

func TestListenAdmin_UnixSocketReplacesStale(t *testing.T) {
	// macOS caps UNIX socket paths at 104 bytes; use a short path.
	dir, err := os.MkdirTemp("/tmp", "wgsock")
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "s")
	if err := os.WriteFile(sock, []byte("stale"), 0o600); err != nil {
		t.Fatalf("write stale: %v", err)
	}
	al, err := ListenAdmin(httpConfig("", withUnix(sock)))
	if err != nil {
		t.Fatalf("ListenAdmin: %v", err)
	}
	_ = al.Listener.Close()
}

func TestListenHealth(t *testing.T) {
	l, err := ListenHealth("127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenHealth: %v", err)
	}
	defer l.Close()
	if _, ok := l.(*net.TCPListener); !ok {
		t.Errorf("Listener type = %T, want *net.TCPListener", l)
	}
}

// Smoke: actually serve a tiny handler on the bound admin listener and
// confirm the response comes back. Catches "bind succeeded but accept
// broken" regressions.
func TestListenAdmin_TCPPlain_ServesARequest(t *testing.T) {
	al, err := ListenAdmin(httpConfig("127.0.0.1:0"))
	if err != nil {
		t.Fatalf("ListenAdmin: %v", err)
	}
	defer al.Listener.Close()

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})}
	go func() { _ = srv.Serve(al.Listener) }()
	defer srv.Close()

	resp, err := http.Get("http://" + al.Listener.Addr().String() + "/")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// ---- options helpers ----

type httpOpt func(*config.HTTPConfig)

func httpConfig(addr string, opts ...httpOpt) config.HTTPConfig {
	c := config.HTTPConfig{Addr: addr}
	for _, o := range opts {
		o(&c)
	}
	return c
}

func withUnix(p string) httpOpt {
	return func(c *config.HTTPConfig) { c.UnixSocket = p }
}
func withTLS(cert, key string) httpOpt {
	return func(c *config.HTTPConfig) {
		c.TLSCertFile = cert
		c.TLSKeyFile = key
	}
}
