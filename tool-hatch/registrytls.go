package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	keystore "github.com/pavlo-v-chernykh/keystore-go/v4"
	pkcs12 "software.sslmate.com/src/go-pkcs12"
)

func registryFile(path, label string) ([]byte, error) {
	f, err := os.Open(strings.TrimSpace(path))
	if err != nil {
		return nil, errors.New("Could not open " + label + ". Check the path and file permissions.")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New(label + " must be a regular file.")
	}
	data, err := io.ReadAll(io.LimitReader(f, 16<<20+1))
	if err != nil || len(data) > 16<<20 {
		return nil, errors.New("Could not read " + label + " (maximum 16 MiB).")
	}
	return data, nil
}

func registryJKS(data []byte, password string) (keystore.KeyStore, error) {
	store := keystore.New()
	err := store.Load(bytes.NewReader(data), []byte(password))
	return store, err
}

func registryKey(path, password string) (tls.Certificate, error) {
	data, err := registryFile(path, "Keystore Path")
	if err != nil {
		return tls.Certificate{}, err
	}
	var key any
	var chain [][]byte
	switch strings.ToLower(filepath.Ext(strings.TrimSpace(path))) {
	case ".pem":
		certificate, err := tls.X509KeyPair(data, data)
		if err != nil {
			return tls.Certificate{}, errors.New("Invalid PEM keystore: use a combined certificate chain and unencrypted private key, or PKCS#12/JKS for password-protected keys.")
		}
		return certificate, nil
	case ".p12", ".pfx":
		private, leaf, ca, decodeErr := pkcs12.DecodeChain(data, password)
		if decodeErr != nil {
			return tls.Certificate{}, errors.New("Cannot decode PKCS#12 keystore. Check the password and certificate/private-key contents.")
		}
		key = private
		chain = append(chain, leaf.Raw)
		for _, cert := range ca {
			chain = append(chain, cert.Raw)
		}
	case ".jks":
		store, err := registryJKS(data, password)
		if err != nil {
			return tls.Certificate{}, errors.New("Cannot decode JKS keystore. Check its format and password.")
		}
		count := 0
		for _, alias := range store.Aliases() {
			if !store.IsPrivateKeyEntry(alias) {
				continue
			}
			count++
			entry, err := store.GetPrivateKeyEntry(alias, []byte(password))
			if err != nil {
				return tls.Certificate{}, errors.New("Cannot unlock JKS private key. The key and store must use the same password.")
			}
			key, err = x509.ParsePKCS8PrivateKey(entry.PrivateKey)
			if err != nil {
				return tls.Certificate{}, errors.New("Unsupported JKS private key (PKCS#8 required).")
			}
			for _, cert := range entry.CertificateChain {
				chain = append(chain, cert.Content)
			}
		}
		if count != 1 {
			return tls.Certificate{}, errors.New("JKS keystore must contain exactly one private-key entry.")
		}
	default:
		return tls.Certificate{}, errors.New("Unsupported keystore format. Use .jks, .p12/.pfx, or combined .pem.")
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return tls.Certificate{}, errors.New("Invalid keystore private key.")
	}
	var certPEM []byte
	for _, cert := range chain {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert})...)
	}
	certificate, err := tls.X509KeyPair(certPEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}))
	if err != nil {
		return tls.Certificate{}, errors.New("Keystore certificate and private key are invalid or do not match.")
	}
	return certificate, nil
}

func registryTrust(path, password string) (*x509.CertPool, error) {
	data, err := registryFile(path, "Truststore Path")
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	var certs []*x509.Certificate
	switch strings.ToLower(filepath.Ext(strings.TrimSpace(path))) {
	case ".pem", ".crt":
		if !roots.AppendCertsFromPEM(data) {
			return nil, errors.New("Truststore contains no valid PEM certificates.")
		}
		return roots, nil
	case ".p12", ".pfx":
		certs, err = pkcs12.DecodeTrustStore(data, password)
		if err != nil {
			return nil, errors.New("Cannot decode PKCS#12 truststore. Check the password and Java trusted-certificate entries.")
		}
	case ".jks":
		store, decodeErr := registryJKS(data, password)
		if decodeErr != nil {
			return nil, errors.New("Cannot decode JKS truststore. Check its format and password.")
		}
		for _, alias := range store.Aliases() {
			if !store.IsTrustedCertificateEntry(alias) {
				continue
			}
			entry, err := store.GetTrustedCertificateEntry(alias)
			if err != nil {
				return nil, errors.New("Invalid JKS trusted certificate entry.")
			}
			cert, err := x509.ParseCertificate(entry.Certificate.Content)
			if err != nil {
				return nil, errors.New("Invalid X.509 truststore certificate.")
			}
			certs = append(certs, cert)
		}
	default:
		return nil, errors.New("Unsupported truststore format. Use .jks, .p12/.pfx, or PEM .pem/.crt.")
	}
	if len(certs) == 0 {
		return nil, errors.New("Truststore contains no trusted certificates.")
	}
	for _, cert := range certs {
		roots.AddCert(cert)
	}
	return roots, nil
}
