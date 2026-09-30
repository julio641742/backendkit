package localtls

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// Tests that call Config without a valid cached cert hit
// the real https://qip.sh. They are skipped with -short or when
// LOCALTLS_SKIP_NETWORK is set, so offline CI doesn't flake.

func needsNetwork(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("downloads from qip.sh; skipped with -short")
	}
	if os.Getenv("LOCALTLS_SKIP_NETWORK") != "" {
		t.Skip("downloads from qip.sh; skipped because LOCALTLS_SKIP_NETWORK is set")
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// genPEM returns a self-signed cert and its PKCS#8 key as one PEM blob.
func genPEM(t *testing.T, notBefore, notAfter time.Time) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return append(certPEM(t, key, notBefore, notAfter), keyPEM(t, key)...)
}

func certPEM(t *testing.T, key *ecdsa.PrivateKey, notBefore, notAfter time.Time) []byte {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test.local"},
		DNSNames:     []string{"test.local"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func keyPEM(t *testing.T, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// inTempDir runs the test from an empty directory, since pemPath is relative
// to the working directory.
func inTempDir(t *testing.T) {
	t.Helper()
	t.Chdir(t.TempDir())
}

func writeCached(t *testing.T, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(pemPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pemPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func leafOf(t *testing.T, cfg *tls.Config) *x509.Certificate {
	t.Helper()
	if cfg == nil || len(cfg.Certificates) != 1 {
		t.Fatalf("config = %+v, want exactly one certificate", cfg)
	}
	leaf := cfg.Certificates[0].Leaf
	if leaf == nil {
		t.Fatal("certificate Leaf is nil")
	}
	return leaf
}

// ---------------------------------------------------------------------------
// isCertValid
// ---------------------------------------------------------------------------

func TestIsCertValid(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name      string
		notBefore time.Time
		notAfter  time.Time
		want      bool
	}{
		{"current", now.Add(-time.Hour), now.Add(time.Hour), true},
		{"expired", now.Add(-2 * time.Hour), now.Add(-time.Hour), false},
		{"not yet valid", now.Add(time.Hour), now.Add(2 * time.Hour), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &x509.Certificate{NotBefore: tt.notBefore, NotAfter: tt.notAfter}
			if got := isCertValid(c); got != tt.want {
				t.Fatalf("isCertValid = %v, want %v", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

func TestLoadUsesValidCachedCert(t *testing.T) {
	inTempDir(t)
	now := time.Now()
	data := genPEM(t, now.Add(-time.Hour), now.Add(time.Hour))
	writeCached(t, data)

	cfg, err := Config()
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	if cn := leafOf(t, cfg).Subject.CommonName; cn != "test.local" {
		t.Fatalf("leaf CN = %q, want cached test.local (was it re-downloaded?)", cn)
	}
	got, _ := os.ReadFile(pemPath)
	if !bytes.Equal(got, data) {
		t.Fatal("cached file was modified")
	}
}

// wantDownloaded checks that cfg holds the real qip.sh cert and that it was
// written to pemPath.
func wantDownloaded(t *testing.T, cfg *tls.Config) {
	t.Helper()
	leaf := leafOf(t, cfg)
	if !slices.Contains(leaf.DNSNames, "*.i.qip.sh") {
		t.Fatalf("leaf DNSNames = %v, want *.i.qip.sh", leaf.DNSNames)
	}
	if !isCertValid(leaf) {
		t.Fatalf("downloaded cert not valid now: %s – %s", leaf.NotBefore, leaf.NotAfter)
	}

	info, err := os.Stat(pemPath)
	if err != nil {
		t.Fatalf("cached file not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("cached file perm = %o, want 600", perm)
	}
	data, _ := os.ReadFile(pemPath)
	cached, err := tls.X509KeyPair(data, data)
	if err != nil {
		t.Fatalf("cached file does not parse: %v", err)
	}
	if !cached.Leaf.Equal(leaf) {
		t.Fatal("cached file does not match returned cert")
	}
}

func TestLoadDownloadsWhenMissing(t *testing.T) {
	needsNetwork(t)
	inTempDir(t)

	cfg, err := Config()
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	wantDownloaded(t, cfg)
}

func TestLoadRedownloadsExpiredCert(t *testing.T) {
	needsNetwork(t)
	inTempDir(t)
	now := time.Now()
	writeCached(t, genPEM(t, now.Add(-2*time.Hour), now.Add(-time.Hour)))

	cfg, err := Config()
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	wantDownloaded(t, cfg)
}

func TestLoadRedownloadsCorruptCache(t *testing.T) {
	needsNetwork(t)
	inTempDir(t)
	writeCached(t, []byte("garbage"))

	cfg, err := Config()
	if err != nil {
		t.Fatalf("Config: %v", err)
	}
	wantDownloaded(t, cfg)
}
