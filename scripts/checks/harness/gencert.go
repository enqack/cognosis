package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// genCert writes a throwaway TLS trust chain into dir for the tls check:
//
//	ca.pem          the trust anchor, handed to curl as --cacert
//	server.pem      the leaf, handed to the daemon as tls.cert_file
//	server-key.pem  the leaf's key, handed to the daemon as tls.key_file
//
// It is a CA plus a leaf signed by it rather than one self-signed cert because
// curl needs --cacert to name a real trust anchor; a bare self-signed leaf is
// not one. The leaf carries 127.0.0.1 and ::1 in its SAN since the check binds
// the daemon to 0.0.0.0 but connects over loopback.
//
// Certs live an hour: long enough for a check run, short enough that a stray
// copy left in a sandbox is worthless. Generated in Go, not openssl, because
// openssl is not in the flake's dev shell and Go always is.
func genCert(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	notBefore := time.Now().Add(-time.Minute) // tolerate a little clock skew
	notAfter := time.Now().Add(time.Hour)

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	caSerial, err := serial()
	if err != nil {
		return err
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          caSerial,
		Subject:               pkix.Name{CommonName: "cognosis check CA"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return err
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	leafSerial, err := serial()
	if err != nil {
		return err
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: leafSerial,
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return err
	}

	if err := writePEM(filepath.Join(dir, "ca.pem"), "CERTIFICATE", caDER); err != nil {
		return err
	}
	if err := writePEM(filepath.Join(dir, "server.pem"), "CERTIFICATE", leafDER); err != nil {
		return err
	}
	leafKeyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		return err
	}
	return writePEM(filepath.Join(dir, "server-key.pem"), "PRIVATE KEY", leafKeyDER)
}

// serial draws a certificate serial number from the same 128-bit space the
// CA/Browser Forum requires, which is far more than two throwaway certs need.
func serial() (*big.Int, error) {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("serial: %w", err)
	}
	return n, nil
}

func writePEM(path, blockType string, der []byte) error {
	buf := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	if buf == nil {
		return fmt.Errorf("pem encode %s", path)
	}
	return os.WriteFile(path, buf, 0o600)
}
