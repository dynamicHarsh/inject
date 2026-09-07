package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCLIIdentity(t *testing.T) {
	binaryPath := filepath.Join(t.TempDir(), "inject")
	build := exec.Command("go", "build", "-o", binaryPath, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}

	t.Run("inject is the primary command", func(t *testing.T) {
		output, err := exec.Command(binaryPath, "--help").CombinedOutput()
		if err != nil {
			t.Fatalf("run inject help: %v\n%s", err, output)
		}
		if !strings.Contains(string(output), "Usage:\n  inject [command]") {
			t.Fatalf("inject help = %q, want primary product name", output)
		}
	})

	t.Run("env-pull remains a legacy alias", func(t *testing.T) {
		legacyPath := filepath.Join(t.TempDir(), "env-pull")
		if err := os.Symlink(binaryPath, legacyPath); err != nil {
			t.Fatalf("create legacy alias: %v", err)
		}

		output, err := exec.Command(legacyPath, "--help").CombinedOutput()
		if err != nil {
			t.Fatalf("run env-pull help: %v\n%s", err, output)
		}
		if !strings.Contains(string(output), "env-pull is deprecated; use inject instead") {
			t.Fatalf("legacy invocation = %q, want migration notice", output)
		}

	})
}

func TestConfiguredRun(t *testing.T) {
	binaryPath := filepath.Join(t.TempDir(), "inject")
	build := exec.Command("go", "build", "-o", binaryPath, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}

	projectDir := t.TempDir()
	config := `format_version = 1
project_id = "test-project"

[profiles.default]
provider = "1password"
account = "acme"
vault = "Engineering"
item_id = "stable-note-id"

[commands.show-token]
profile = "default"
command = ["sh", "-c", "printf %s \"$TOKEN\""]
`
	if err := os.WriteFile(filepath.Join(projectDir, "inject.toml"), []byte(config), 0o600); err != nil {
		t.Fatalf("write inject.toml: %v", err)
	}
	opPath := filepath.Join(projectDir, "op")
	op := "#!/bin/sh\nprintf '%s\\n' '{\"fields\":[{\"id\":\"notesPlain\",\"value\":\"TOKEN=from-configured-note\\n\"}]}'\n"
	if err := os.WriteFile(opPath, []byte(op), 0o700); err != nil {
		t.Fatalf("write fake op: %v", err)
	}

	command := exec.Command(binaryPath, "run", "--", "sh", "-c", `printf %s "$TOKEN"`)
	command.Dir = projectDir
	command.Env = append(os.Environ(), "PATH="+projectDir+string(os.PathListSeparator)+os.Getenv("PATH"), "TOKEN=parent-value")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run configured command: %v\n%s", err, output)
	}
	if got := string(output); got != "from-configured-note" {
		t.Errorf("injected output = %q, want configured secret", got)
	}

	binding := exec.Command(binaryPath, "show-token")
	binding.Dir = projectDir
	binding.Env = append(os.Environ(), "PATH="+projectDir+string(os.PathListSeparator)+os.Getenv("PATH"), "TOKEN=parent-value")
	output, err = binding.CombinedOutput()
	if err != nil {
		t.Fatalf("run configured binding: %v\n%s", err, output)
	}
	if got := string(output); got != "from-configured-note" {
		t.Errorf("bound command output = %q, want configured secret", got)
	}
}

func TestConfiguredBitwardenRun(t *testing.T) {
	binaryPath := filepath.Join(t.TempDir(), "inject")
	build := exec.Command("go", "build", "-o", binaryPath, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}

	projectDir := t.TempDir()
	config := `format_version = 1
project_id = "test-project"

[profiles.default]
provider = "bitwarden"
item_id = "stable-note-id"

[commands.show-token]
profile = "default"
command = ["sh", "-c", "printf %s \"$TOKEN\""]
`
	if err := os.WriteFile(filepath.Join(projectDir, "inject.toml"), []byte(config), 0o600); err != nil {
		t.Fatalf("write inject.toml: %v", err)
	}
	bw := "#!/bin/sh\nif [ \"$BW_SESSION\" != \"ci-session\" ]; then exit 1; fi\nprintf 'TOKEN=from-bitwarden-note\\n'\n"
	if err := os.WriteFile(filepath.Join(projectDir, "bw"), []byte(bw), 0o700); err != nil {
		t.Fatalf("write fake bw: %v", err)
	}

	for _, args := range [][]string{
		{"run", "--", "sh", "-c", `printf %s "$TOKEN"`},
		{"show-token"},
	} {
		command := exec.Command(binaryPath, args...)
		command.Dir = projectDir
		command.Env = append(os.Environ(), "PATH="+projectDir+string(os.PathListSeparator)+os.Getenv("PATH"), "BW_SESSION=ci-session", "TOKEN=parent-value")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("run %q: %v\n%s", args, err, output)
		}
		if got := string(output); got != "from-bitwarden-note" {
			t.Errorf("run %q output = %q, want injected Bitwarden secret", args, got)
		}
	}
}

func TestConfiguredRunDoesNotLaunchChildWhenSourceFails(t *testing.T) {
	binaryPath := filepath.Join(t.TempDir(), "inject")
	build := exec.Command("go", "build", "-o", binaryPath, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}

	projectDir := t.TempDir()
	config := `format_version = 1
project_id = "test-project"
[profiles.default]
provider = "1password"
account = "acme"
vault = "Engineering"
item_id = "stable-note-id"
`
	if err := os.WriteFile(filepath.Join(projectDir, "inject.toml"), []byte(config), 0o600); err != nil {
		t.Fatalf("write inject.toml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "op"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatalf("write failing fake op: %v", err)
	}
	sentinel := filepath.Join(projectDir, "child-was-launched")

	command := exec.Command(binaryPath, "run", "--", "sh", "-c", "touch \"$1\"", "sh", sentinel)
	command.Dir = projectDir
	command.Env = append(os.Environ(), "PATH="+projectDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("run configured command = success, want source failure; output: %s", output)
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Errorf("child launch sentinel stat error = %v, want not exist", err)
	}
}

func TestFreshCloneRunsCommittedPackageScriptWithoutSetup(t *testing.T) {
	npmPath, err := exec.LookPath("npm")
	if err != nil {
		t.Skip("npm is not installed")
	}
	buildDirectory := t.TempDir()
	binaryPath := filepath.Join(buildDirectory, "inject")
	build := exec.Command("go", "build", "-o", binaryPath, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}

	projectDir := t.TempDir()
	config := `format_version = 1
project_id = "test-project"

[profiles.default]
provider = "1password"
account = "acme"
vault = "Engineering"
item_id = "stable-note-id"

[script_bindings.dev]
profile = "default"
package_manager = "npm"
wrapper = "inject __run-package-script \"dev\""
script = "inject:original:dev"
original = "printf 'main:%s\\n' \"$TOKEN\" >> \"$ORDER_FILE\""
pre_script = "inject:original:predev"
pre_original = "printf 'pre:%s\\n' \"$TOKEN\" >> \"$ORDER_FILE\""
post_script = "inject:original:postdev"
post_original = "printf 'post:%s\\n' \"$TOKEN\" >> \"$ORDER_FILE\""
`
	manifest := `{"scripts":{"dev":"inject __run-package-script \"dev\"","inject:original:predev":"sh lifecycle.sh pre","inject:original:dev":"sh lifecycle.sh main","inject:original:postdev":"sh lifecycle.sh post"}}`
	if err := os.WriteFile(filepath.Join(projectDir, "inject.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "package.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	lifecycle := `#!/bin/sh
stage=$1
shift
printf '%s:%s:%s:%s\n' "$stage" "$TOKEN" "$PWD" "$#" >> "$ORDER_FILE"
for argument in "$@"; do
  printf 'arg:%s\n' "$argument" >> "$ORDER_FILE"
done
`
	if err := os.WriteFile(filepath.Join(projectDir, "lifecycle.sh"), []byte(lifecycle), 0o700); err != nil {
		t.Fatal(err)
	}
	opPath := filepath.Join(projectDir, "op")
	if err := os.WriteFile(opPath, []byte("#!/bin/sh\nprintf '%s\\n' '{\"fields\":[{\"id\":\"notesPlain\",\"value\":\"TOKEN=injected-value\\n\"}]}'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	orderPath := filepath.Join(projectDir, "order.log")
	nestedDir := filepath.Join(projectDir, "nested", "directory")
	if err := os.MkdirAll(nestedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("npm", "run", "--silent", "dev", "--", "first value", "--flag")
	command.Dir = nestedDir
	command.Env = append(os.Environ(),
		"PATH="+projectDir+string(os.PathListSeparator)+buildDirectory+string(os.PathListSeparator)+os.Getenv("PATH"),
		"ORDER_FILE="+orderPath,
		"TOKEN=parent-value",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("npm run dev: %v\n%s", err, output)
	}
	order, err := os.ReadFile(orderPath)
	if err != nil {
		t.Fatal(err)
	}
	canonicalProjectDir, err := filepath.EvalSymlinks(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	want := "pre:injected-value:" + canonicalProjectDir + ":0\n" +
		"main:injected-value:" + canonicalProjectDir + ":2\n" +
		"arg:first value\narg:--flag\n" +
		"post:injected-value:" + canonicalProjectDir + ":0\n"
	if got := string(order); got != want {
		t.Errorf("lifecycle output = %q, want %q", got, want)
	}
	if got := os.Getenv("TOKEN"); got == "injected-value" {
		t.Errorf("parent TOKEN = %q, must remain unchanged", got)
	}

	if err := os.Remove(orderPath); err != nil {
		t.Fatal(err)
	}
	exitManifest := `{"scripts":{"dev":"inject __run-package-script \"dev\"","inject:original:predev":"exit 0","inject:original:dev":"exit 7","inject:original:postdev":"exit 0"}}`
	if err := os.WriteFile(filepath.Join(projectDir, "package.json"), []byte(exitManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	exitCommand := exec.Command("npm", "run", "--silent", "dev")
	exitCommand.Dir = projectDir
	exitCommand.Env = command.Env
	if output, err := exitCommand.CombinedOutput(); err == nil {
		t.Fatalf("npm run dev = success, want exit status 7; output: %s", output)
	} else if exitError, ok := err.(*exec.ExitError); !ok || exitError.ExitCode() != 7 {
		t.Fatalf("npm run dev error = %v, want exit status 7; output: %s", err, output)
	}

	if err := os.WriteFile(opPath, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	failed := exec.Command("npm", "run", "--silent", "dev")
	failed.Dir = projectDir
	failed.Env = command.Env
	if output, err := failed.CombinedOutput(); err == nil {
		t.Fatalf("npm run dev = success with unavailable source; output: %s", output)
	}
	if _, err := os.Stat(orderPath); !os.IsNotExist(err) {
		t.Errorf("lifecycle output stat error = %v, want no process launched", err)
	}

	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	isolatedPath := t.TempDir()
	if err := os.Symlink(nodePath, filepath.Join(isolatedPath, "node")); err != nil {
		t.Fatal(err)
	}
	missingInject := exec.Command(npmPath, "run", "--silent", "dev")
	missingInject.Dir = nestedDir
	missingInject.Env = append(os.Environ(),
		"PATH="+isolatedPath+string(os.PathListSeparator)+"/usr/bin:/bin",
		"ORDER_FILE="+orderPath,
	)
	if output, err := missingInject.CombinedOutput(); err == nil {
		t.Fatalf("npm run dev = success without inject on PATH; output: %s", output)
	}
	if _, err := os.Stat(orderPath); !os.IsNotExist(err) {
		t.Errorf("lifecycle output stat error = %v, want no process launched without inject", err)
	}
}

func TestSetupPreservesDeveloperWorkflowWithoutPersistingSecrets(t *testing.T) {
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm is not installed")
	}
	toolsDirectory := t.TempDir()
	binaryPath := filepath.Join(toolsDirectory, "inject")
	build := exec.Command("go", "build", "-o", binaryPath, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}

	projectDir := t.TempDir()
	originalManifest := []byte(`{"name":"acceptance-project","scripts":{"predev":"node lifecycle.js pre","dev":"node lifecycle.js main","postdev":"node lifecycle.js post"}}`)
	if err := os.WriteFile(filepath.Join(projectDir, "package.json"), originalManifest, 0o600); err != nil {
		t.Fatal(err)
	}
	originalLockfile := []byte(`{"lockfileVersion":3}`)
	if err := os.WriteFile(filepath.Join(projectDir, "package-lock.json"), originalLockfile, 0o600); err != nil {
		t.Fatal(err)
	}
	lifecycle := `require("fs").appendFileSync(process.env.ORDER_FILE, process.argv[2] + ":" + process.env.TOKEN + ":" + process.env.SECOND + "\n")`
	if err := os.WriteFile(filepath.Join(projectDir, "lifecycle.js"), []byte(lifecycle), 0o600); err != nil {
		t.Fatal(err)
	}
	unrelatedPath := filepath.Join(projectDir, "unrelated.txt")
	if err := os.WriteFile(unrelatedPath, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}

	orderPath := filepath.Join(projectDir, "order.log")
	t.Setenv("TOKEN", "parent-token")
	t.Setenv("SECOND", "parent-second")
	developerCommand := func() *exec.Cmd {
		command := exec.Command("npm", "run", "--silent", "dev")
		command.Dir = projectDir
		command.Env = append(os.Environ(),
			"PATH="+toolsDirectory+string(os.PathListSeparator)+os.Getenv("PATH"),
			"ORDER_FILE="+orderPath,
		)
		return command
	}
	if output, err := developerCommand().CombinedOutput(); err != nil {
		t.Fatalf("npm run dev before setup: %v\n%s", err, output)
	}
	if got, err := os.ReadFile(orderPath); err != nil || string(got) != "pre:parent-token:parent-second\nmain:parent-token:parent-second\npost:parent-token:parent-second\n" {
		t.Fatalf("lifecycle before setup = %q, %v", got, err)
	}
	if err := os.Remove(orderPath); err != nil {
		t.Fatal(err)
	}

	secret := "acceptance-secret-value"
	secondSecret := "complete-set"
	opPath := filepath.Join(toolsDirectory, "op")
	op := "#!/bin/sh\nif [ \"$1\" = account ]; then exit 0; fi\nprintf '%s\\n' '{\"fields\":[{\"id\":\"notesPlain\",\"value\":\"TOKEN=" + secret + "\\nSECOND=" + secondSecret + "\\n\"}]}'\n"
	if err := os.WriteFile(opPath, []byte(op), 0o700); err != nil {
		t.Fatal(err)
	}
	setup := exec.Command(binaryPath, "setup",
		"--project-id", "acceptance-project",
		"--account", "acme", "--vault", "Engineering", "--item-id", "stable-note-id",
		"--package-script", "dev", "--yes",
		"--validate=sh", "--validate=-c", `--validate=test -n "$TOKEN" && test -n "$SECOND"`,
	)
	setup.Dir = projectDir
	setup.Env = append(os.Environ(), "PATH="+toolsDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))
	setupOutput, err := setup.CombinedOutput()
	if err != nil {
		t.Fatalf("inject setup: %v\n%s", err, setupOutput)
	}
	for _, value := range []string{secret, secondSecret} {
		if strings.Contains(string(setupOutput), value) {
			t.Errorf("setup output exposed secret value: %s", setupOutput)
		}
	}
	for _, name := range []string{"inject.toml", "package.json", "package-lock.json"} {
		contents, err := os.ReadFile(filepath.Join(projectDir, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range []string{secret, secondSecret} {
			if strings.Contains(string(contents), value) {
				t.Errorf("%s exposed secret value", name)
			}
		}
	}

	if output, err := developerCommand().CombinedOutput(); err != nil {
		t.Fatalf("npm run dev after setup: %v\n%s", err, output)
	}
	if got, err := os.ReadFile(orderPath); err != nil || string(got) != "pre:"+secret+":"+secondSecret+"\nmain:"+secret+":"+secondSecret+"\npost:"+secret+":"+secondSecret+"\n" {
		t.Fatalf("injected lifecycle = %q, %v", got, err)
	}
	if got := os.Getenv("TOKEN"); got != "parent-token" {
		t.Errorf("parent TOKEN = %q, want unchanged", got)
	}
	if got := os.Getenv("SECOND"); got != "parent-second" {
		t.Errorf("parent SECOND = %q, want unchanged", got)
	}
	if err := os.Remove(orderPath); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(opPath, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	failureOutput, err := developerCommand().CombinedOutput()
	if err == nil {
		t.Fatalf("npm run dev succeeded with unavailable source: %s", failureOutput)
	}
	for _, value := range []string{secret, secondSecret} {
		if strings.Contains(string(failureOutput), value) {
			t.Errorf("source failure exposed secret value: %s", failureOutput)
		}
	}
	if _, err := os.Stat(orderPath); !os.IsNotExist(err) {
		t.Errorf("lifecycle output stat error = %v, want no process launched", err)
	}

	remove := exec.Command(binaryPath, "remove", "--yes")
	remove.Dir = projectDir
	remove.Env = setup.Env
	removeOutput, err := remove.CombinedOutput()
	if err != nil {
		t.Fatalf("inject remove: %v\n%s", err, removeOutput)
	}
	for _, value := range []string{secret, secondSecret} {
		if strings.Contains(string(removeOutput), value) {
			t.Errorf("remove output exposed secret value: %s", removeOutput)
		}
	}
	if _, err := os.Stat(filepath.Join(projectDir, "inject.toml")); !os.IsNotExist(err) {
		t.Errorf("inject.toml stat error = %v, want removed", err)
	}
	restoredManifest, err := os.ReadFile(filepath.Join(projectDir, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	var restored struct {
		Name    string            `json:"name"`
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(restoredManifest, &restored); err != nil {
		t.Fatal(err)
	}
	wantScripts := map[string]string{
		"predev":  "node lifecycle.js pre",
		"dev":     "node lifecycle.js main",
		"postdev": "node lifecycle.js post",
	}
	if len(restored.Scripts) != len(wantScripts) {
		t.Errorf("restored scripts = %#v, want %#v", restored.Scripts, wantScripts)
	}
	for name, want := range wantScripts {
		if got := restored.Scripts[name]; got != want {
			t.Errorf("restored script %q = %q, want %q", name, got, want)
		}
	}
	if restored.Name != "acceptance-project" {
		t.Errorf("restored package name = %q, want acceptance-project", restored.Name)
	}
	if got, err := os.ReadFile(filepath.Join(projectDir, "package-lock.json")); err != nil || string(got) != string(originalLockfile) {
		t.Errorf("package-lock.json = %q, %v; want unchanged", got, err)
	}
	if got, err := os.ReadFile(unrelatedPath); err != nil || string(got) != "keep me" {
		t.Errorf("unrelated project state = %q, %v; want unchanged", got, err)
	}
}

func TestConfiguredPackageScriptSupportsAllPackageManagerFamilies(t *testing.T) {
	buildDirectory := t.TempDir()
	binaryPath := filepath.Join(buildDirectory, "inject")
	build := exec.Command("go", "build", "-o", binaryPath, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}

	for _, manager := range []string{"npm", "pnpm", "yarn", "bun"} {
		t.Run(manager, func(t *testing.T) {
			projectDir := t.TempDir()
			config := `format_version = 1
project_id = "test-project"

[profiles.default]
provider = "1password"
account = "acme"
vault = "Engineering"
item_id = "stable-note-id"

[script_bindings.dev]
profile = "default"
package_manager = "` + manager + `"
wrapper = "inject __run-package-script \"dev\""
script = "inject:original:dev"
original = "preserved main script"
pre_script = "inject:original:predev"
pre_original = "preserved pre script"
post_script = "inject:original:postdev"
post_original = "preserved post script"
`
			if err := os.WriteFile(filepath.Join(projectDir, "inject.toml"), []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(projectDir, "op"), []byte("#!/bin/sh\nprintf '%s\\n' '{\"fields\":[{\"id\":\"notesPlain\",\"value\":\"TOKEN=injected-value\\n\"}]}'\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			managerPath := filepath.Join(projectDir, manager)
			managerScript := `#!/bin/sh
case "$2" in
		dev)
			shift 2
			if [ "$1" = "--" ]; then shift; fi
			cd "$PROJECT_ROOT"
			exec "$INJECT_BINARY" __run-package-script dev "$@"
			;;
	inject:original:predev) : > pre.cwd; printf 'pre:%s\n' "$TOKEN" >> "$ORDER_FILE" ;;
	inject:original:dev)
		shift 3
		: > main.cwd
		printf 'main:%s:%s\n' "$TOKEN" "$#" >> "$ORDER_FILE"
		for argument in "$@"; do printf 'arg:%s\n' "$argument" >> "$ORDER_FILE"; done
		exit "${MAIN_EXIT:-0}"
		;;
	inject:original:postdev) : > post.cwd; printf 'post:%s\n' "$TOKEN" >> "$ORDER_FILE" ;;
  *) exit 64 ;;
esac
`
			if err := os.WriteFile(managerPath, []byte(managerScript), 0o700); err != nil {
				t.Fatal(err)
			}
			orderPath := filepath.Join(projectDir, "order.log")
			nestedDir := filepath.Join(projectDir, "nested", "directory")
			if err := os.MkdirAll(nestedDir, 0o700); err != nil {
				t.Fatal(err)
			}
			environment := []string{
				"PATH=" + projectDir,
				"npm_execpath=" + managerPath,
				"npm_config_user_agent=" + manager + "/1.0.0",
				"ORDER_FILE=" + orderPath,
				"PROJECT_ROOT=" + projectDir,
				"INJECT_BINARY=" + binaryPath,
				"TOKEN=parent-value",
			}
			command := exec.Command(managerPath, "run", "dev", "--", "first value", "--flag")
			command.Dir = nestedDir
			command.Env = environment
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("run package script: %v\n%s", err, output)
			}
			order, err := os.ReadFile(orderPath)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := string(order), "pre:injected-value\nmain:injected-value:2\narg:first value\narg:--flag\npost:injected-value\n"; got != want {
				t.Errorf("lifecycle output = %q, want %q", got, want)
			}
			for _, stage := range []string{"pre", "main", "post"} {
				if _, err := os.Stat(filepath.Join(projectDir, stage+".cwd")); err != nil {
					t.Errorf("%s lifecycle did not run from project root: %v", stage, err)
				}
				if _, err := os.Stat(filepath.Join(nestedDir, stage+".cwd")); !os.IsNotExist(err) {
					t.Errorf("%s lifecycle marker found in nested invocation directory", stage)
				}
			}

			if err := os.Remove(orderPath); err != nil {
				t.Fatal(err)
			}
			exitCommand := exec.Command(managerPath, "run", "dev")
			exitCommand.Dir = nestedDir
			exitCommand.Env = append(environment, "MAIN_EXIT=7")
			if output, err := exitCommand.CombinedOutput(); err == nil {
				t.Fatalf("run package script = success, want exit status 7; output: %s", output)
			} else if exitError, ok := err.(*exec.ExitError); !ok || exitError.ExitCode() != 7 {
				t.Fatalf("run package script error = %v, want exit status 7; output: %s", err, output)
			}
		})
	}
}

func TestConfiguredPackageScriptForwardsSignals(t *testing.T) {
	buildDirectory := t.TempDir()
	binaryPath := filepath.Join(buildDirectory, "inject")
	build := exec.Command("go", "build", "-o", binaryPath, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}

	projectDir := t.TempDir()
	config := `format_version = 1
project_id = "test-project"

[profiles.default]
provider = "1password"
account = "acme"
vault = "Engineering"
item_id = "stable-note-id"

[script_bindings.dev]
profile = "default"
package_manager = "npm"
wrapper = "inject __run-package-script \"dev\""
script = "inject:original:dev"
original = "long-running process"
`
	if err := os.WriteFile(filepath.Join(projectDir, "inject.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "op"), []byte("#!/bin/sh\nprintf '%s\\n' '{\"fields\":[{\"id\":\"notesPlain\",\"value\":\"TOKEN=injected-value\\n\"}]}'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	managerPath := filepath.Join(projectDir, "npm")
	managerScript := `#!/bin/sh
if [ "$2" = "dev" ]; then
  cd "$PROJECT_ROOT"
  exec "$INJECT_BINARY" __run-package-script dev
fi
echo $$ > "$CHILD_PID_FILE"
trap 'echo interrupt > "$SIGNAL_FILE"; exit 23' INT
touch "$READY_FILE"
while :; do sleep 1; done
`
	if err := os.WriteFile(managerPath, []byte(managerScript), 0o700); err != nil {
		t.Fatal(err)
	}

	readyPath := filepath.Join(projectDir, "ready")
	signalPath := filepath.Join(projectDir, "signal")
	childPIDPath := filepath.Join(projectDir, "child.pid")
	nestedDir := filepath.Join(projectDir, "nested", "directory")
	if err := os.MkdirAll(nestedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(managerPath, "run", "dev")
	command.Dir = nestedDir
	command.Env = []string{
		"PATH=" + projectDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"npm_execpath=" + managerPath,
		"npm_config_user_agent=npm/1.0.0",
		"PROJECT_ROOT=" + projectDir,
		"INJECT_BINARY=" + binaryPath,
		"READY_FILE=" + readyPath,
		"SIGNAL_FILE=" + signalPath,
		"CHILD_PID_FILE=" + childPIDPath,
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		data, err := os.ReadFile(childPIDPath)
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err == nil {
			if process, findErr := os.FindProcess(pid); findErr == nil {
				_ = process.Kill()
			}
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(readyPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = command.Process.Kill()
			t.Fatal("retained main script did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := command.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	err := command.Wait()
	exitError, ok := err.(*exec.ExitError)
	if !ok || exitError.ExitCode() != 23 {
		t.Fatalf("inject exit error = %v, want retained child exit status 23", err)
	}
	if got, err := os.ReadFile(signalPath); err != nil || string(got) != "interrupt\n" {
		t.Fatalf("signal marker = %q, %v; want forwarded interrupt", got, err)
	}
}

func TestConfiguredBitwardenRunDoesNotLaunchChildWhenSourceFails(t *testing.T) {
	binaryPath := filepath.Join(t.TempDir(), "inject")
	build := exec.Command("go", "build", "-o", binaryPath, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}

	projectDir := t.TempDir()
	config := `format_version = 1
project_id = "test-project"
[profiles.default]
provider = "bitwarden"
item_id = "stable-note-id"
`
	if err := os.WriteFile(filepath.Join(projectDir, "inject.toml"), []byte(config), 0o600); err != nil {
		t.Fatalf("write inject.toml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "bw"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatalf("write failing fake bw: %v", err)
	}
	sentinel := filepath.Join(projectDir, "child-was-launched")

	command := exec.Command(binaryPath, "run", "--", "sh", "-c", "touch \"$1\"", "sh", sentinel)
	command.Dir = projectDir
	command.Env = append(os.Environ(), "PATH="+projectDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("run configured command = success, want source failure; output: %s", output)
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Errorf("child launch sentinel stat error = %v, want not exist", err)
	}
}

func TestSetupWritesConfirmedConfigurationAfterValidation(t *testing.T) {
	testSetupWritesConfirmedConfigurationAfterValidation(t, nil)
}

func TestSetupWritesConfirmedConfigurationWithExplicitOnePasswordProvider(t *testing.T) {
	testSetupWritesConfirmedConfigurationAfterValidation(t, []string{"--provider", "1password"})
}

func testSetupWritesConfirmedConfigurationAfterValidation(t *testing.T, providerArgs []string) {
	t.Helper()
	binaryPath := filepath.Join(t.TempDir(), "inject")
	build := exec.Command("go", "build", "-o", binaryPath, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}

	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "package.json"), []byte(`{"packageManager":"npm@10.0.0","scripts":{"dev":"vite","test":"go test ./..."}}`), 0o600); err != nil {
		t.Fatalf("write package.json: %v", err)
	}
	opPath := filepath.Join(projectDir, "op")
	op := "#!/bin/sh\nif [ \"$1\" = account ]; then exit 0; fi\nprintf '%s\\n' '{\"fields\":[{\"id\":\"notesPlain\",\"value\":\"TOKEN=from-note\\n\"}]}'\n"
	if err := os.WriteFile(opPath, []byte(op), 0o700); err != nil {
		t.Fatalf("write fake op: %v", err)
	}

	args := append([]string{"setup"}, providerArgs...)
	args = append(args,
		"--project-id", "billing-api", "--account", "acme", "--vault", "Engineering", "--item-id", "stable-note-id", "--yes",
		"--package-script", "dev,test",
		"--validate=sh", "--validate=-c", "--validate=exit 0",
	)
	command := exec.Command(binaryPath, args...)
	command.Dir = projectDir
	command.Env = append(os.Environ(), "PATH="+projectDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run setup: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "Validation succeeded") {
		t.Errorf("setup output = %q, want validation confirmation", output)
	}
	config, err := os.ReadFile(filepath.Join(projectDir, "inject.toml"))
	if err != nil {
		t.Fatalf("read inject.toml: %v", err)
	}
	if strings.Contains(string(config), "TOKEN=") || !strings.Contains(string(config), "item_id = \"stable-note-id\"") {
		t.Errorf("inject.toml = %q, want non-secret remote reference", config)
	}
	for _, binding := range []string{"[script_bindings.dev]", "[script_bindings.test]"} {
		if !strings.Contains(string(config), binding) {
			t.Errorf("inject.toml = %q, want %s", config, binding)
		}
	}
	if count := strings.Count(string(config), `profile = "default"`); count != 2 {
		t.Errorf("inject.toml has %d default-profile script bindings, want 2", count)
	}
}
