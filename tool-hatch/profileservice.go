package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

var profileIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

// ProfileConnection deliberately excludes Topic, which is query-specific.
type ProfileConnection struct {
	URL            string `json:"url"`
	KeystorePath   string `json:"keystorePath"`
	TruststorePath string `json:"truststorePath"`
	Password       string `json:"password"`
}

type RegistryProfile struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	URL            string `json:"url"`
	KeystorePath   string `json:"keystorePath"`
	TruststorePath string `json:"truststorePath"`
	HasPassword    bool   `json:"hasPassword"`
}

// Complete connections live only in the private local profile file; never log them.
type profileRecord struct {
	RegistryProfile
	Password        string `json:"password,omitempty"`
	PasswordMissing bool   `json:"passwordMissing,omitempty"`
	// Read legacy references without accessing an OS credential manager.
	CredentialID string `json:"credentialID,omitempty"`
}
type profileFile struct {
	Version    int             `json:"version"`
	SelectedID string          `json:"selectedID"`
	Profiles   []profileRecord `json:"profiles"`
	Default    *profileRecord  `json:"default,omitempty"`
}
type ProfileResponse struct {
	Success             bool               `json:"success"`
	Warning             bool               `json:"warning"`
	Message             string             `json:"message"`
	Profiles            []RegistryProfile  `json:"profiles"`
	SelectedID          string             `json:"selectedID"`
	Config              *ProfileConnection `json:"config"`
	CredentialAvailable bool               `json:"credentialAvailable"`
}
type ProfileService struct {
	mu   sync.Mutex
	path string
}

func NewProfileService() *ProfileService {
	dir, err := os.UserConfigDir()
	path := ""
	if err == nil {
		path = filepath.Join(dir, "ToolHatch", "registry-profiles.json")
	}
	return &ProfileService{path: path}
}
func profileError(message string) ProfileResponse {
	return ProfileResponse{Message: message, Profiles: []RegistryProfile{}}
}
func profileState(data profileFile) ProfileResponse {
	profiles := make([]RegistryProfile, 0, len(data.Profiles))
	for _, p := range data.Profiles {
		profiles = append(profiles, p.RegistryProfile)
	}
	return ProfileResponse{Success: true, Profiles: profiles, SelectedID: data.SelectedID, CredentialAvailable: true}
}
func (s *ProfileService) read() (profileFile, error) {
	empty := profileFile{Version: 3, Profiles: []profileRecord{}, Default: emptyDefaultRecord()}
	if s.path == "" {
		return empty, errors.New("Profile configuration directory is unavailable. Current inputs remain usable, but profile persistence is unavailable.")
	}
	f, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return empty, nil
	}
	if err != nil {
		return empty, errors.New("Cannot read profile storage. Check file permissions; Current inputs remain usable, but profile persistence is unavailable.")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err != nil || len(raw) > 1<<20 {
		return empty, errors.New("Cannot read profile storage (maximum 1 MiB).")
	}
	var data profileFile
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&data) != nil || decoder.Decode(new(any)) != io.EOF || (data.Version != 1 && data.Version != 2 && data.Version != 3) || data.Profiles == nil || len(data.Profiles) > 100 {
		return empty, errors.New("Invalid profile storage. Existing data was left unchanged; repair the file before restoring or saving profiles.")
	}
	if data.Default == nil {
		if data.Version >= 2 {
			return empty, errors.New("Invalid Default Profile storage; existing data was left unchanged.")
		}
		data.Default = emptyDefaultRecord()
	}
	// Legacy passwords stay in the old OS store. Do not retrieve/delete them.
	// Preserve a visible re-entry requirement while migrating connection metadata.
	migrate := func(p *profileRecord) {
		if data.Version < 3 {
			p.PasswordMissing = p.HasPassword
			p.Password = ""
		}
		p.CredentialID = ""
	}
	if data.Version == 3 && data.Default.CredentialID != "" {
		return empty, errors.New("Invalid profile storage.")
	}
	migrate(data.Default)
	if data.Default.ID != "default" || data.Default.Name != "Default Profile" ||
		!validStoredPassword(*data.Default) || validateDefaultDraft(recordConnection(*data.Default)) != nil {
		return empty, errors.New("Invalid Default Profile storage; existing data was left unchanged.")
	}
	ids, names := map[string]bool{}, map[string]bool{}
	for i := range data.Profiles {
		p := &data.Profiles[i]
		if data.Version == 3 && p.CredentialID != "" {
			return empty, errors.New("Invalid profile storage.")
		}
		migrate(p)
		if !profileIDPattern.MatchString(p.ID) || ids[p.ID] || strings.TrimSpace(p.Name) == "" || names[strings.ToLower(p.Name)] ||
			!validStoredPassword(*p) ||
			validateProfileConnection(ProfileConnection{URL: p.URL, KeystorePath: p.KeystorePath, TruststorePath: p.TruststorePath, Password: placeholderPassword(p.HasPassword)}) != nil {
			return empty, errors.New("Invalid profile storage. Existing data was left unchanged; repair the file before restoring or saving profiles.")
		}
		ids[p.ID] = true
		names[strings.ToLower(p.Name)] = true
	}
	data.Version = 3
	if !ids[data.SelectedID] {
		data.SelectedID = ""
	}
	return data, nil
}
func placeholderPassword(has bool) string {
	if has {
		return "stored"
	}
	return ""
}
func validateProfileConnection(c ProfileConnection) error {
	base, err := url.Parse(strings.TrimSpace(c.URL))
	if err != nil || base.Hostname() == "" || (base.Scheme != "http" && base.Scheme != "https") || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return errors.New("A valid http(s) Schema Registry URL without embedded credentials, query or fragment is required.")
	}
	anySSL := c.KeystorePath != "" || c.TruststorePath != "" || c.Password != ""
	if anySSL && (strings.TrimSpace(c.KeystorePath) == "" || strings.TrimSpace(c.TruststorePath) == "" || c.Password == "") {
		return errors.New("SSL configuration requires Keystore Path, Truststore Path and Password, or all three empty.")
	}
	if anySSL && base.Scheme != "https" {
		return errors.New("SSL configuration requires an https URL.")
	}
	if len(c.Password) > 1024 {
		return errors.New("Profile passwords must be at most 1024 bytes.")
	}
	return nil
}
func randomProfileID() (string, error) {
	var data [16]byte
	_, err := rand.Read(data[:])
	return hex.EncodeToString(data[:]), err
}
func (s *ProfileService) write(data profileFile) error {
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil || len(raw) > 1<<20 {
		return errors.New("Profile data exceeds the storage limit.")
	}
	return writePrivateFile(s.path, raw, "profile")
}

// writePrivateFile atomically replaces path (tempfile, sync, rename) with a
// 0600 file inside a 0700 app directory. noun names the storage in messages.
func writePrivateFile(path string, raw []byte, noun string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return errors.New("Cannot create " + noun + " storage directory.")
	}
	if err := os.Chmod(filepath.Dir(path), 0700); err != nil {
		return errors.New("Cannot restrict " + noun + " directory permissions.")
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+noun+"-*")
	if err != nil {
		return errors.New("Cannot write " + noun + " storage.")
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(append(raw, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		return errors.New("Cannot save " + noun + " storage. Existing " + noun + " data was left unchanged.")
	}
	return nil
}
func (s *ProfileService) List() ProfileResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.read()
	if err != nil {
		return profileError(err.Error())
	}
	return profileState(data)
}
func (s *ProfileService) Apply(id string) ProfileResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.read()
	if err != nil {

		return profileError(err.Error())
	}
	config := ProfileConnection{}
	found := id == ""
	available := true
	if id == "" {
		config, available = s.connection(*data.Default)
	}
	for _, p := range data.Profiles {
		if p.ID == id {
			found = true
			config, available = s.connection(p)
		}
	}
	if !found {
		return profileError("Selected profile no longer exists. Choose another profile or Default Profile.")
	}
	data.SelectedID = id
	if err = s.write(data); err != nil {
		return profileError(err.Error())
	}
	result := profileState(data)
	result.Config = &config
	result.CredentialAvailable = available
	result.Message = "Profile applied."
	if id == "" {
		result.Message = "Default Profile restored."
	}
	if !available {
		result.Warning = true
		result.Message = "Legacy saved password is not stored locally. Re-enter Password and save this profile."
	}
	return result
}
func (s *ProfileService) Save(id, name string, config ProfileConnection) ProfileResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.read()
	if err != nil {
		return profileError(err.Error())
	}
	name = strings.TrimSpace(name)
	if id == "" && strings.EqualFold(name, "Default Profile") {
		return profileError("Default Profile is built in; choose another name.")
	}
	if name == "" || len(name) > 120 {
		return profileError("Profile Name is required (maximum 120 bytes).")
	}
	if err = validateProfileConnection(config); err != nil {
		return profileError(err.Error())
	}
	index := -1
	for i, p := range data.Profiles {
		if p.ID == id && id != "" {
			index = i
		}
		if strings.EqualFold(p.Name, name) && p.ID != id {
			return profileError("A profile with this name already exists. Choose another name.")
		}
	}
	if id != "" && index < 0 {
		return profileError("Profile no longer exists. Use Save As to create a new profile.")
	}
	if index < 0 && len(data.Profiles) >= 100 {
		return profileError("Maximum 100 profiles supported.")
	}
	if id == "" {
		id, err = randomProfileID()
		if err != nil {
			return profileError("Could not generate a profile ID.")
		}
	}
	record := profileRecord{RegistryProfile: RegistryProfile{ID: id, Name: name, URL: config.URL, KeystorePath: config.KeystorePath, TruststorePath: config.TruststorePath, HasPassword: config.Password != ""}, Password: config.Password}
	if index < 0 {
		data.Profiles = append(data.Profiles, record)
	} else {
		data.Profiles[index] = record
	}
	data.SelectedID = id
	if err = s.write(data); err != nil {
		return profileError(err.Error())
	}
	result := profileState(data)
	result.Message = "Profile saved locally."

	return result
}
func (s *ProfileService) Delete(id string) ProfileResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.read()
	if err != nil {
		return profileError(err.Error())
	}
	index := -1
	for i, p := range data.Profiles {
		if p.ID == id {
			index = i
			break
		}
	}
	if index < 0 {
		return profileError("Profile no longer exists. Refresh or choose Default Profile.")
	}
	wasSelected := data.SelectedID == id
	data.Profiles = append(data.Profiles[:index], data.Profiles[index+1:]...)
	if data.SelectedID == id {
		data.SelectedID = ""
	}
	if err = s.write(data); err != nil {
		return profileError(err.Error())
	}
	result := profileState(data)
	result.Message = "Profile deleted."
	if wasSelected {
		config, available := s.connection(*data.Default)
		result.Config = &config
		result.CredentialAvailable = available
		if !available {
			result.Warning = true
			result.Message += " Default Profile legacy password must be re-entered and saved locally."
		}
	}
	return result
}

func emptyDefaultRecord() *profileRecord {
	return &profileRecord{RegistryProfile: RegistryProfile{ID: "default", Name: "Default Profile"}}
}
func recordConnection(p profileRecord) ProfileConnection {
	return ProfileConnection{URL: p.URL, KeystorePath: p.KeystorePath, TruststorePath: p.TruststorePath, Password: p.Password}
}
func validStoredPassword(p profileRecord) bool {
	return p.HasPassword == (p.Password != "" || p.PasswordMissing) && !(p.PasswordMissing && p.Password != "") && len(p.Password) <= 1024
}
func (s *ProfileService) connection(p profileRecord) (ProfileConnection, bool) {
	return recordConnection(p), !p.PasswordMissing
}

// Default stores drafts, including empty/partial fields. Request validation is
// still enforced by RegistryService. URLs must not carry inline credentials.
func validateDefaultDraft(c ProfileConnection) error {
	if len(c.Password) > 1024 {
		return errors.New("Profile passwords must be at most 1024 bytes.")
	}
	if strings.ContainsAny(c.URL, "@?#") {
		return errors.New("Default URL cannot contain embedded credentials, query or fragment; use the Password field for SSL credentials.")
	}
	return nil
}

// Explicit edits replace/clear the shared password; nonsecret autosaves preserve it.
// This method never changes the selected named profile.
func (s *ProfileService) SaveDefault(config ProfileConnection, passwordChanged bool) ProfileResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.read()
	if err != nil {
		return profileError(err.Error())
	}
	if err = validateDefaultDraft(config); err != nil {
		return profileError(err.Error())
	}
	record := *data.Default
	record.URL, record.KeystorePath, record.TruststorePath = config.URL, config.KeystorePath, config.TruststorePath
	if passwordChanged {
		record.Password = config.Password
		record.HasPassword = config.Password != ""
		record.PasswordMissing = false
	}
	data.Default = &record
	if err = s.write(data); err != nil {
		return profileError(err.Error())
	}
	result := profileState(data)
	result.Message = "Default Profile saved automatically."
	if record.PasswordMissing {
		result.Warning = true
		result.CredentialAvailable = false
		result.Message = "Default settings saved; legacy password must be re-entered and saved locally."
	}
	return result
}
