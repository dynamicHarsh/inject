package setup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/harsh-sonkar/env-pull/internal/project"
)

func TestApplyProjectFilesReportsCommitAndRollbackFailures(t *testing.T) {
	directory := t.TempDir()
	manifestPath := filepath.Join(directory, "package.json")
	if err := os.WriteFile(manifestPath, []byte(`{"scripts":{"dev":"vite"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	config := project.Config{ScriptBindings: map[string]project.ScriptBinding{
		"dev": {Script: "inject:original:dev", Original: "vite", Wrapper: `inject __run-package-script "dev"`},
	}}
	commitErr := errors.New("config commit unavailable")
	rollbackErr := errors.New("manifest rollback unavailable")
	writes := 0

	err := applyProjectFiles(directory, nil, config, []byte("config"), nil, func(path string, data []byte, mode os.FileMode) error {
		writes++
		switch writes {
		case 1:
			return WriteFileAtomically(path, data, mode)
		case 2:
			return commitErr
		default:
			return rollbackErr
		}
	})
	if err == nil || !strings.Contains(err.Error(), commitErr.Error()) || !strings.Contains(err.Error(), rollbackErr.Error()) {
		t.Fatalf("applyProjectFiles() error = %v, want commit and rollback failures", err)
	}
}
