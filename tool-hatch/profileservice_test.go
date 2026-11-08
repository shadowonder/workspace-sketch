package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func profileFixture(t *testing.T) *ProfileService {
	t.Helper()
	return &ProfileService{path: filepath.Join(t.TempDir(), "ToolHatch", "registry-profiles.json")}
}
func TestProfilesCompleteLocalRestartAndCRUD(t *testing.T) {
	s := profileFixture(t)
	if err := os.MkdirAll(filepath.Dir(s.path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(s.path), 0755); err != nil {
		t.Fatal(err)
	}
	c := ProfileConnection{URL: "https://registry.example.com/base", KeystorePath: "/keystore.jks", TruststorePath: "/truststore.jks", Password: " exact unicode fixture 世界 \n"}
	saved := s.Save("", "Development", c)
	if !saved.Success || saved.SelectedID == "" {
		t.Fatal("save failed")
	}
	id := saved.SelectedID
	raw, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	var disk profileFile
	if json.Unmarshal(raw, &disk) != nil || disk.Version != 3 || disk.Profiles[0].Password != c.Password || strings.Contains(string(raw), `"topic"`) || strings.Contains(string(raw), `credentialID`) {
		t.Fatal("complete profile not stored locally")
	}
	for path, mode := range map[string]os.FileMode{s.path: 0600, filepath.Dir(s.path): 0700} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != mode {
			t.Fatal("private file permissions not enforced")
		}
	}
	restarted := &ProfileService{path: s.path}
	if got := restarted.List(); !got.Success || got.SelectedID != id {
		t.Fatal("selection not restored")
	}
	if got := restarted.Apply(id); !got.Success || got.Config == nil || *got.Config != c || !got.CredentialAvailable {
		t.Fatal("complete connection not restored")
	}
	clean := ProfileConnection{URL: "https://another.example.com"}
	if !s.Save(id, "Development", clean).Success {
		t.Fatal("update failed")
	}
	if got := s.Apply(id); *got.Config != clean {
		t.Fatal("connection merged instead of replaced")
	}
	if got := s.Apply(""); !got.Success || *got.Config != (ProfileConnection{}) {
		t.Fatal("Default not independent")
	}
	if got := s.Delete(id); !got.Success || len(got.Profiles) != 0 {
		t.Fatal("delete failed")
	}
	if s.Apply(id).Success || s.Delete(id).Success || s.Save(id, "Missing", c).Success {
		t.Fatal("missing profile accepted")
	}
}
func TestProfilesValidationCorruptionAndWriteFailure(t *testing.T) {
	s := profileFixture(t)
	for _, c := range []ProfileConnection{{}, {URL: "https://u:p@example.com"}, {URL: "https://example.com", KeystorePath: "x"}, {URL: "http://example.com", KeystorePath: "x", TruststorePath: "y", Password: "fixture"}} {
		if s.Save("", "Invalid", c).Success {
			t.Fatal("invalid connection accepted")
		}
	}
	clean := ProfileConnection{URL: "https://example.com"}
	if s.Save("", " ", clean).Success || s.Save("", "Default Profile", clean).Success {
		t.Fatal("invalid name accepted")
	}
	if !s.Save("", "One", clean).Success || s.Save("", "one", clean).Success {
		t.Fatal("duplicate name validation")
	}
	for _, bad := range []string{`{`, `{"version":99,"profiles":[]}`, `{"version":1,"profiles":[],"password":"bad"}`, `{"version":1,"profiles":null}`, `{"version":3,"profiles":[]}`} {
		if err := os.WriteFile(s.path, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if s.List().Success || s.Save("", "New", clean).Success || s.Apply("").Success {
			t.Fatal("corrupt storage accepted")
		}
		raw, _ := os.ReadFile(s.path)
		if string(raw) != bad {
			t.Fatal("corrupt file overwritten")
		}
	}
	blocked := profileFixture(t)
	if err := os.WriteFile(filepath.Dir(blocked.path), []byte("block directory creation"), 0600); err != nil {
		t.Fatal(err)
	}
	result := blocked.SaveDefault(ProfileConnection{Password: "fixture-secret"}, true)
	if result.Success || strings.Contains(result.Message, "fixture-secret") {
		t.Fatal("write failure unsafe")
	}
}
func TestDefaultLocalDraftPersistenceAndNamedIsolation(t *testing.T) {
	s := profileFixture(t)
	named := s.Save("", "Named", ProfileConnection{URL: "https://named.example.com"})
	draft := ProfileConnection{URL: "htt", KeystorePath: "/partial/keystore.jks", Password: "default-fixture"}
	if !s.SaveDefault(draft, true).Success {
		t.Fatal("draft rejected")
	}
	if got := s.List(); got.SelectedID != named.SelectedID {
		t.Fatal("autosave changed named selection")
	}
	restarted := &ProfileService{path: s.path}
	if got := restarted.Apply(""); !got.Success || *got.Config != draft {
		t.Fatal("complete Default did not survive restart")
	}
	if _, _, err := registryClient(RegistryConfig{URL: draft.URL, Topic: "topic", KeystorePath: draft.KeystorePath, Password: draft.Password}); err == nil {
		t.Fatal("draft bypassed Fetch validation")
	}
	edited := draft
	edited.URL = "https://changed.example.com"
	edited.Password = ""
	if !s.SaveDefault(edited, false).Success {
		t.Fatal("nonsecret save failed")
	}
	if got := s.Apply(""); got.Config.Password != draft.Password || got.Config.URL != edited.URL {
		t.Fatal("nonsecret edit erased password")
	}
	draft.URL = edited.URL
	if !s.Save(named.SelectedID, "Named", ProfileConnection{URL: "https://edited.example.com"}).Success {
		t.Fatal("named save failed")
	}
	if got := s.Apply(""); *got.Config != draft {
		t.Fatal("named edit overwrote Default")
	}
	s.Apply(named.SelectedID)
	if got := s.Delete(named.SelectedID); !got.Success || got.Config == nil || *got.Config != draft {
		t.Fatal("deletion did not restore Default")
	}
	if s.Delete("").Success || s.Delete("default").Success {
		t.Fatal("Default deletable")
	}
	if !s.SaveDefault(edited, true).Success || s.Apply("").Config.Password != "" {
		t.Fatal("explicit clear failed")
	}
	if !s.SaveDefault(ProfileConnection{}, true).Success || s.SaveDefault(ProfileConnection{URL: "https://u:p@example.com"}, false).Success {
		t.Fatal("Default draft validation failed")
	}
}
func TestLegacyProfilesMigrateWithoutCredentialManager(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(string(rune('0'+version)), func(t *testing.T) {
			s := profileFixture(t)
			id := "0123456789abcdef0123456789abcdef"
			record := profileRecord{RegistryProfile: RegistryProfile{ID: id, Name: "Legacy", URL: "https://legacy.example.com", KeystorePath: "keystore.jks", TruststorePath: "truststore.jks", HasPassword: true}, CredentialID: id}
			data := profileFile{Version: version, SelectedID: id, Profiles: []profileRecord{record}}
			if version == 2 {
				data.Default = emptyDefaultRecord()
				data.Default.HasPassword = true
				data.Default.CredentialID = id
			}
			raw, _ := json.Marshal(data)
			os.MkdirAll(filepath.Dir(s.path), 0700)
			if err := os.WriteFile(s.path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			got := s.Apply(id)
			if !got.Success || !got.Warning || got.CredentialAvailable || got.Config.Password != "" || got.Config.KeystorePath != "keystore.jks" {
				t.Fatal("legacy password re-entry not explicit")
			}
			disk, _ := os.ReadFile(s.path)
			if strings.Contains(string(disk), "credentialID") {
				t.Fatal("legacy vault reference retained")
			}
			if got := s.Apply(id); !got.Warning {
				t.Fatal("migration warning lost on restart")
			}
			c := *got.Config
			c.Password = "replacement-fixture"
			if !s.Save(id, "Legacy", c).Success {
				t.Fatal("password re-entry failed")
			}
			if loaded := (&ProfileService{path: s.path}).Apply(id); !loaded.CredentialAvailable || *loaded.Config != c || loaded.Warning {
				t.Fatal("local password replacement not restored")
			}
			if version == 2 {
				d := s.Apply("")
				if !d.Warning || d.CredentialAvailable {
					t.Fatal("Default legacy warning missing")
				}
				if r := s.SaveDefault(*d.Config, false); !r.Success || !r.Warning {
					t.Fatal("nonsecret autosave erased migration warning")
				}
				d.Config.Password = "default-fixture"
				if !s.SaveDefault(*d.Config, true).Success || s.Apply("").Config.Password != "default-fixture" {
					t.Fatal("Default legacy password replacement failed")
				}
			}
		})
	}
}
