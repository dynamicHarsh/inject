// Package setup guides a project from a legacy .env workflow to inject.
package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/harsh-sonkar/env-pull/internal/executor"
	"github.com/harsh-sonkar/env-pull/internal/parser"
	"github.com/harsh-sonkar/env-pull/internal/project"
	"github.com/harsh-sonkar/env-pull/internal/store"
	"github.com/harsh-sonkar/env-pull/internal/vaults"
)

var ErrOnePasswordUnavailable = errors.New("1password unavailable")

var tomlBareKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

type Request struct {
	Directory           string
	ProjectID           string
	Provider            string
	Account             string
	Vault               string
	ItemID              string
	Item                string
	Binding             string
	Command             []string
	PackageScript       string
	PackageScripts      []string
	SelectPackageScript func([]string, string) (string, error)
	Validate            []string
	Local               bool
	SelectedInputs      []string
	Confirm             bool
	RemoveLegacyEnv     bool
	ConfirmRemoveEnv    bool
	NonInteractive      bool
	CheckOnePassword    func() error
	RunValidation       func([]string) error
	ResolveConflict     func(Conflict) (ConflictResolution, error)
	Store               store.Store
	Output              io.Writer
}

type Conflict struct {
	Script  string
	current string
}

type ConflictResolution int

const (
	RetainConflict ConflictResolution = iota
	ReplaceConflict
)

type Plan struct {
	ProjectID            string
	Source               string
	PlaintextInputs      []PlaintextInput
	DeveloperCommands    []DeveloperCommand
	ValidationCandidates []string
	ValidationCommand    []string
	PackageManager       string
	FileChanges          []string
	ConfigData           []byte
}

type PlaintextInput struct {
	Name       string
	Profile    string
	Selected   bool
	Precedence string
	Variables  []string
}

type DeveloperCommand struct {
	Name    string
	Default bool
}

type Discovery struct {
	ProjectID            string
	DefaultSource        string
	PlaintextInputs      []PlaintextInput
	SelectedInputs       []string
	DeveloperCommands    []DeveloperCommand
	SelectedCommands     []string
	ValidationCandidates []string
	PackageManager       string
}

// Discover returns non-secret project metadata and setup defaults.
func Discover(directory string) (Discovery, error) {
	if directory == "" {
		directory = "."
	}
	projectID, err := defaultProjectID(directory)
	if err != nil {
		return Discovery{}, err
	}
	inputs := detectPlaintextInputs(directory)
	scripts := candidatePackageScripts(directory)
	var existing *project.Config
	configPath := filepath.Join(directory, project.FileName)
	if _, exists := statFile(configPath); exists {
		config, err := project.Load(configPath)
		if err != nil {
			return Discovery{}, fmt.Errorf("setup: load existing configuration: %w", err)
		}
		existing = &config
		projectID = config.ProjectID
		for index := range inputs {
			_, inputs[index].Selected = config.Profiles[inputs[index].Profile]
		}
	}
	discovery := Discovery{
		ProjectID:       projectID,
		PlaintextInputs: inputs,
		PackageManager:  detectedPackageManager(directory),
		DefaultSource:   "1password",
	}
	if len(inputs) > 0 {
		discovery.DefaultSource = "local"
	}
	if existing != nil {
		if profile, ok := existing.Profiles["default"]; ok {
			discovery.DefaultSource = profile.Provider
		}
	}
	for _, input := range inputs {
		if input.Selected {
			discovery.SelectedInputs = append(discovery.SelectedInputs, input.Name)
		}
	}
	for _, name := range scripts {
		command := DeveloperCommand{Name: name, Default: isRuntimeScript(name)}
		discovery.DeveloperCommands = append(discovery.DeveloperCommands, command)
		if existing != nil {
			if _, selected := existing.ScriptBindings[name]; selected {
				discovery.SelectedCommands = append(discovery.SelectedCommands, name)
			}
		} else if command.Default {
			discovery.SelectedCommands = append(discovery.SelectedCommands, name)
		}
		if isFiniteValidationScript(name) {
			discovery.ValidationCandidates = append(discovery.ValidationCandidates, name)
		}
	}
	return discovery, nil
}

func detectedPackageManager(directory string) string {
	manager, _ := detectPackageManager(directory)
	return manager
}

// Run previews and, after confirmation, applies a non-secret inject configuration.
func Run(request Request) error {
	if request.Directory == "" {
		request.Directory = "."
	}
	if request.Output == nil {
		request.Output = io.Discard
	}
	configPath := filepath.Join(request.Directory, project.FileName)
	var existingConfig *project.Config
	if _, exists := statFile(configPath); exists {
		existing, err := project.Load(configPath)
		if err != nil {
			return fmt.Errorf("setup: load existing configuration: %w", err)
		}
		existingConfig = &existing
		applyExistingDefaults(&request, existing)
	}
	legacyEnvPath := filepath.Join(request.Directory, ".env")
	inputs := detectPlaintextInputs(request.Directory)
	if err := selectPlaintextInputs(inputs, request.SelectedInputs); err != nil {
		return err
	}
	selectSource(&request, len(inputs) > 0)
	if request.ProjectID == "" {
		projectID, err := defaultProjectID(request.Directory)
		if err != nil {
			return err
		}
		request.ProjectID = projectID
	}
	if err := validateRequest(request); err != nil {
		return err
	}
	if existingConfig != nil {
		if existingConfig.ProjectID != request.ProjectID {
			return fmt.Errorf("setup: existing project ID %q does not match %q", existingConfig.ProjectID, request.ProjectID)
		}
	}

	candidates := candidatePackageScripts(request.Directory)
	if request.PackageScript == "" && request.Binding == "" && request.SelectPackageScript != nil {
		selection, err := request.SelectPackageScript(candidates, defaultPackageScript(candidates))
		if err != nil {
			return err
		}
		request.PackageScript = selection
	}
	if request.PackageScript != "" && len(request.Command) == 0 {
		request.PackageScripts = append(request.PackageScripts, request.PackageScript)
	}

	var localProfiles map[string]map[string]string
	if request.Local {
		var err error
		localProfiles, err = composeLocalProfiles(request.Directory, inputs)
		if err != nil {
			return err
		}
	}
	config, err := plan(request, localProfiles, existingConfig)
	if err != nil {
		return err
	}
	retainedScripts, err := resolveOwnedScriptConflicts(request, existingConfig, &config)
	if err != nil {
		return err
	}
	configData, err := encodeConfig(config)
	if err != nil {
		return err
	}
	setupPlan := buildPlan(request, inputs, candidates, configData)
	previewPlan(request.Output, setupPlan)
	if !request.Confirm {
		fmt.Fprintln(request.Output, "No changes made; rerun with explicit confirmation")
		return nil
	}
	if !request.Local && effectiveProvider(request) == "1password" {
		if err := checkOnePassword(request); err != nil {
			fmt.Fprintln(request.Output, "1Password is unavailable; run `op signin` and retry")
			return err
		}
	}
	if !request.Local || request.RemoveLegacyEnv || len(request.Validate) > 0 {
		if err := validateValidationCommand(request.Validate); err != nil {
			return err
		}
	}
	var rollbackStore func() error
	if request.Local {
		if request.Store == nil {
			return fmt.Errorf("setup: local credential store is unavailable")
		}
		if len(localProfiles) == 0 {
			return fmt.Errorf("setup: select at least one plaintext input for local setup")
		}
		if len(request.Validate) > 0 {
			if err := runValidation(request, localProfiles["default"]); err != nil {
				return fmt.Errorf("setup: validation failed: %w", err)
			}
		}
		affectedProfiles := make(map[string]map[string]string, len(localProfiles))
		for profile, secrets := range localProfiles {
			affectedProfiles[profile] = secrets
		}
		if existingConfig != nil {
			for profile, existingProfile := range existingConfig.Profiles {
				if existingProfile.Provider == "local" {
					if _, retained := localProfiles[profile]; !retained {
						affectedProfiles[profile] = nil
					}
				}
			}
		}
		snapshots, err := snapshotProfiles(request.Store, request.ProjectID, affectedProfiles)
		if err != nil {
			return err
		}
		rollbackStore = func() error {
			return restoreProfiles(request.Store, request.ProjectID, snapshots)
		}
		for _, profile := range sortedKeys(localProfiles) {
			secrets := localProfiles[profile]
			if err := request.Store.Put(request.ProjectID, profile, secrets); err != nil {
				if rollbackErr := rollbackStore(); rollbackErr != nil {
					return errors.Join(fmt.Errorf("setup: save local profile %q: %w", profile, err), rollbackErr)
				}
				return fmt.Errorf("setup: save local profile %q: %w", profile, err)
			}
		}
		for _, profile := range sortedKeys(affectedProfiles) {
			if _, retained := localProfiles[profile]; retained {
				continue
			}
			if err := request.Store.Delete(request.ProjectID, profile); err != nil && !errors.Is(err, store.ErrUnavailable) {
				if rollbackErr := rollbackStore(); rollbackErr != nil {
					return errors.Join(fmt.Errorf("setup: remove local profile %q: %w", profile, err), rollbackErr)
				}
				return fmt.Errorf("setup: remove local profile %q: %w", profile, err)
			}
		}
	}

	if !request.Local {
		if err := runValidation(request, nil); err != nil {
			return fmt.Errorf("setup: validation failed: %w", err)
		}
	}
	if err := applyProjectFiles(request.Directory, existingConfig, config, configData, retainedScripts, WriteFileAtomically); err != nil {
		if rollbackStore != nil {
			if rollbackErr := rollbackStore(); rollbackErr != nil {
				return errors.Join(err, rollbackErr)
			}
		}
		return err
	}
	fmt.Fprintln(request.Output, "Validation succeeded")
	if request.RemoveLegacyEnv && hasPlaintextInput(inputs, ".env") {
		if !request.ConfirmRemoveEnv {
			fmt.Fprintln(request.Output, "Legacy .env was retained; its removal requires explicit confirmation")
			return nil
		}
		if err := os.Remove(legacyEnvPath); err != nil {
			return fmt.Errorf("setup: remove legacy .env: %w", err)
		}
		fmt.Fprintln(request.Output, "Removed legacy .env")
	}
	return nil
}

func applyExistingDefaults(request *Request, existing project.Config) {
	if request.ProjectID == "" {
		request.ProjectID = existing.ProjectID
	}
	if !request.Local && request.Provider == "" && !hasRemoteReference(*request) {
		if profile, exists := existing.Profiles["default"]; exists {
			request.Provider = profile.Provider
			request.Local = profile.Provider == "local"
			request.Account, request.Vault = profile.Account, profile.Vault
			request.ItemID, request.Item = profile.ItemID, profile.Item
			if request.Local {
				request.Provider = ""
			}
		}
	}
	if request.SelectedInputs == nil && request.Local {
		for _, profile := range sortedKeys(existing.Profiles) {
			name := ".env." + profile
			if profile == "default" {
				name = ".env"
			}
			request.SelectedInputs = append(request.SelectedInputs, name)
		}
	}
	if request.PackageScripts == nil && request.PackageScript == "" && request.Binding == "" {
		request.PackageScripts = sortedKeys(existing.ScriptBindings)
	}
}

type profileSnapshot struct {
	secrets map[string]string
	exists  bool
}

func snapshotProfiles(credentialStore store.Store, projectID string, profiles map[string]map[string]string) (map[string]profileSnapshot, error) {
	snapshots := make(map[string]profileSnapshot, len(profiles))
	for _, profile := range sortedKeys(profiles) {
		secrets, err := credentialStore.Get(projectID, profile)
		if err == nil {
			snapshots[profile] = profileSnapshot{secrets: secrets, exists: true}
			continue
		}
		if !errors.Is(err, store.ErrUnavailable) {
			return nil, fmt.Errorf("setup: snapshot local profile %q: %w", profile, err)
		}
		snapshots[profile] = profileSnapshot{}
	}
	return snapshots, nil
}

func restoreProfiles(credentialStore store.Store, projectID string, snapshots map[string]profileSnapshot) error {
	var rollbackErrors []error
	for _, profile := range sortedKeys(snapshots) {
		snapshot := snapshots[profile]
		if snapshot.exists {
			if err := credentialStore.Put(projectID, profile, snapshot.secrets); err != nil {
				rollbackErrors = append(rollbackErrors, fmt.Errorf("setup: restore local profile %q: %w", profile, err))
			}
			continue
		}
		if err := credentialStore.Delete(projectID, profile); err != nil && !errors.Is(err, store.ErrUnavailable) {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("setup: remove local profile %q during rollback: %w", profile, err))
		}
	}
	return errors.Join(rollbackErrors...)
}

func selectSource(request *Request, plaintextInputExists bool) {
	if request.Local || request.Provider != "" || hasRemoteReference(*request) || !plaintextInputExists || request.NonInteractive {
		return
	}
	request.Local = true
}

func buildPlan(request Request, inputs []PlaintextInput, scripts []string, configData []byte) Plan {
	packageManager, _ := detectPackageManager(request.Directory)
	plan := Plan{
		ProjectID:         request.ProjectID,
		Source:            effectiveProvider(request),
		PlaintextInputs:   inputs,
		ValidationCommand: append([]string(nil), request.Validate...),
		PackageManager:    packageManager,
		FileChanges:       []string{"create inject.toml"},
		ConfigData:        configData,
	}
	if request.Local {
		plan.Source = "local"
	}
	for _, name := range scripts {
		plan.DeveloperCommands = append(plan.DeveloperCommands, DeveloperCommand{Name: name, Default: isRuntimeScript(name)})
		if isFiniteValidationScript(name) {
			plan.ValidationCandidates = append(plan.ValidationCandidates, name)
		}
	}
	if request.RemoveLegacyEnv {
		plan.FileChanges = append(plan.FileChanges, "remove .env after successful validation")
	}
	if len(request.PackageScripts) > 0 {
		plan.FileChanges = append(plan.FileChanges, "modify package.json scripts")
	}
	return plan
}

func previewPlan(output io.Writer, plan Plan) {
	fmt.Fprintln(output, "Setup plan:")
	fmt.Fprintf(output, "Project ID: %s\n", plan.ProjectID)
	switch plan.Source {
	case "local":
		fmt.Fprintln(output, "Source choices: Local (selected), 1Password, Bitwarden")
	case "bitwarden":
		fmt.Fprintln(output, "Source choices: Local, 1Password, Bitwarden (selected)")
	default:
		fmt.Fprintln(output, "Source choices: Local, 1Password (selected), Bitwarden")
	}
	for _, input := range plan.PlaintextInputs {
		state := "available"
		if input.Selected {
			state = "selected"
		}
		details := fmt.Sprintf("%s; profile %s", state, input.Profile)
		if input.Precedence != "" {
			details += "; precedence " + input.Precedence
		}
		fmt.Fprintf(output, "Plaintext input: %s (%s)\n", input.Name, details)
		if input.Selected {
			fmt.Fprintf(output, "Variables for profile %s: %s\n", input.Profile, strings.Join(input.Variables, ", "))
		}
	}
	for _, command := range plan.DeveloperCommands {
		if command.Default {
			fmt.Fprintf(output, "Developer command: %s (default)\n", command.Name)
			continue
		}
		fmt.Fprintf(output, "Developer command: %s\n", command.Name)
	}
	for _, name := range plan.ValidationCandidates {
		fmt.Fprintf(output, "Validation candidate: %s\n", name)
	}
	if len(plan.ValidationCommand) > 0 {
		command, _ := json.Marshal(plan.ValidationCommand)
		fmt.Fprintf(output, "Selected validation: %s\n", command)
	}
	fmt.Fprintf(output, "Package manager: %s\n", plan.PackageManager)
	for _, change := range plan.FileChanges {
		fmt.Fprintf(output, "File change: %s\n", change)
	}
	fmt.Fprintln(output, "Will write inject.toml:")
	fmt.Fprint(output, string(plan.ConfigData))
}

func detectPlaintextInputs(directory string) []PlaintextInput {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil
	}
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !isPlaintextInputName(name) {
			continue
		}
		names = append(names, name)
	}
	sort.Slice(names, func(left, right int) bool {
		if names[left] == ".env" {
			return true
		}
		if names[right] == ".env" {
			return false
		}
		return names[left] < names[right]
	})
	inputs := make([]PlaintextInput, 0, len(names))
	for _, name := range names {
		input := PlaintextInput{Name: name, Profile: "default", Selected: name == ".env"}
		if name != ".env" {
			input.Profile = strings.TrimPrefix(name, ".env.")
			if hasPlaintextInputName(names, ".env") {
				input.Precedence = ".env < " + name
			}
		}
		inputs = append(inputs, input)
	}
	return inputs
}

func selectPlaintextInputs(inputs []PlaintextInput, selected []string) error {
	if selected == nil {
		return nil
	}
	selectedNames := make(map[string]bool, len(selected)+1)
	for _, name := range selected {
		if selectedNames[name] {
			return fmt.Errorf("setup: plaintext input %q selected more than once", name)
		}
		selectedNames[name] = true
	}
	if hasPlaintextInput(inputs, ".env") && len(selectedNames) > 0 {
		selectedNames[".env"] = true
	}
	for index := range inputs {
		inputs[index].Selected = selectedNames[inputs[index].Name]
		delete(selectedNames, inputs[index].Name)
	}
	if len(selectedNames) > 0 {
		for name := range selectedNames {
			return fmt.Errorf("setup: plaintext input %q is not available", name)
		}
	}
	return nil
}

func composeLocalProfiles(directory string, inputs []PlaintextInput) (map[string]map[string]string, error) {
	profiles := make(map[string]map[string]string)
	var base map[string]string
	for index := range inputs {
		if !inputs[index].Selected {
			continue
		}
		data, err := os.ReadFile(filepath.Join(directory, inputs[index].Name))
		if err != nil {
			return nil, fmt.Errorf("setup: read plaintext input %q: %w", inputs[index].Name, err)
		}
		secrets, err := parser.Parse(data)
		if err != nil {
			return nil, fmt.Errorf("setup: import plaintext input %q: %w", inputs[index].Name, err)
		}
		if inputs[index].Name == ".env" {
			base = secrets
		}
		complete := make(map[string]string, len(base)+len(secrets))
		for name, value := range base {
			complete[name] = value
		}
		for name, value := range secrets {
			complete[name] = value
		}
		inputs[index].Variables = sortedKeys(complete)
		profiles[inputs[index].Profile] = complete
	}
	return profiles, nil
}

func sortedKeys[Value any](values map[string]Value) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func isPlaintextInputName(name string) bool {
	if name == ".env" {
		return true
	}
	if !strings.HasPrefix(name, ".env.") || len(name) == len(".env.") {
		return false
	}
	excluded := map[string]bool{
		"example": true, "examples": true, "sample": true, "template": true,
		"tmpl": true, "dist": true, "backup": true, "bak": true, "old": true,
		"orig": true, "generated": true,
	}
	for _, part := range strings.Split(strings.TrimPrefix(name, ".env."), ".") {
		if excluded[strings.ToLower(part)] {
			return false
		}
	}
	return true
}

func hasPlaintextInput(inputs []PlaintextInput, name string) bool {
	for _, input := range inputs {
		if input.Name == name {
			return true
		}
	}
	return false
}

func hasPlaintextInputName(inputs []string, name string) bool {
	for _, input := range inputs {
		if input == name {
			return true
		}
	}
	return false
}

func isRuntimeScript(name string) bool {
	switch strings.ToLower(name) {
	case "dev", "start", "serve":
		return true
	default:
		return false
	}
}

func isFiniteValidationScript(name string) bool {
	switch strings.ToLower(name) {
	case "test", "check", "lint", "typecheck", "type-check", "build":
		return true
	default:
		return false
	}
}

func detectPackageManager(directory string) (string, error) {
	if data, err := os.ReadFile(filepath.Join(directory, "package.json")); err == nil {
		var manifest struct {
			PackageManager string `json:"packageManager"`
		}
		if json.Unmarshal(data, &manifest) == nil {
			family, _, _ := strings.Cut(manifest.PackageManager, "@")
			switch family {
			case "npm", "pnpm", "yarn", "bun":
				return family, nil
			case "":
			default:
				return "", fmt.Errorf("setup: unsupported package manager %q", family)
			}
		}
	}
	families := make(map[string]bool)
	for _, candidate := range []struct {
		file   string
		family string
	}{
		{file: "pnpm-lock.yaml", family: "pnpm"},
		{file: "yarn.lock", family: "yarn"},
		{file: "bun.lock", family: "bun"},
		{file: "bun.lockb", family: "bun"},
		{file: "package-lock.json", family: "npm"},
	} {
		if _, exists := statFile(filepath.Join(directory, candidate.file)); exists {
			families[candidate.family] = true
		}
	}
	if len(families) == 0 {
		return "", fmt.Errorf("setup: package manager is not detected")
	}
	if len(families) > 1 {
		return "", fmt.Errorf("setup: package manager metadata is ambiguous")
	}
	for family := range families {
		return family, nil
	}
	panic("unreachable")
}

func hasRemoteReference(request Request) bool {
	return request.Account != "" || request.Vault != "" || request.ItemID != "" || request.Item != ""
}

func effectiveProvider(request Request) string {
	if request.Local {
		return "local"
	}
	if request.Provider != "" {
		return request.Provider
	}
	if hasRemoteReference(request) {
		return "1password"
	}
	return ""
}

func defaultProjectID(directory string) (string, error) {
	if data, err := os.ReadFile(filepath.Join(directory, "package.json")); err == nil {
		var manifest struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(data, &manifest) == nil && strings.TrimSpace(manifest.Name) != "" {
			return manifest.Name, nil
		}
	}
	if root, err := exec.Command("git", "-C", directory, "rev-parse", "--show-toplevel").Output(); err == nil {
		return filepath.Base(strings.TrimSpace(string(root))), nil
	}
	absoluteDirectory, err := filepath.Abs(directory)
	if err != nil {
		return "", fmt.Errorf("setup: determine project directory: %w", err)
	}
	return filepath.Base(absoluteDirectory), nil
}

func validateRequest(request Request) error {
	if strings.TrimSpace(request.ProjectID) == "" {
		return fmt.Errorf("setup: project ID is required")
	}
	provider := effectiveProvider(request)
	if provider == "" {
		return fmt.Errorf("setup: source is required (local, 1password, or bitwarden)")
	}
	if provider != "local" && provider != "1password" && provider != "bitwarden" {
		return fmt.Errorf("setup: unsupported provider %q", request.Provider)
	}
	if request.Provider != "" && request.Local {
		return fmt.Errorf("setup: provider and local source cannot be selected together")
	}
	if request.Local {
		return nil
	}
	if provider == "1password" && (strings.TrimSpace(request.Account) == "" || strings.TrimSpace(request.Vault) == "") {
		return fmt.Errorf("setup: project ID, 1Password account, and vault are required")
	}
	if strings.TrimSpace(request.ItemID) == "" && strings.TrimSpace(request.Item) == "" {
		return fmt.Errorf("setup: a remote item ID or item name is required")
	}
	if len(request.PackageScripts) > 0 && request.Binding != "" {
		return fmt.Errorf("setup: package scripts and explicit binding cannot be selected together")
	}
	if request.PackageScript != "" && request.Binding != "" && request.Binding != request.PackageScript {
		return fmt.Errorf("setup: package script and binding must have the same name")
	}
	if request.Binding != "" && len(request.Command) == 0 && request.PackageScript == "" {
		return fmt.Errorf("setup: binding %q requires a command", request.Binding)
	}
	return nil
}

func checkOnePassword(request Request) error {
	if request.CheckOnePassword != nil {
		return request.CheckOnePassword()
	}
	path, err := exec.LookPath("op")
	if err != nil {
		return ErrOnePasswordUnavailable
	}
	if err := exec.Command(path, "account", "list").Run(); err != nil {
		return ErrOnePasswordUnavailable
	}
	return nil
}

func plan(request Request, localProfiles map[string]map[string]string, existing *project.Config) (project.Config, error) {
	profile := project.Profile{
		Provider: effectiveProvider(request), Account: request.Account, Vault: request.Vault, ItemID: request.ItemID, Item: request.Item,
	}
	if request.Local {
		profile = project.Profile{Provider: "local"}
	}
	config := project.Config{
		FormatVersion:  1,
		ProjectID:      request.ProjectID,
		Profiles:       map[string]project.Profile{"default": profile},
		Commands:       map[string]project.Binding{},
		ScriptBindings: map[string]project.ScriptBinding{},
	}
	if request.Local && len(localProfiles) > 0 {
		config.Profiles = make(map[string]project.Profile, len(localProfiles))
		for name := range localProfiles {
			config.Profiles[name] = project.Profile{Provider: "local"}
		}
	}
	if len(request.PackageScripts) == 0 && request.Binding == "" && request.PackageScript == "" {
		return config, nil
	}
	if len(request.PackageScripts) > 0 {
		bindings, err := packageScriptBindings(request.Directory, request.PackageScripts, existing)
		if err != nil {
			return project.Config{}, err
		}
		config.ScriptBindings = bindings
		return config, nil
	}
	binding := request.Binding
	if binding == "" {
		binding = request.PackageScript
	}
	if request.PackageScript != "" {
		if err := validatePackageScript(request.Directory, request.PackageScript); err != nil {
			return project.Config{}, err
		}
	}
	if len(request.Command) == 0 {
		return project.Config{}, fmt.Errorf("setup: binding %q requires a command", binding)
	}
	config.Commands[binding] = project.Binding{Command: request.Command}
	return config, nil
}

func encodeConfig(config project.Config) ([]byte, error) {
	data := []byte(fmt.Sprintf("format_version = 1\nproject_id = %q\n", config.ProjectID))
	profileNames := sortedKeys(config.Profiles)
	for _, name := range profileNames {
		profile := config.Profiles[name]
		data = append(data, fmt.Sprintf("\n[profiles.%s]\nprovider = %q\n", tomlKey(name), profile.Provider)...)
		if profile.Provider == "1password" {
			data = append(data, fmt.Sprintf("account = %q\nvault = %q\n", profile.Account, profile.Vault)...)
		}
		if profile.Provider == "local" {
			continue
		}
		if profile.ItemID != "" {
			data = append(data, fmt.Sprintf("item_id = %q\n", profile.ItemID)...)
		} else {
			data = append(data, fmt.Sprintf("item = %q\n", profile.Item)...)
		}
	}
	for name, binding := range config.Commands {
		command, err := json.Marshal(binding.Command)
		if err != nil {
			return nil, err
		}
		data = append(data, fmt.Sprintf("\n[commands.%s]\nprofile = \"default\"\ncommand = %s\n", name, command)...)
	}
	for _, name := range sortedKeys(config.ScriptBindings) {
		binding := config.ScriptBindings[name]
		data = append(data, fmt.Sprintf("\n[script_bindings.%s]\nprofile = %q\npackage_manager = %q\nwrapper = %q\nscript = %q\noriginal = %q\n", tomlKey(name), binding.Profile, binding.PackageManager, binding.Wrapper, binding.Script, binding.Original)...)
		if binding.PreScript != "" {
			data = append(data, fmt.Sprintf("pre_script = %q\npre_original = %q\n", binding.PreScript, binding.PreOriginal)...)
		}
		if binding.PostScript != "" {
			data = append(data, fmt.Sprintf("post_script = %q\npost_original = %q\n", binding.PostScript, binding.PostOriginal)...)
		}
	}
	if _, err := project.Parse(data); err != nil {
		return nil, fmt.Errorf("setup: invalid planned configuration: %w", err)
	}
	return data, nil
}

func tomlKey(name string) string {
	if tomlBareKey.MatchString(name) {
		return name
	}
	return fmt.Sprintf("%q", name)
}

func validatePackageScript(directory, script string) error {
	data, err := os.ReadFile(filepath.Join(directory, "package.json"))
	if err != nil {
		return fmt.Errorf("setup: read package.json: %w", err)
	}
	var manifest struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("setup: invalid package.json: %w", err)
	}
	if _, exists := manifest.Scripts[script]; !exists {
		return fmt.Errorf("setup: package.json has no %q script", script)
	}
	return nil
}

func packageScriptBindings(directory string, selected []string, existing *project.Config) (map[string]project.ScriptBinding, error) {
	data, err := os.ReadFile(filepath.Join(directory, "package.json"))
	if err != nil {
		return nil, fmt.Errorf("setup: read package.json: %w", err)
	}
	var manifest struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("setup: invalid package.json: %w", err)
	}
	manager, err := detectPackageManager(directory)
	if err != nil {
		return nil, err
	}
	bindings := make(map[string]project.ScriptBinding, len(selected))
	for _, name := range selected {
		if existing != nil {
			if binding, owned := existing.ScriptBindings[name]; owned {
				bindings[name] = binding
				continue
			}
		}
		original, exists := manifest.Scripts[name]
		if !exists {
			return nil, fmt.Errorf("setup: package.json has no %q script", name)
		}
		if _, duplicate := bindings[name]; duplicate {
			return nil, fmt.Errorf("setup: package script %q selected more than once", name)
		}
		binding := project.ScriptBinding{
			Profile: "default", PackageManager: manager,
			Wrapper: "inject __run-package-script " + strconv.Quote(name),
			Script:  reservedScriptName(name), Original: original,
		}
		if value, exists := manifest.Scripts["pre"+name]; exists {
			binding.PreScript = reservedScriptName("pre" + name)
			binding.PreOriginal = value
		}
		if value, exists := manifest.Scripts["post"+name]; exists {
			binding.PostScript = reservedScriptName("post" + name)
			binding.PostOriginal = value
		}
		for _, generated := range []string{binding.Script, binding.PreScript, binding.PostScript} {
			if generated == "" {
				continue
			}
			for _, collision := range []string{generated, "pre" + generated, "post" + generated} {
				if _, exists := manifest.Scripts[collision]; exists {
					return nil, fmt.Errorf("setup: reserved package script %q already exists", collision)
				}
			}
		}
		bindings[name] = binding
	}
	return bindings, nil
}

func resolveOwnedScriptConflicts(request Request, existing *project.Config, planned *project.Config) (map[string]string, error) {
	if existing == nil || len(existing.ScriptBindings) == 0 {
		return nil, nil
	}
	data, err := os.ReadFile(filepath.Join(request.Directory, "package.json"))
	if err != nil {
		return nil, fmt.Errorf("setup: read package.json: %w", err)
	}
	var manifest struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("setup: invalid package.json: %w", err)
	}
	var conflicts []Conflict
	for _, name := range sortedKeys(existing.ScriptBindings) {
		conflicts = append(conflicts, ownedPackageScriptConflicts(name, existing.ScriptBindings[name], manifest.Scripts)...)
	}
	if len(conflicts) == 0 {
		return nil, nil
	}
	if request.NonInteractive || request.ResolveConflict == nil {
		names := make([]string, 0, len(conflicts))
		for _, conflict := range conflicts {
			names = append(names, strconv.Quote(conflict.Script))
		}
		return nil, fmt.Errorf("setup: owned package scripts were modified: %s", strings.Join(names, ", "))
	}
	retained := make(map[string]string)
	for _, conflict := range conflicts {
		resolution, err := request.ResolveConflict(conflict)
		if err != nil {
			return nil, err
		}
		if resolution == RetainConflict {
			retained[conflict.Script] = conflict.current
			adoptRetainedScript(planned, conflict)
		}
	}
	return retained, nil
}

func adoptRetainedScript(config *project.Config, conflict Conflict) {
	for name, binding := range config.ScriptBindings {
		switch conflict.Script {
		case name:
			binding.Wrapper = conflict.current
		case binding.Script:
			binding.Original = conflict.current
		case binding.PreScript:
			binding.PreOriginal = conflict.current
		case binding.PostScript:
			binding.PostOriginal = conflict.current
		default:
			continue
		}
		config.ScriptBindings[name] = binding
		return
	}
}

func ownedPackageScriptConflicts(name string, binding project.ScriptBinding, scripts map[string]string) []Conflict {
	expected := map[string]string{name: binding.Wrapper, binding.Script: binding.Original}
	if binding.PreScript != "" {
		expected[binding.PreScript] = binding.PreOriginal
	}
	if binding.PostScript != "" {
		expected[binding.PostScript] = binding.PostOriginal
	}
	var conflicts []Conflict
	for _, entry := range sortedKeys(expected) {
		if scripts[entry] != expected[entry] {
			conflicts = append(conflicts, Conflict{Script: entry, current: scripts[entry]})
		}
	}
	for _, lifecycle := range []string{"pre" + name, "post" + name} {
		if _, exists := scripts[lifecycle]; exists {
			conflicts = append(conflicts, Conflict{Script: lifecycle, current: scripts[lifecycle]})
		}
	}
	return conflicts
}

func reservedScriptName(name string) string { return "inject:original:" + name }

func applyProjectFiles(directory string, existing *project.Config, config project.Config, configData []byte, retainedScripts map[string]string, writeFile func(string, []byte, os.FileMode) error) error {
	manifestPath := filepath.Join(directory, "package.json")
	var originalManifest []byte
	var existingBindings map[string]project.ScriptBinding
	if existing != nil {
		existingBindings = existing.ScriptBindings
	}
	if len(existingBindings) > 0 || len(config.ScriptBindings) > 0 {
		var err error
		originalManifest, err = os.ReadFile(manifestPath)
		if err != nil {
			return fmt.Errorf("setup: read package.json: %w", err)
		}
		updatedManifest, err := rewritePackageScripts(originalManifest, existingBindings, config.ScriptBindings, retainedScripts)
		if err != nil {
			return err
		}
		if err := writeFile(manifestPath, updatedManifest, 0o644); err != nil {
			return fmt.Errorf("setup: write package.json: %w", err)
		}
	}
	if err := writeFile(filepath.Join(directory, project.FileName), configData, 0o644); err != nil {
		commitErr := fmt.Errorf("setup: write inject.toml: %w", err)
		if originalManifest != nil {
			if rollbackErr := writeFile(manifestPath, originalManifest, 0o644); rollbackErr != nil {
				return errors.Join(commitErr, fmt.Errorf("setup: restore package.json: %w", rollbackErr))
			}
		}
		return commitErr
	}
	return nil
}

func WriteFileAtomically(path string, data []byte, mode os.FileMode) (err error) {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".inject-setup-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}()
	if err = temporary.Chmod(mode); err != nil {
		return err
	}
	if _, err = temporary.Write(data); err != nil {
		return err
	}
	if err = temporary.Sync(); err != nil {
		return err
	}
	if err = temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func RestorePackageScripts(data []byte, bindings map[string]project.ScriptBinding) ([]byte, error) {
	var manifest struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("setup: invalid package.json: %w", err)
	}
	var conflicts []Conflict
	for _, name := range sortedKeys(bindings) {
		conflicts = append(conflicts, ownedPackageScriptConflicts(name, bindings[name], manifest.Scripts)...)
	}
	if len(conflicts) > 0 {
		names := make([]string, 0, len(conflicts))
		for _, conflict := range conflicts {
			names = append(names, strconv.Quote(conflict.Script))
		}
		return nil, fmt.Errorf("owned package scripts were modified: %s", strings.Join(names, ", "))
	}
	return rewritePackageScripts(data, bindings, nil, nil)
}

func rewritePackageScripts(data []byte, existing, bindings map[string]project.ScriptBinding, retained map[string]string) ([]byte, error) {
	var manifest map[string]json.RawMessage
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("setup: invalid package.json: %w", err)
	}
	var scripts map[string]string
	if err := json.Unmarshal(manifest["scripts"], &scripts); err != nil {
		return nil, fmt.Errorf("setup: invalid package.json scripts: %w", err)
	}
	for name, binding := range existing {
		if _, retained := bindings[name]; retained {
			continue
		}
		if _, keep := retained[name]; !keep {
			scripts[name] = binding.Original
		}
		if _, keep := retained[binding.Script]; !keep {
			delete(scripts, binding.Script)
		}
		if binding.PreScript != "" {
			if _, keep := retained["pre"+name]; !keep {
				scripts["pre"+name] = binding.PreOriginal
			}
			if _, keep := retained[binding.PreScript]; !keep {
				delete(scripts, binding.PreScript)
			}
		}
		if binding.PostScript != "" {
			if _, keep := retained["post"+name]; !keep {
				scripts["post"+name] = binding.PostOriginal
			}
			if _, keep := retained[binding.PostScript]; !keep {
				delete(scripts, binding.PostScript)
			}
		}
	}
	for name, binding := range bindings {
		scripts[binding.Script] = binding.Original
		if binding.PreScript != "" {
			delete(scripts, "pre"+name)
			scripts[binding.PreScript] = binding.PreOriginal
		}
		if binding.PostScript != "" {
			delete(scripts, "post"+name)
			scripts[binding.PostScript] = binding.PostOriginal
		}
		scripts[name] = binding.Wrapper
	}
	scriptData, err := json.Marshal(scripts)
	if err != nil {
		return nil, err
	}
	manifest["scripts"] = scriptData
	updated, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(updated, '\n'), nil
}

func candidatePackageScripts(directory string) []string {
	data, err := os.ReadFile(filepath.Join(directory, "package.json"))
	if err != nil {
		return nil
	}
	var manifest struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(data, &manifest) != nil {
		return nil
	}
	candidates := make([]string, 0, len(manifest.Scripts))
	if _, exists := manifest.Scripts["dev"]; exists {
		candidates = append(candidates, "dev")
	}
	for name := range manifest.Scripts {
		if name != "dev" && !strings.HasPrefix(name, "inject:original:") {
			candidates = append(candidates, name)
		}
	}
	if len(candidates) > 1 {
		sort.Strings(candidates[1:])
	}
	return candidates
}

func defaultPackageScript(candidates []string) string {
	if len(candidates) > 0 && candidates[0] == "dev" {
		return "dev"
	}
	return ""
}

func runValidation(request Request, localSecrets map[string]string) error {
	if request.RunValidation != nil {
		return request.RunValidation(request.Validate)
	}
	if request.Local {
		return executor.RunCommand(request.Validate, localSecrets)
	}
	if effectiveProvider(request) == "bitwarden" {
		provider, err := vaults.NewBitwardenProvider()
		if err != nil {
			return err
		}
		secrets, err := provider.Fetch(requestContext(), vaults.BitwardenReference{ItemID: request.ItemID, Item: request.Item})
		if err != nil {
			return err
		}
		return executor.RunCommand(request.Validate, secrets)
	}
	provider, err := vaults.NewOnePasswordProvider()
	if err != nil {
		return err
	}
	secrets, err := provider.Fetch(requestContext(), vaults.OnePasswordReference{Account: request.Account, Vault: request.Vault, ItemID: request.ItemID, Item: request.Item})
	if err != nil {
		return err
	}
	return executor.RunCommand(request.Validate, secrets)
}

func requestContext() context.Context { return context.Background() }

func validateValidationCommand(command []string) error {
	if len(command) == 0 {
		return fmt.Errorf("setup: a finite validation command is required")
	}
	if containsLongRunningMarker(command) {
		return fmt.Errorf("setup: validation command must be finite")
	}
	return nil
}

func containsLongRunningMarker(command []string) bool {
	for _, argument := range command {
		switch strings.ToLower(argument) {
		case "dev", "serve", "server", "start", "watch":
			return true
		}
	}
	return false
}

func statFile(path string) (os.FileInfo, bool) {
	info, err := os.Stat(path)
	return info, err == nil && !info.IsDir()
}
