package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/harsh-sonkar/env-pull/internal/project"
	"github.com/harsh-sonkar/env-pull/internal/store"
)

func TestLoadLocalProfileSecretsUsesProjectAndProfileScope(t *testing.T) {
	credentialStore := store.NewMemory()
	if err := credentialStore.Put("billing-api", "staging", map[string]string{"TOKEN": "staging-value"}); err != nil {
		t.Fatal(err)
	}
	config := project.Config{
		ProjectID: "billing-api",
		Profiles:  map[string]project.Profile{"staging": {Provider: "local"}},
	}

	got, err := loadLocalProfileSecrets(config, "staging", credentialStore)
	if err != nil {
		t.Fatalf("loadLocalProfileSecrets() error = %v", err)
	}
	if want := map[string]string{"TOKEN": "staging-value"}; !reflect.DeepEqual(got, want) {
		t.Errorf("loadLocalProfileSecrets() = %q, want %q", got, want)
	}
}

func TestLoadRemoteProfileSecretsRefreshesOptInCacheForOfflineUse(t *testing.T) {
	credentialStore := store.NewMemory()
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	config := project.Config{
		ProjectID: "billing-api",
		Cache:     project.CachePolicy{Enabled: true, MaxAge: project.Duration{Duration: time.Hour}},
		Profiles:  map[string]project.Profile{"staging": {Provider: "bitwarden"}},
	}
	fetches := 0
	fetch := func() (map[string]string, error) {
		fetches++
		return map[string]string{"TOKEN": "fresh-value"}, nil
	}

	got, err := loadRemoteProfileSecrets(config, "staging", false, credentialStore, false, now, fetch)
	if err != nil {
		t.Fatalf("fresh load error = %v", err)
	}
	if want := map[string]string{"TOKEN": "fresh-value"}; !reflect.DeepEqual(got, want) {
		t.Errorf("fresh load = %q, want %q", got, want)
	}

	got, err = loadRemoteProfileSecrets(config, "staging", true, credentialStore, false, now.Add(30*time.Minute), fetch)
	if err != nil {
		t.Fatalf("offline load error = %v", err)
	}
	if want := map[string]string{"TOKEN": "fresh-value"}; !reflect.DeepEqual(got, want) {
		t.Errorf("offline load = %q, want %q", got, want)
	}
	if fetches != 1 {
		t.Errorf("fetches = %d, want 1", fetches)
	}
}

func TestLoadRemoteProfileSecretsRejectsUnavailableOfflineCaches(t *testing.T) {
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	cachePolicy := project.CachePolicy{Enabled: true, MaxAge: project.Duration{Duration: time.Hour}}
	tests := []struct {
		name      string
		cache     project.CachePolicy
		profile   string
		cachedAt  time.Time
		cacheName string
		ci        bool
	}{
		{name: "caching disabled", cache: project.CachePolicy{}},
		{name: "cache missing", cache: cachePolicy},
		{name: "cache expired", cache: cachePolicy, cachedAt: now.Add(-time.Hour), cacheName: "default"},
		{name: "other profile cache", cache: cachePolicy, cachedAt: now, cacheName: "staging", profile: "default"},
		{name: "CI", cache: cachePolicy, cachedAt: now, cacheName: "default", ci: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			credentialStore := store.NewMemory()
			if test.cacheName != "" {
				if err := credentialStore.PutCache("billing-api", test.cacheName, map[string]string{"TOKEN": "cached-value"}, test.cachedAt); err != nil {
					t.Fatal(err)
				}
			}
			fetches := 0
			_, err := loadRemoteProfileSecrets(project.Config{ProjectID: "billing-api", Cache: test.cache}, test.profile, true, credentialStore, test.ci, now, func() (map[string]string, error) {
				fetches++
				return map[string]string{"TOKEN": "fresh-value"}, nil
			})
			if err == nil {
				t.Fatal("offline load error = nil, want unavailable cache error")
			}
			if fetches != 0 {
				t.Errorf("fetches = %d, want 0", fetches)
			}
		})
	}
}

func TestLoadRemoteProfileSecretsDoesNotAccessCacheInCI(t *testing.T) {
	credentialStore := &cacheAccessStore{Store: store.NewMemory()}
	config := project.Config{
		ProjectID: "billing-api",
		Cache:     project.CachePolicy{Enabled: true, MaxAge: project.Duration{Duration: time.Hour}},
	}
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	if _, err := loadRemoteProfileSecrets(config, "default", false, credentialStore, true, now, func() (map[string]string, error) {
		return map[string]string{"TOKEN": "fresh-value"}, nil
	}); err != nil {
		t.Fatalf("fresh CI load error = %v", err)
	}
	if _, err := loadRemoteProfileSecrets(config, "default", true, credentialStore, true, now, func() (map[string]string, error) {
		t.Fatal("offline CI load fetched remote secrets")
		return nil, nil
	}); err == nil {
		t.Fatal("offline CI load error = nil, want unavailable cache error")
	}
	if credentialStore.cacheReads != 0 || credentialStore.cacheWrites != 0 {
		t.Errorf("cache reads/writes = %d/%d, want 0/0", credentialStore.cacheReads, credentialStore.cacheWrites)
	}
}

func TestLoadProfileSecretsCachesBothRemoteProvidersForOfflineUse(t *testing.T) {
	directory := t.TempDir()
	originalDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(directory); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(originalDirectory) })

	config := `format_version = 1
project_id = "billing-api"

[cache]
enabled = true

[profiles.onepassword]
provider = "1password"
account = "acme"
vault = "Engineering"
item_id = "onepassword-note"

[profiles.bitwarden]
provider = "bitwarden"
item_id = "bitwarden-note"
`
	if err := os.WriteFile(project.FileName, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("op", []byte("#!/bin/sh\nprintf '%s\\n' '{\"fields\":[{\"id\":\"notesPlain\",\"value\":\"TOKEN=from-onepassword\\n\"}]}'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("bw", []byte("#!/bin/sh\nprintf 'TOKEN=from-bitwarden\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	credentialStore := store.NewMemory()
	originalStoreFactory := newCredentialStore
	newCredentialStore = func() store.Store { return credentialStore }
	t.Cleanup(func() { newCredentialStore = originalStoreFactory })
	t.Setenv("PATH", directory)

	for _, profileName := range []string{"onepassword", "bitwarden"} {
		fresh, err := loadProfileSecrets(profileName, false)
		if err != nil {
			t.Fatalf("fresh %s load: %v", profileName, err)
		}
		t.Setenv("PATH", t.TempDir())
		offline, err := loadProfileSecrets(profileName, true)
		if err != nil {
			t.Fatalf("offline %s load: %v", profileName, err)
		}
		if !reflect.DeepEqual(offline, fresh) {
			t.Errorf("offline %s secrets = %q, want %q", profileName, offline, fresh)
		}
		t.Setenv("PATH", directory)
	}
}

func TestExtractRunFlags(t *testing.T) {
	profile, offline, remaining := extractRunFlags([]string{"--profile=staging", "--offline", "--", "sh", "-c", "echo ok"})
	if profile != "staging" || !offline {
		t.Errorf("flags = profile %q, offline %t; want staging, true", profile, offline)
	}
	if want := []string{"--", "sh", "-c", "echo ok"}; !reflect.DeepEqual(remaining, want) {
		t.Errorf("remaining = %q, want %q", remaining, want)
	}
}

type cacheAccessStore struct {
	store.Store
	cacheReads  int
	cacheWrites int
}

func (store *cacheAccessStore) PutCache(projectID, profile string, secrets map[string]string, cachedAt time.Time) error {
	store.cacheWrites++
	return store.Store.PutCache(projectID, profile, secrets, cachedAt)
}

func (store *cacheAccessStore) GetCache(projectID, profile string) (map[string]string, time.Time, error) {
	store.cacheReads++
	return store.Store.GetCache(projectID, profile)
}

func TestRemoveProjectDeletesLocalProfilesAndConfiguration(t *testing.T) {
	directory := t.TempDir()
	config := `format_version = 1
project_id = "billing-api"

[profiles.default]
provider = "local"

[profiles.staging]
provider = "local"

[profiles.remote]
provider = "bitwarden"
item_id = "stable-note-id"
`
	if err := os.WriteFile(filepath.Join(directory, project.FileName), []byte(config), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	credentialStore := store.NewMemory()
	for _, profileName := range []string{"default", "staging"} {
		if err := credentialStore.Put("billing-api", profileName, map[string]string{"TOKEN": profileName}); err != nil {
			t.Fatal(err)
		}
	}
	if err := credentialStore.PutCache("billing-api", "remote", map[string]string{"TOKEN": "cached-value"}, time.Now()); err != nil {
		t.Fatal(err)
	}

	if err := runRemove(directory, credentialStore, true, io.Discard); err != nil {
		t.Fatalf("runRemove() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, project.FileName)); !os.IsNotExist(err) {
		t.Errorf("inject.toml stat error = %v, want deleted", err)
	}
	for _, profileName := range []string{"default", "staging"} {
		if _, err := credentialStore.Get("billing-api", profileName); err == nil {
			t.Errorf("local profile %q remains available", profileName)
		}
	}
	if _, _, err := credentialStore.GetCache("billing-api", "remote"); err == nil {
		t.Error("remote cache remains available")
	}
}

func TestRunRemovePreviewsAndCancelsWithoutChangingLocalState(t *testing.T) {
	directory := t.TempDir()
	config := `format_version = 1
project_id = "billing-api"

[profiles.local]
provider = "local"

[profiles.remote]
provider = "bitwarden"
item_id = "stable-note-id"
`
	if err := os.WriteFile(filepath.Join(directory, project.FileName), []byte(config), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	credentialStore := store.NewMemory()
	if err := credentialStore.Put("billing-api", "local", map[string]string{"TOKEN": "local-value"}); err != nil {
		t.Fatal(err)
	}
	if err := credentialStore.PutCache("billing-api", "remote", map[string]string{"TOKEN": "cached-value"}, time.Now()); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	err := runRemove(directory, credentialStore, false, &output)
	if err == nil {
		t.Fatal("runRemove() error = nil, want confirmation error")
	}
	if got := output.String(); !containsAll(got, "inject.toml", "local credential-store entry for profile \"local\"", "remote cache for profile \"remote\"") || containsAny(got, "local-value", "cached-value") {
		t.Errorf("preview = %q, want non-secret local state", got)
	}
	if _, err := os.Stat(filepath.Join(directory, project.FileName)); err != nil {
		t.Errorf("inject.toml stat error = %v, want configuration preserved", err)
	}
	if _, err := credentialStore.Get("billing-api", "local"); err != nil {
		t.Errorf("local profile unavailable after cancellation: %v", err)
	}
	if _, _, err := credentialStore.GetCache("billing-api", "remote"); err != nil {
		t.Errorf("remote cache unavailable after cancellation: %v", err)
	}
}

func TestRunRemovePreviewsPackageScriptRestorationWithoutMutation(t *testing.T) {
	directory := t.TempDir()
	config := `format_version = 1
project_id = "billing-api"

[profiles.default]
provider = "local"

[script_bindings.dev]
profile = "default"
package_manager = "npm"
wrapper = "inject __run-package-script \"dev\""
script = "inject:original:dev"
original = "vite"
pre_script = "inject:original:predev"
pre_original = "prepare"
`
	if err := os.WriteFile(filepath.Join(directory, project.FileName), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := []byte(`{"scripts":{"predev":"","dev":"inject __run-package-script \"dev\"","inject:original:predev":"prepare","inject:original:dev":"vite"}}`)
	manifestPath := filepath.Join(directory, "package.json")
	if err := os.WriteFile(manifestPath, manifest, 0o600); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	err := runRemove(directory, store.NewMemory(), false, &output)
	if err == nil {
		t.Fatal("runRemove() error = nil, want confirmation error")
	}
	if got := output.String(); !containsAll(got, `restore package.json script "dev"`, `restore package.json lifecycle hook "predev"`, `delete reserved package.json entry "inject:original:dev"`, `delete reserved package.json entry "inject:original:predev"`) || containsAny(got, "vite", "prepare") {
		t.Errorf("preview = %q, want non-secret manifest restoration", got)
	}
	if got, readErr := os.ReadFile(manifestPath); readErr != nil || !bytes.Equal(got, manifest) {
		t.Errorf("package.json = %q, %v; want unchanged", got, readErr)
	}
}

func TestRunRemoveRestoresOwnedPackageScripts(t *testing.T) {
	directory := t.TempDir()
	config := `format_version = 1
project_id = "billing-api"

[profiles.default]
provider = "local"

[script_bindings.dev]
profile = "default"
package_manager = "npm"
wrapper = "inject __run-package-script \"dev\""
script = "inject:original:dev"
original = "vite --host 0.0.0.0 | tee app.log"
pre_script = "inject:original:predev"
pre_original = "printf 'ready\\n'"
post_script = "inject:original:postdev"
post_original = "node -e \"console.log('done')\""
`
	if err := os.WriteFile(filepath.Join(directory, project.FileName), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(directory, "package.json")
	manifest := `{"name":"billing-api","scripts":{"dev":"inject __run-package-script \"dev\"","inject:original:dev":"vite --host 0.0.0.0 | tee app.log","inject:original:predev":"printf 'ready\\n'","inject:original:postdev":"node -e \"console.log('done')\"","test":"go test ./..."}}`
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := runRemove(directory, store.NewMemory(), true, io.Discard); err != nil {
		t.Fatalf("runRemove() error = %v", err)
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var restored struct {
		Name    string            `json:"name"`
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"predev":  `printf 'ready\n'`,
		"dev":     "vite --host 0.0.0.0 | tee app.log",
		"postdev": `node -e "console.log('done')"`,
		"test":    "go test ./...",
	}
	if restored.Name != "billing-api" || !reflect.DeepEqual(restored.Scripts, want) {
		t.Errorf("package.json = %#v, want name and restored scripts %#v", restored, want)
	}
}

func TestRunRemoveRefusesOwnedPackageScriptConflictsBeforeMutation(t *testing.T) {
	tests := []struct {
		name       string
		conflicted string
		value      string
	}{
		{name: "wrapper", conflicted: "dev", value: "manual wrapper"},
		{name: "preserved entry", conflicted: "inject:original:dev", value: "manual original"},
		{name: "lifecycle hook", conflicted: "predev", value: "manual hook"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			config := `format_version = 1
project_id = "billing-api"

[profiles.default]
provider = "local"

[script_bindings.dev]
profile = "default"
package_manager = "npm"
wrapper = "inject __run-package-script \"dev\""
script = "inject:original:dev"
original = "vite"
`
			configPath := filepath.Join(directory, project.FileName)
			if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			scripts := map[string]string{"dev": `inject __run-package-script "dev"`, "inject:original:dev": "vite"}
			scripts[test.conflicted] = test.value
			manifest, err := json.Marshal(map[string]any{"scripts": scripts})
			if err != nil {
				t.Fatal(err)
			}
			manifestPath := filepath.Join(directory, "package.json")
			if err := os.WriteFile(manifestPath, manifest, 0o600); err != nil {
				t.Fatal(err)
			}
			credentialStore := store.NewMemory()
			if err := credentialStore.Put("billing-api", "default", map[string]string{"TOKEN": "secret"}); err != nil {
				t.Fatal(err)
			}

			err = runRemove(directory, credentialStore, true, io.Discard)
			if err == nil || !strings.Contains(err.Error(), test.conflicted) {
				t.Fatalf("runRemove() error = %v, want conflict for %q", err, test.conflicted)
			}
			if got, readErr := os.ReadFile(manifestPath); readErr != nil || !bytes.Equal(got, manifest) {
				t.Errorf("package.json = %q, %v; want unchanged", got, readErr)
			}
			if _, statErr := os.Stat(configPath); statErr != nil {
				t.Errorf("inject.toml stat error = %v, want preserved", statErr)
			}
			if _, getErr := credentialStore.Get("billing-api", "default"); getErr != nil {
				t.Errorf("local profile unavailable after conflict: %v", getErr)
			}
		})
	}
}

func TestRunRemoveFailurePreservesConfigurationAndOtherProjects(t *testing.T) {
	directory := t.TempDir()
	config := `format_version = 1
project_id = "billing-api"

[profiles.local]
provider = "local"

[script_bindings.dev]
profile = "local"
package_manager = "npm"
wrapper = "inject __run-package-script \"dev\""
script = "inject:original:dev"
original = "vite"
`
	if err := os.WriteFile(filepath.Join(directory, project.FileName), []byte(config), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	manifest := []byte(`{"scripts":{"dev":"inject __run-package-script \"dev\"","inject:original:dev":"vite"}}`)
	manifestPath := filepath.Join(directory, "package.json")
	if err := os.WriteFile(manifestPath, manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	credentialStore := &failingDeleteStore{Store: store.NewMemory()}
	if err := credentialStore.Put("billing-api", "local", map[string]string{"TOKEN": "local-value"}); err != nil {
		t.Fatal(err)
	}
	if err := credentialStore.Put("other-project", "local", map[string]string{"TOKEN": "other-value"}); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	err := runRemove(directory, credentialStore, true, &output)
	if err == nil || strings.Contains(err.Error(), "local-value") {
		t.Fatalf("runRemove() error = %v, want non-secret deletion failure", err)
	}
	if _, err := os.Stat(filepath.Join(directory, project.FileName)); err != nil {
		t.Errorf("inject.toml stat error = %v, want configuration preserved", err)
	}
	if _, err := credentialStore.Get("billing-api", "local"); err != nil {
		t.Errorf("local profile unavailable after failed removal: %v", err)
	}
	if _, err := credentialStore.Get("other-project", "local"); err != nil {
		t.Errorf("other project profile unavailable after failed removal: %v", err)
	}
	if got, readErr := os.ReadFile(manifestPath); readErr != nil || !bytes.Equal(got, manifest) {
		t.Errorf("package.json = %q, %v; want rolled back", got, readErr)
	}
}

func TestRunRemoveRollsBackPartialCredentialCleanup(t *testing.T) {
	directory := t.TempDir()
	config := `format_version = 1
project_id = "billing-api"

[profiles.local]
provider = "local"

[profiles.remote]
provider = "bitwarden"
item_id = "remote-item"
`
	configPath := filepath.Join(directory, project.FileName)
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	credentialStore := &failingCacheDeleteStore{Store: store.NewMemory()}
	localSecrets := map[string]string{"TOKEN": "local-value"}
	if err := credentialStore.Put("billing-api", "local", localSecrets); err != nil {
		t.Fatal(err)
	}
	if err := credentialStore.PutCache("billing-api", "remote", map[string]string{"TOKEN": "cached-value"}, time.Now()); err != nil {
		t.Fatal(err)
	}

	err := runRemove(directory, credentialStore, true, io.Discard)
	if err == nil {
		t.Fatal("runRemove() error = nil, want cache deletion failure")
	}
	if got, getErr := credentialStore.Get("billing-api", "local"); getErr != nil || !reflect.DeepEqual(got, localSecrets) {
		t.Errorf("local profile = %q, %v; want restored", got, getErr)
	}
	if _, _, getErr := credentialStore.GetCache("billing-api", "remote"); getErr != nil {
		t.Errorf("remote cache unavailable after rollback: %v", getErr)
	}
	if _, statErr := os.Stat(configPath); statErr != nil {
		t.Errorf("inject.toml stat error = %v, want preserved", statErr)
	}
}

func TestRunRemoveDeletesConfigurationAfterCredentialCleanup(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, project.FileName)
	config := `format_version = 1
project_id = "billing-api"

[profiles.default]
provider = "local"
`
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	credentialStore := &configAwareStore{Store: store.NewMemory(), configPath: configPath}
	if err := credentialStore.Put("billing-api", "default", map[string]string{"TOKEN": "secret"}); err != nil {
		t.Fatal(err)
	}

	if err := runRemove(directory, credentialStore, true, io.Discard); err != nil {
		t.Fatalf("runRemove() error = %v", err)
	}
	if !credentialStore.configPresentDuringDelete {
		t.Error("inject.toml was not present during credential deletion")
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Errorf("inject.toml stat error = %v, want deleted", err)
	}
}

type failingDeleteStore struct {
	store.Store
}

func (store *failingDeleteStore) Delete(string, string) error {
	return errors.New("credential store unavailable")
}

type failingCacheDeleteStore struct {
	store.Store
}

func (store *failingCacheDeleteStore) DeleteCache(string, string) error {
	return errors.New("cache deletion unavailable")
}

type configAwareStore struct {
	store.Store
	configPath                string
	configPresentDuringDelete bool
}

func (store *configAwareStore) Delete(projectID, profile string) error {
	_, err := os.Stat(store.configPath)
	store.configPresentDuringDelete = err == nil
	return store.Store.Delete(projectID, profile)
}

func containsAll(value string, substrings ...string) bool {
	for _, substring := range substrings {
		if !strings.Contains(value, substring) {
			return false
		}
	}
	return true
}

func containsAny(value string, substrings ...string) bool {
	for _, substring := range substrings {
		if strings.Contains(value, substring) {
			return true
		}
	}
	return false
}
