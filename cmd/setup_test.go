package cmd

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	setupworkflow "github.com/harsh-sonkar/env-pull/internal/setup"
)

func TestRunSetupUsesInteractiveFlowOnlyWithTerminalInputAndOutput(t *testing.T) {
	for _, test := range []struct {
		name            string
		stdinTerminal   bool
		stdoutTerminal  bool
		wantInteractive bool
	}{
		{name: "both terminals", stdinTerminal: true, stdoutTerminal: true, wantInteractive: true},
		{name: "redirected input", stdoutTerminal: true},
		{name: "redirected output", stdinTerminal: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			interactiveCalls := 0
			workflowCalls := 0
			err := runSetup(setupworkflow.Request{}, test.stdinTerminal, test.stdoutTerminal,
				func(setupworkflow.Request) error { interactiveCalls++; return nil },
				func(request setupworkflow.Request) error {
					workflowCalls++
					if !request.NonInteractive {
						t.Error("deterministic workflow request must be non-interactive")
					}
					return nil
				})
			if err != nil {
				t.Fatalf("runSetup() error = %v", err)
			}
			if got := interactiveCalls == 1; got != test.wantInteractive {
				t.Errorf("interactive called = %v, want %v", got, test.wantInteractive)
			}
			if got := workflowCalls == 1; got == test.wantInteractive {
				t.Errorf("workflow called = %v, want %v", got, !test.wantInteractive)
			}
		})
	}
}

func TestRunSetupCancellationDoesNotApplyWorkflow(t *testing.T) {
	cancelled := errors.New("cancelled")
	workflowCalled := false
	err := runSetup(setupworkflow.Request{}, true, true,
		func(setupworkflow.Request) error { return cancelled },
		func(setupworkflow.Request) error { workflowCalled = true; return nil })

	if !errors.Is(err, cancelled) {
		t.Fatalf("runSetup() error = %v, want cancellation", err)
	}
	if workflowCalled {
		t.Error("workflow ran after interactive cancellation")
	}
}

func TestRunGuidedSetupAppliesCollectedChoices(t *testing.T) {
	discovery := setupworkflow.Discovery{ProjectID: "billing-api"}
	want := setupworkflow.Request{ProjectID: "chosen-id", Local: true, Confirm: true}
	var applied setupworkflow.Request

	err := runGuidedSetup(setupworkflow.Request{},
		func(string) (setupworkflow.Discovery, error) { return discovery, nil },
		func(got setupworkflow.Discovery, _ setupworkflow.Request) (setupworkflow.Request, error) {
			if !reflect.DeepEqual(got, discovery) {
				t.Errorf("discovery = %#v, want %#v", got, discovery)
			}
			return want, nil
		},
		func(request setupworkflow.Request) error { applied = request; return nil })
	if err != nil {
		t.Fatalf("runGuidedSetup() error = %v", err)
	}
	if !reflect.DeepEqual(applied, want) {
		t.Errorf("applied request = %#v, want %#v", applied, want)
	}
}

func TestRunGuidedSetupDoesNotApplyCancelledChoices(t *testing.T) {
	workflowCalled := false
	err := runGuidedSetup(setupworkflow.Request{},
		func(string) (setupworkflow.Discovery, error) { return setupworkflow.Discovery{}, nil },
		func(setupworkflow.Discovery, setupworkflow.Request) (setupworkflow.Request, error) {
			return setupworkflow.Request{}, errSetupCancelled
		},
		func(setupworkflow.Request) error { workflowCalled = true; return nil })

	if err != nil {
		t.Fatalf("runGuidedSetup() error = %v, want clean cancellation", err)
	}
	if workflowCalled {
		t.Error("workflow ran after guided setup cancellation")
	}
}

func TestPromptSetupDrivesDetectedChoicesWithoutTerminal(t *testing.T) {
	discovery := setupworkflow.Discovery{
		ProjectID:            "billing-api",
		DefaultSource:        "local",
		PlaintextInputs:      []setupworkflow.PlaintextInput{{Name: ".env", Profile: "default", Selected: true}, {Name: ".env.staging", Profile: "staging"}},
		SelectedInputs:       []string{".env"},
		DeveloperCommands:    []setupworkflow.DeveloperCommand{{Name: "dev", Default: true}, {Name: "release"}, {Name: "test"}},
		SelectedCommands:     []string{"dev"},
		ValidationCandidates: []string{"test"},
		PackageManager:       "npm",
	}
	var output bytes.Buffer
	input := &responseReader{responses: []string{"billing-api", "1", "unused-account", "unused-vault", "unused-id", "", "unused-id", "", "0", "0", "2", "unused", "n", "y", "2"}}

	request, err := promptSetupWithIO(discovery, setupworkflow.Request{Output: &output}, input, &output, true)
	if err != nil {
		t.Fatalf("promptSetupWithIO() error = %v; output = %q", err, output.String())
	}
	if request.ProjectID != "billing-api" || !request.Local || !request.Confirm || request.RemoveLegacyEnv {
		t.Errorf("request = %#v, want confirmed local defaults without cleanup", request)
	}
	if !reflect.DeepEqual(request.SelectedInputs, []string{".env"}) || !reflect.DeepEqual(request.PackageScripts, []string{"dev"}) {
		t.Errorf("request selections = inputs %q, commands %q", request.SelectedInputs, request.PackageScripts)
	}
	if !reflect.DeepEqual(request.Validate, []string{"npm", "run", "test"}) {
		t.Errorf("validation = %q, want npm run test", request.Validate)
	}
	resolution, err := request.ResolveConflict(setupworkflow.Conflict{Script: "dev"})
	if err != nil {
		t.Fatalf("ResolveConflict() error = %v", err)
	}
	if resolution != setupworkflow.ReplaceConflict {
		t.Errorf("resolution = %v, want explicit replacement", resolution)
	}
	for _, message := range []string{"Project ID", "Secret source", "Environment inputs and profiles", "Developer commands", "Finite validation command", "Review setup", "Applying setup..."} {
		if !strings.Contains(output.String(), message) {
			t.Errorf("output missing %q: %q", message, output.String())
		}
	}
}

type responseReader struct {
	responses []string
}

func (reader *responseReader) Read(data []byte) (int, error) {
	if len(reader.responses) == 0 {
		return 0, io.EOF
	}
	response := reader.responses[0] + "\n"
	reader.responses = reader.responses[1:]
	return copy(data, response), nil
}
