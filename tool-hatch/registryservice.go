package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// RegistryConfig is a complete request snapshot. Profiles persist the connection
// separately (OS vault for passwords); Topic remains query-specific.
type RegistryConfig struct {
	URL            string `json:"url"`
	Topic          string `json:"topic"`
	KeystorePath   string `json:"keystorePath"`
	TruststorePath string `json:"truststorePath"`
	Password       string `json:"password"`
}

type RegistryResponse struct {
	Success  bool   `json:"success"`
	Message  string `json:"message"`
	Versions []int  `json:"versions"`
	Version  int    `json:"version"`
	Schema   string `json:"schema"`
}

type RegistryService struct{}

func (s *RegistryService) Fetch(config RegistryConfig) RegistryResponse {
	client, endpoint, err := registryClient(config)
	if err != nil {
		return registryFailure(err)
	}
	defer client.CloseIdleConnections()
	var versions []int
	if err = registryGet(client, endpoint, &versions); err != nil {
		return registryFailure(err)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(versions)))
	unique := make([]int, 0, len(versions))
	for _, version := range versions {
		if version < 1 {
			return registryFailure(errors.New("Registry returned an invalid version list."))
		}
		if len(unique) == 0 || unique[len(unique)-1] != version {
			unique = append(unique, version)
		}
	}
	if len(unique) == 0 {
		return RegistryResponse{Success: true, Message: "No versions found for this subject.", Versions: unique}
	}
	response := registrySchema(client, endpoint, unique[0])
	response.Versions = unique
	return response
}

func (s *RegistryService) Schema(config RegistryConfig, version int) RegistryResponse {
	if version < 1 {
		return registryFailure(errors.New("Select a valid schema version."))
	}
	client, endpoint, err := registryClient(config)
	if err != nil {
		return registryFailure(err)
	}
	defer client.CloseIdleConnections()
	return registrySchema(client, endpoint, version)
}

func registryFailure(err error) RegistryResponse {
	return RegistryResponse{Message: err.Error(), Versions: []int{}}
}

func registryClient(config RegistryConfig) (*http.Client, string, error) {
	base, err := url.Parse(strings.TrimSpace(config.URL))
	if err != nil || base.Hostname() == "" || (base.Scheme != "http" && base.Scheme != "https") || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, "", errors.New("Schema Registry URL is required: use an http(s) URL without credentials, query or fragment.")
	}
	topic := strings.TrimSpace(config.Topic)
	if topic == "" {
		return nil, "", errors.New("Topic Name is required.")
	}
	missing := []string{}
	configured := config.KeystorePath != "" || config.TruststorePath != "" || config.Password != ""
	if configured {
		if strings.TrimSpace(config.KeystorePath) == "" {
			missing = append(missing, "Keystore Path")
		}
		if strings.TrimSpace(config.TruststorePath) == "" {
			missing = append(missing, "Truststore Path")
		}
		if config.Password == "" {
			missing = append(missing, "Password")
		}
		if len(missing) > 0 {
			return nil, "", fmt.Errorf("SSL configuration missing: %s.", strings.Join(missing, ", "))
		}
		if base.Scheme != "https" {
			return nil, "", errors.New("SSL configuration requires an https URL.")
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if configured {
		certificate, err := registryKey(config.KeystorePath, config.Password)
		if err != nil {
			return nil, "", err
		}
		roots, err := registryTrust(config.TruststorePath, config.Password)
		if err != nil {
			return nil, "", err
		}
		transport.TLSClientConfig.Certificates = []tls.Certificate{certificate}
		transport.TLSClientConfig.RootCAs = roots
	}
	client := &http.Client{Transport: transport, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirects are not followed") }}
	return client, strings.TrimRight(base.String(), "/") + "/subjects/" + url.PathEscape(topic+"-value") + "/versions", nil
}

func registryGet(client *http.Client, endpoint string, destination any) error {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return errors.New("Invalid Schema Registry request URL.")
	}
	req.Header.Set("Accept", "application/vnd.schemaregistry.v1+json, application/json")
	response, err := client.Do(req)
	if err != nil {
		var verification *tls.CertificateVerificationError
		var unknown x509.UnknownAuthorityError
		var hostname x509.HostnameError
		var invalid x509.CertificateInvalidError
		var timeout net.Error
		var dns *net.DNSError
		switch {
		case errors.As(err, &verification), errors.As(err, &unknown), errors.As(err, &hostname), errors.As(err, &invalid):
			return errors.New("TLS certificate verification failed. Check Truststore Path, certificate validity and server hostname.")
		case errors.As(err, &timeout) && timeout.Timeout():
			return errors.New("Schema Registry connection timed out.")
		case errors.As(err, &dns):
			return errors.New("Schema Registry hostname could not be resolved.")
		default:
			return errors.New("Could not connect to Schema Registry. Check the URL, network and TLS/client certificate configuration; redirects are not followed.")
		}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		switch response.StatusCode {
		case 401:
			return errors.New("Authentication failed (HTTP 401). Check client certificate credentials; HTTP Basic/Bearer authentication is not configured by this tool.")
		case 403:
			return errors.New("Access denied (HTTP 403). Check registry permissions and client certificate credentials.")
		case 404:
			return errors.New("Subject or version not found (HTTP 404). Check Topic Name and the -value subject.")
		default:
			return fmt.Errorf("Schema Registry returned HTTP %d.", response.StatusCode)
		}
	}
	const limit = 4 << 20
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return errors.New("Could not read the registry response.")
	}
	if len(data) > limit {
		return errors.New("Registry response exceeds the 4 MiB limit.")
	}
	if json.Unmarshal(data, destination) != nil {
		return errors.New("Registry returned an invalid JSON response.")
	}
	return nil
}

func registrySchema(client *http.Client, endpoint string, version int) RegistryResponse {
	var data struct {
		Schema     string `json:"schema"`
		SchemaType string `json:"schemaType"`
	}
	if err := registryGet(client, endpoint+"/"+strconv.Itoa(version), &data); err != nil {
		return registryFailure(err)
	}
	if data.SchemaType != "" && data.SchemaType != "AVRO" {
		return registryFailure(errors.New("Selected version is not an Avro schema."))
	}
	var formatted bytes.Buffer
	if json.Indent(&formatted, []byte(data.Schema), "", "  ") != nil {
		return registryFailure(errors.New("Registry returned an invalid Avro schema JSON string."))
	}
	return RegistryResponse{Success: true, Message: "Schema loaded.", Version: version, Schema: strings.TrimRight(formatted.String(), " \t\r\n"), Versions: []int{}}
}
