package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	keystore "github.com/pavlo-v-chernykh/keystore-go/v4"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	pkcs12 "software.sslmate.com/src/go-pkcs12"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRegistryReadOnlyFetchAndSchema(t *testing.T) {
	var paths []string
	var pathsMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("write request: %s", r.Method)
		}
		pathsMu.Lock()
		paths = append(paths, r.URL.EscapedPath())
		pathsMu.Unlock()
		if r.URL.EscapedPath() == "/registry/subjects/orders%2Fwest-value/versions" {
			w.Write([]byte(`[1,3,2,3]`))
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"schema": `{"z":9007199254740993,"a":["null","string"]}`})
	}))
	defer server.Close()
	service := &RegistryService{}
	config := RegistryConfig{URL: server.URL + "/registry/", Topic: "orders/west"}
	result := service.Fetch(config)
	if !result.Success || result.Version != 3 || len(result.Versions) != 3 || result.Versions[0] != 3 || result.Versions[2] != 1 {
		t.Fatalf("%+v", result)
	}
	if !strings.Contains(result.Schema, "9007199254740993") || !strings.Contains(result.Schema, "\n  \"z\"") {
		t.Fatal(result.Schema)
	}
	older := service.Schema(config, 1)
	if !older.Success || older.Version != 1 {
		t.Fatal(older)
	}
	pathsMu.Lock()
	defer pathsMu.Unlock()
	if len(paths) != 3 || !strings.HasSuffix(paths[1], "/3") || !strings.HasSuffix(paths[2], "/1") {
		t.Fatal(paths)
	}
}

func TestRegistryErrorsAndValidation(t *testing.T) {
	service := &RegistryService{}
	for _, tc := range []struct {
		code         int
		body, reason string
	}{
		{401, `password=secret`, "Authentication failed"}, {403, "", "Access denied"}, {404, "", "not found"}, {500, "private server detail", "HTTP 500"},
		{200, `[0]`, "invalid version"}, {200, `null`, "No versions"}, {200, `[]`, "No versions"}, {200, `not json`, "invalid JSON"},
	} {
		t.Run(tc.reason+tc.body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.code); w.Write([]byte(tc.body)) }))
			defer server.Close()
			result := service.Fetch(RegistryConfig{URL: server.URL, Topic: "orders"})
			if !strings.Contains(result.Message, tc.reason) || strings.Contains(result.Message, "secret") || strings.Contains(result.Message, "private server") {
				t.Fatal(result)
			}
		})
	}
	for _, config := range []RegistryConfig{
		{}, {URL: "file:///tmp/x", Topic: "a"}, {URL: "https://user:pass@example.com", Topic: "a"},
		{URL: "https://example.com?x=1", Topic: "a"}, {URL: "https://example.com"},
		{URL: "https://example.com", Topic: "a", Password: "secret"},
		{URL: "http://example.com", Topic: "a", Password: "secret", KeystorePath: "x", TruststorePath: "y"},
	} {
		if result := service.Fetch(config); result.Success || result.Message == "" || result.Schema != "" {
			t.Fatal(result)
		}
	}
	config := RegistryConfig{URL: "https://example.com", Topic: "a", Password: "secret"}
	result := service.Fetch(config)
	if !strings.Contains(result.Message, "Keystore Path, Truststore Path") {
		t.Fatal(result)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/subjects/a-value/versions":
			w.Write([]byte(`[1]`))
		default:
			w.Write([]byte(`{"schema":"not JSON","schemaType":"AVRO"}`))
		}
	}))
	defer server.Close()
	if result := service.Fetch(RegistryConfig{URL: server.URL, Topic: "a"}); result.Success || result.Schema != "" || len(result.Versions) != 1 {
		t.Fatal(result)
	}
	if service.Schema(RegistryConfig{}, 0).Success {
		t.Fatal("invalid version accepted")
	}
}

func TestRegistryRedirectAndNonAvro(t *testing.T) {
	hits := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++; w.Write([]byte(`[]`)) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer server.Close()
	result := (&RegistryService{}).Fetch(RegistryConfig{URL: server.URL, Topic: "a"})
	if result.Success || hits != 0 {
		t.Fatal(result, hits)
	}
	nonAvro := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"schema":"{}","schemaType":"PROTOBUF"}`))
	}))
	defer nonAvro.Close()
	if result := (&RegistryService{}).Schema(RegistryConfig{URL: nonAvro.URL, Topic: "a"}, 1); result.Success || !strings.Contains(result.Message, "not an Avro") {
		t.Fatal(result)
	}
}

func testRegistryCert(t *testing.T, template, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if parent == nil {
		parent = template
		parentKey = key
	}
	raw, err := x509.CreateCertificate(rand.Reader, template, parent, &key.PublicKey, parentKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(raw)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}
func TestRegistryTLSFormatsAndVerification(t *testing.T) {
	now := time.Now()
	ca, caKey := testRegistryCert(t, &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Test CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}, nil, nil)
	leaf, leafKey := testRegistryCert(t, &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, KeyUsage: x509.KeyUsageDigitalSignature}, ca, caKey)
	keyDER, _ := x509.MarshalPKCS8PrivateKey(leafKey)
	combined := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})...)
	cert, _ := tls.X509KeyPair(combined, combined)
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			t.Error("client certificate missing")
		}
		if strings.HasSuffix(r.URL.Path, "/versions") {
			w.Write([]byte(`[2,1]`))
		} else {
			w.Write([]byte(`{"schema":"{\"type\":\"string\"}"}`))
		}
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	password := "test-secret"
	p12, err := pkcs12.Modern.Encode(leafKey, leaf, []*x509.Certificate{ca}, password)
	if err != nil {
		t.Fatal(err)
	}
	trust12, err := pkcs12.Modern.EncodeTrustStore([]*x509.Certificate{ca}, password)
	if err != nil {
		t.Fatal(err)
	}
	keys := keystore.New()
	if err := keys.SetPrivateKeyEntry("client", keystore.PrivateKeyEntry{CreationTime: now, PrivateKey: keyDER, CertificateChain: []keystore.Certificate{{Type: "X509", Content: leaf.Raw}}}, []byte(password)); err != nil {
		t.Fatal(err)
	}
	trusts := keystore.New()
	if err := trusts.SetTrustedCertificateEntry("ca", keystore.TrustedCertificateEntry{CreationTime: now, Certificate: keystore.Certificate{Type: "X509", Content: ca.Raw}}); err != nil {
		t.Fatal(err)
	}
	var jks, trustJKS bytes.Buffer
	if err := keys.Store(&jks, []byte(password)); err != nil {
		t.Fatal(err)
	}
	if err := trusts.Store(&trustJKS, []byte(password)); err != nil {
		t.Fatal(err)
	}
	configs := []RegistryConfig{
		{KeystorePath: write("client.pem", combined), TruststorePath: write("ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw}))},
		{KeystorePath: write("client.p12", p12), TruststorePath: write("trust.p12", trust12)},
		{KeystorePath: write("keystore.jks", jks.Bytes()), TruststorePath: write("truststore.jks", trustJKS.Bytes())},
	}
	for _, config := range configs {
		config.URL = server.URL
		config.Topic = "orders"
		config.Password = password
		result := (&RegistryService{}).Fetch(config)
		if !result.Success || result.Version != 2 {
			t.Fatalf("%s: %+v", filepath.Ext(config.KeystorePath), result)
		}
		if filepath.Ext(config.KeystorePath) != ".pem" {
			wrong := config
			wrong.Password = "wrong"
			result = (&RegistryService{}).Fetch(wrong)
			if result.Success || !strings.Contains(result.Message, "password") || strings.Contains(result.Message, password) {
				t.Fatal(result)
			}
		}
	}
	// No configured store must still reject a server not signed by system roots.
	result := (&RegistryService{}).Fetch(RegistryConfig{URL: server.URL, Topic: "orders"})
	if result.Success || !strings.Contains(result.Message, "TLS certificate verification failed") {
		t.Fatal(result)
	}
	// A trusted CA does not bypass hostname verification.
	wrongName := configs[0]
	wrongName.URL = strings.Replace(server.URL, "127.0.0.1", "localhost", 1)
	wrongName.Topic = "orders"
	wrongName.Password = password
	if result := (&RegistryService{}).Fetch(wrongName); result.Success || !strings.Contains(result.Message, "TLS certificate verification failed") {
		t.Fatal(result)
	}
	// JKS store integrity and private-key protection must share the one UI password.
	mismatchedKeys := keystore.New()
	if err := mismatchedKeys.SetPrivateKeyEntry("client", keystore.PrivateKeyEntry{CreationTime: now, PrivateKey: keyDER, CertificateChain: []keystore.Certificate{{Type: "X509", Content: leaf.Raw}}}, []byte("different-key-password")); err != nil {
		t.Fatal(err)
	}
	var mismatch bytes.Buffer
	if err := mismatchedKeys.Store(&mismatch, []byte(password)); err != nil {
		t.Fatal(err)
	}
	if _, err := registryKey(write("different-key-password.jks", mismatch.Bytes()), password); err == nil || !strings.Contains(err.Error(), "same password") || strings.Contains(err.Error(), password) {
		t.Fatal("JKS shared private-key/store password rule not enforced safely")
	}
	if _, err := registryTrust(configs[2].TruststorePath, "wrong"); err == nil {
		t.Fatal("wrong truststore password accepted")
	}
	if _, err := registryKey(write("broken.p12", []byte("bad")), password); err == nil {
		t.Fatal("bad keystore accepted")
	}
	if _, err := registryTrust(write("empty.pem", []byte("none")), password); err == nil {
		t.Fatal("empty trust accepted")
	}
	if _, err := registryKey(filepath.Join(dir, "missing.p12"), password); err == nil {
		t.Fatal("missing file accepted")
	}
}
