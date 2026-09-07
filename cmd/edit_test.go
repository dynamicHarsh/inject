package cmd

import (
	"bytes"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/harsh-sonkar/env-pull/internal/project"
	"github.com/harsh-sonkar/env-pull/internal/store"
)

func TestRunConfiguredEditSelectsProfileAndDirectsRemoteEditingToProvider(t *testing.T) {
	credentialStore := &countingEditStore{Store: store.NewMemory()}
	config := project.Config{
		ProjectID: "billing-api",
		Profiles: map[string]project.Profile{
			"default": {Provider: "local"},
			"staging": {Provider: "bitwarden"},
		},
	}
	selected := []string{}

	err := runConfiguredEdit(config, credentialStore, func(profiles []editProfile) (string, error) {
		for _, profile := range profiles {
			selected = append(selected, profile.Name+":"+profile.Provider)
		}
		return "staging", nil
	}, nil)

	if err == nil || !strings.Contains(err.Error(), "Bitwarden") {
		t.Fatalf("runConfiguredEdit() error = %v, want Bitwarden provider guidance", err)
	}
	if got, want := strings.Join(selected, ","), "default:local,staging:bitwarden"; got != want {
		t.Errorf("profiles = %q, want %q", got, want)
	}
	if credentialStore.puts != 0 {
		t.Errorf("credential-store writes = %d, want 0", credentialStore.puts)
	}
}

func TestRunSecretEditorMasksValuesUntilExplicitRevealAndStagesEveryMutation(t *testing.T) {
	actions := []editAction{
		{Kind: editReveal, Name: "TOKEN"},
		{Kind: editAdd, Name: "REGION", Value: "us-east-1"},
		{Kind: editRename, Name: "TOKEN", NewName: "API_TOKEN"},
		{Kind: editUpdate, Name: "API_TOKEN", Value: "new-secret"},
		{Kind: editDelete, Name: "OLD_KEY"},
		{Kind: editConfirm},
	}
	views := []editView{}

	got, confirmed, err := runSecretEditor(map[string]string{"TOKEN": "old-secret", "OLD_KEY": "remove-me"}, func(view editView) (editAction, error) {
		views = append(views, view)
		action := actions[0]
		actions = actions[1:]
		return action, nil
	})

	if err != nil {
		t.Fatalf("runSecretEditor() error = %v", err)
	}
	if !confirmed {
		t.Fatal("runSecretEditor() confirmed = false, want true")
	}
	if value := viewValue(views[0], "TOKEN"); value != maskedSecret {
		t.Errorf("initial TOKEN display = %q, want masked", value)
	}
	if value := viewValue(views[1], "TOKEN"); value != "old-secret" {
		t.Errorf("revealed TOKEN display = %q, want explicit value", value)
	}
	want := map[string]string{"API_TOKEN": "new-secret", "REGION": "us-east-1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("edited secrets = %q, want %q", got, want)
	}
}

func TestRunConfiguredEditWritesCompleteSecretSetExactlyOnceAfterConfirmation(t *testing.T) {
	credentialStore := &countingEditStore{Store: store.NewMemory()}
	if err := credentialStore.Store.Put("billing-api", "default", map[string]string{"TOKEN": "old"}); err != nil {
		t.Fatal(err)
	}
	config := project.Config{ProjectID: "billing-api", Profiles: map[string]project.Profile{"default": {Provider: "local"}}}

	err := runConfiguredEdit(config, credentialStore,
		func([]editProfile) (string, error) { return "default", nil },
		func(_ string, secrets map[string]string) (map[string]string, bool, error) {
			secrets["TOKEN"] = "new"
			secrets["ADDED"] = "value"
			return secrets, true, nil
		})

	if err != nil {
		t.Fatalf("runConfiguredEdit() error = %v", err)
	}
	if credentialStore.puts != 1 {
		t.Fatalf("credential-store writes = %d, want 1", credentialStore.puts)
	}
	got, err := credentialStore.Get("billing-api", "default")
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"TOKEN": "new", "ADDED": "value"}; !reflect.DeepEqual(got, want) {
		t.Errorf("stored secrets = %q, want %q", got, want)
	}
}

func TestRunConfiguredEditCancellationAndValidationFailureDoNotWrite(t *testing.T) {
	tests := []struct {
		name   string
		action editAction
		want   string
	}{
		{name: "cancel", action: editAction{Kind: editCancel}},
		{name: "empty name", action: editAction{Kind: editAdd, Value: "secret"}, want: "invalid environment-variable name"},
		{name: "invalid name", action: editAction{Kind: editAdd, Name: "NOT PORTABLE", Value: "secret"}, want: "invalid environment-variable name"},
		{name: "duplicate name", action: editAction{Kind: editAdd, Name: "TOKEN", Value: "secret"}, want: "already exists"},
		{name: "duplicate rename", action: editAction{Kind: editRename, Name: "TOKEN", NewName: "OTHER"}, want: "already exists"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			credentialStore := &countingEditStore{Store: store.NewMemory()}
			if err := credentialStore.Store.Put("billing-api", "default", map[string]string{"TOKEN": "secret", "OTHER": "value"}); err != nil {
				t.Fatal(err)
			}
			config := project.Config{ProjectID: "billing-api", Profiles: map[string]project.Profile{"default": {Provider: "local"}}}

			err := runConfiguredEdit(config, credentialStore,
				func([]editProfile) (string, error) { return "default", nil },
				func(_ string, secrets map[string]string) (map[string]string, bool, error) {
					return runSecretEditor(secrets, func(editView) (editAction, error) { return test.action, nil })
				})

			if test.want == "" && err != nil {
				t.Fatalf("runConfiguredEdit() error = %v", err)
			}
			if test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("runConfiguredEdit() error = %v, want %q", err, test.want)
			}
			if credentialStore.puts != 0 {
				t.Errorf("credential-store writes = %d, want 0", credentialStore.puts)
			}
		})
	}
}

func TestRunEditKeepsConfiguredAndLegacyEditorsSeparated(t *testing.T) {
	config := project.Config{ProjectID: "billing-api", Profiles: map[string]project.Profile{"default": {Provider: "local"}}}
	credentialStore := store.NewMemory()
	if err := credentialStore.Put("billing-api", "default", map[string]string{"TOKEN": "secret"}); err != nil {
		t.Fatal(err)
	}

	legacyCalls := 0
	err := runEdit(
		func() (project.Config, error) { return config, nil },
		credentialStore,
		func([]editProfile) (string, error) { return "default", nil },
		func(_ string, secrets map[string]string) (map[string]string, bool, error) { return secrets, false, nil },
		func(string) error { legacyCalls++; return nil },
	)
	if err != nil {
		t.Fatalf("configured runEdit() error = %v", err)
	}
	if legacyCalls != 0 {
		t.Errorf("configured legacy editor calls = %d, want 0", legacyCalls)
	}

	err = runEdit(
		func() (project.Config, error) { return project.Config{}, fmt.Errorf("read: %w", os.ErrNotExist) },
		credentialStore, nil, nil,
		func(path string) error {
			legacyCalls++
			if path != defaultVaultFile {
				t.Errorf("legacy path = %q, want %q", path, defaultVaultFile)
			}
			return nil
		},
	)
	if err != nil {
		t.Fatalf("legacy runEdit() error = %v", err)
	}
	if legacyCalls != 1 {
		t.Errorf("legacy editor calls = %d, want 1", legacyCalls)
	}

	err = runEdit(
		func() (project.Config, error) { return project.Config{}, fmt.Errorf("invalid configuration") },
		credentialStore, nil, nil,
		func(string) error { legacyCalls++; return nil },
	)
	if err == nil || legacyCalls != 1 {
		t.Errorf("invalid config error/calls = %v/%d, want error and no legacy call", err, legacyCalls)
	}
}

func TestEditPromptsRunWithoutTerminalAndRevealOnlyAfterExplicitAction(t *testing.T) {
	var profileOutput bytes.Buffer
	selected, err := promptEditProfileWithIO([]editProfile{
		{Name: "default", Provider: "local"},
		{Name: "staging", Provider: "local"},
	}, &responseReader{responses: []string{"2"}}, &profileOutput, true)
	if err != nil {
		t.Fatalf("promptEditProfileWithIO() error = %v; output = %q", err, profileOutput.String())
	}
	if selected != "staging" {
		t.Errorf("selected profile = %q, want staging", selected)
	}

	var editorOutput bytes.Buffer
	_, confirmed, err := promptSecretSetWithIO("staging", map[string]string{"TOKEN": "top-secret"},
		&responseReader{responses: []string{"2", "1", "7"}}, &editorOutput, true)
	if err != nil {
		t.Fatalf("promptSecretSetWithIO() error = %v; output = %q", err, editorOutput.String())
	}
	if confirmed {
		t.Fatal("promptSecretSetWithIO() confirmed = true, want cancellation")
	}
	output := editorOutput.String()
	maskedAt := strings.Index(output, "TOKEN = "+maskedSecret)
	revealedAt := strings.Index(output, "TOKEN = top-secret")
	if maskedAt < 0 || revealedAt < 0 || revealedAt <= maskedAt {
		t.Errorf("editor output did not mask before explicit reveal: %q", output)
	}
}

func TestRunConfiguredEditRejectsEmptySecretSetWithoutWriting(t *testing.T) {
	credentialStore := &countingEditStore{Store: store.NewMemory()}
	if err := credentialStore.Store.Put("billing-api", "default", map[string]string{"TOKEN": "secret"}); err != nil {
		t.Fatal(err)
	}
	config := project.Config{ProjectID: "billing-api", Profiles: map[string]project.Profile{"default": {Provider: "local"}}}
	actions := []editAction{{Kind: editDelete, Name: "TOKEN"}, {Kind: editConfirm}}

	err := runConfiguredEdit(config, credentialStore,
		func([]editProfile) (string, error) { return "default", nil },
		func(_ string, secrets map[string]string) (map[string]string, bool, error) {
			return runSecretEditor(secrets, func(editView) (editAction, error) {
				action := actions[0]
				actions = actions[1:]
				return action, nil
			})
		})

	if err == nil || !strings.Contains(err.Error(), "must contain at least one") {
		t.Fatalf("runConfiguredEdit() error = %v, want empty secret-set rejection", err)
	}
	if credentialStore.puts != 0 {
		t.Errorf("credential-store writes = %d, want 0", credentialStore.puts)
	}
}

func viewValue(view editView, name string) string {
	for _, entry := range view.Entries {
		if entry.Name == name {
			return entry.Value
		}
	}
	return ""
}

type countingEditStore struct {
	store.Store
	puts int
}

func (credentialStore *countingEditStore) Put(projectID, profile string, secrets map[string]string) error {
	credentialStore.puts++
	return credentialStore.Store.Put(projectID, profile, secrets)
}
