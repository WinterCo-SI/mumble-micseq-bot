package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"math/big"
	"os"
	"strings"
	"sync"
	"time"
)

// loadOrCreateCert loads the bot's client certificate, generating a
// self-signed one on first run.
func loadOrCreateCert(certFile, keyFile, commonName string) (tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err == nil {
		return cert, nil
	}
	_, certErr := os.Stat(certFile)
	_, keyErr := os.Stat(keyFile)
	if !errors.Is(certErr, fs.ErrNotExist) || !errors.Is(keyErr, fs.ErrNotExist) {
		return tls.Certificate{}, fmt.Errorf("load client certificate: %w", err)
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(20, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(certFile, certPEM, 0o644); err != nil {
		return tls.Certificate{}, err
	}
	log.Printf("generated client certificate %s / %s", certFile, keyFile)
	return tls.X509KeyPair(certPEM, keyPEM)
}

func normalizeFingerprint(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.NewReplacer(":", "", " ", "").Replace(s)
}

func fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// serverPin verifies the server certificate by SHA-256 fingerprint. Mumble
// servers usually use self-signed certificates, so CA verification is not
// useful; the first fingerprint seen is trusted unless one is configured.
type serverPin struct {
	mu        sync.Mutex
	fixed     string
	trustFile string
}

func (p *serverPin) verify(rawCerts [][]byte, _ [][]*x509.Certificate) error {
	if len(rawCerts) == 0 {
		return errors.New("server sent no certificate")
	}
	got := fingerprint(rawCerts[0])
	p.mu.Lock()
	defer p.mu.Unlock()

	want := p.fixed
	source := "server_fingerprint"
	if want == "" && p.trustFile != "" {
		data, err := os.ReadFile(p.trustFile)
		switch {
		case err == nil:
			want = normalizeFingerprint(string(data))
			source = p.trustFile
		case errors.Is(err, fs.ErrNotExist):
			if err := os.WriteFile(p.trustFile, []byte(got+"\n"), 0o644); err != nil {
				return fmt.Errorf("save server fingerprint: %w", err)
			}
			log.Printf("trusting server certificate %s (saved to %s)", got, p.trustFile)
			return nil
		default:
			return err
		}
	}
	if want == "" || want == got {
		return nil
	}
	return fmt.Errorf("server certificate fingerprint %s does not match %s from %s; "+
		"if the server certificate was changed on purpose, update or delete %s", got, want, source, source)
}

func (c *Config) tlsConfig() (*tls.Config, error) {
	cert, err := loadOrCreateCert(c.CertFile, c.KeyFile, c.Username)
	if err != nil {
		return nil, err
	}
	pin := &serverPin{fixed: c.ServerFingerprint, trustFile: c.TrustFile}
	return &tls.Config{
		Certificates:          []tls.Certificate{cert},
		InsecureSkipVerify:    true,
		VerifyPeerCertificate: pin.verify,
	}, nil
}
