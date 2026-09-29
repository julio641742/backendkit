// Package localtls provides a TLS config for LOCAL DEVELOPMENT ONLY.
//
// !!! WARNING: DO NOT USE IN PRODUCTION !!!
//
// The certificate and private key are downloaded from https://qip.sh, which
// publishes them openly so anyone can serve HTTPS on *.i.qip.sh (a wildcard
// DNS name that resolves to private IPs). The private key is PUBLIC: anyone
// can impersonate a server using it, and traffic protected by it offers no
// real confidentiality. It exists only so dev servers get a browser-trusted
// certificate without setting up a local CA.
//
//	cfg, err := localtls.Config()
//	srv := &http.Server{Addr: ":8443", Handler: app, TLSConfig: cfg}
//	err = srv.ListenAndServeTLS("", "") // https://app.i.qip.sh:8443
package localtls

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const (
	// pemPath is relative to the process working directory, not to this
	// package or the binary. The ./tls directory and the file are created on
	// first run if they do not exist.
	pemPath = "./tls/i.qip.sh.pem"
	pemURL  = "https://qip.sh/cert/i.qip.sh.pem"

	downloadTimeout = 30 * time.Second
)

// Config returns a *tls.Config using the PUBLIC qip.sh wildcard certificate
// for *.i.qip.sh. DEV ONLY — see the package docs: the private key is
// publicly known, so never use this in production.
//
// It reads the cached PEM from pemPath (relative to the working directory)
// and downloads a fresh copy if the file is missing, unparseable, or expired,
// creating ./tls as needed. The check happens only when this is called
// (normally at startup): a long-running process will keep serving the old
// cert after it expires and must be restarted to pick up a new one. Since
// this is for dev, a failed download is returned as an error and not retried.
func Config() (*tls.Config, error) {
	cached, err := os.ReadFile(pemPath)
	if err == nil {
		if cert, err := parsePEM(cached); err == nil {
			return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
		}
	}

	data, err := downloadPEM()
	if err != nil {
		return nil, fmt.Errorf("localtls: download PEM: %w", err)
	}
	cert, err := parsePEM(data)
	if err != nil {
		return nil, fmt.Errorf("localtls: downloaded PEM: %w", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
}

// parsePEM parses the combined PEM, holding the chain and the key, and
// checks the certificate is currently valid. X509KeyPair checks the key
// matches and parses the leaf into Leaf.
func parsePEM(data []byte) (tls.Certificate, error) {
	cert, err := tls.X509KeyPair(data, data)
	if err != nil {
		return tls.Certificate{}, err
	}
	if !isCertValid(cert.Leaf) {
		return tls.Certificate{}, fmt.Errorf("certificate is not valid now (notBefore=%s, notAfter=%s)",
			cert.Leaf.NotBefore, cert.Leaf.NotAfter)
	}
	return cert, nil
}

// downloadPEM fetches the combined PEM from pemURL and writes it to pemPath,
// creating the parent directory if needed. It gives up after downloadTimeout.
func downloadPEM() ([]byte, error) {
	client := &http.Client{Timeout: downloadTimeout}
	resp, err := client.Get(pemURL)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d from %s", resp.StatusCode, pemURL)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Dir(pemPath), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(pemPath, data, 0o600); err != nil {
		return nil, err
	}

	return data, nil
}

// isCertValid checks that the certificate is currently within its validity period.
func isCertValid(cert *x509.Certificate) bool {
	now := time.Now()
	return now.After(cert.NotBefore) && now.Before(cert.NotAfter)
}
