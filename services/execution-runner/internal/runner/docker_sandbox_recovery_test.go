package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeLifecycleDiscoverer struct {
	containers []DiscoveredDockerContainer
	err error
	calls int
	runnerID string
	lifecycleID string
}

func (fake *fakeLifecycleDiscoverer) DiscoverByLifecycleLabels(
	_ context.Context,
	runnerID string,
	lifecycleID string,
) ([]DiscoveredDockerContainer, error) {
	fake.calls++
	fake.runnerID = runnerID
	fake.lifecycleID = lifecycleID
	return append([]DiscoveredDockerContainer(nil), fake.containers...), fake.err
}

func TestDiscoverPendingDockerLifecyclesIsDryRunAndCorrelatesExactIdentity(t *testing.T) {
	sandbox, err := newJournaledTestSandbox(t, &fakeDockerCLI{})
	if err != nil {
		t.Fatal(err)
	}
	runnerID := strings.Repeat("a", 32)
	lifecycleID, err := sandbox.journal.Begin(runnerID, "exec_recovery_test")
	if err != nil {
		t.Fatal(err)
	}
	containerID := strings.Repeat("b", 64)
	if err := sandbox.journal.Created(lifecycleID, containerID); err != nil {
		t.Fatal(err)
	}
	discoverer := &fakeLifecycleDiscoverer{containers: []DiscoveredDockerContainer{{
		ID: containerID, RunnerInstanceID: runnerID, LifecycleID: lifecycleID, State: "exited",
	}}}

	results, err := DiscoverPendingDockerLifecycles(context.Background(), sandbox.journal, discoverer, runnerID)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Assessment != "PENDING_CONTAINER_MATCH — operator review required" {
		t.Fatalf("discovery result = %+v", results)
	}
	if discoverer.calls != 1 || discoverer.runnerID != runnerID || discoverer.lifecycleID != lifecycleID {
		t.Fatalf("discovery did not use exact journal identity: %+v", discoverer)
	}
	pending, err := sandbox.journal.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Cleaned {
		t.Fatalf("dry-run discovery mutated journal state: %+v", pending)
	}
}

func TestDiscoverPendingDockerLifecyclesFailsClosedOnRunnerMismatch(t *testing.T) {
	sandbox, err := newJournaledTestSandbox(t, &fakeDockerCLI{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sandbox.journal.Begin(strings.Repeat("a", 32), "exec_recovery_test"); err != nil {
		t.Fatal(err)
	}
	discoverer := &fakeLifecycleDiscoverer{}
	_, err = DiscoverPendingDockerLifecycles(context.Background(), sandbox.journal, discoverer, strings.Repeat("c", 32))
	if err == nil {
		t.Fatal("runner identity mismatch was accepted")
	}
	if discoverer.calls != 0 {
		t.Fatal("Docker discovery ran after runner identity mismatch")
	}
}

func TestDiscoverPendingDockerLifecyclesReportsCreateIntentWithoutDeletion(t *testing.T) {
	sandbox, err := newJournaledTestSandbox(t, &fakeDockerCLI{})
	if err != nil {
		t.Fatal(err)
	}
	runnerID := strings.Repeat("a", 32)
	if _, err := sandbox.journal.Begin(runnerID, "exec_create_gap"); err != nil {
		t.Fatal(err)
	}
	discoverer := &fakeLifecycleDiscoverer{}
	results, err := DiscoverPendingDockerLifecycles(context.Background(), sandbox.journal, discoverer, runnerID)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Assessment != "NO_CONTAINER_MATCH — operator review required" {
		t.Fatalf("create-intent discovery result = %+v", results)
	}
}

func TestOSCommandDockerCLIDiscoveryUsesExactLabelsAndVerifiesInspect(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Docker lifecycle discovery targets Linux hosts")
	}
	binary := filepath.Join(t.TempDir(), "docker-stub")
	id := strings.Repeat("a", 64)
	runnerID := strings.Repeat("b", 32)
	lifecycleID := strings.Repeat("c", 32)
	script := strings.Join([]string{
		"#!/bin/sh",
		"if [ \"$1\" = \"ps\" ]; then",
		"  [ \"$2\" = \"--all\" ] || exit 81",
		"  [ \"$3\" = \"--no-trunc\" ] || exit 82",
		"  [ \"$4\" = \"--filter\" ] || exit 83",
		"  [ \"$5\" = \"label=com.skyhigh.runner-instance=" + runnerID + "\" ] || exit 84",
		"  [ \"$6\" = \"--filter\" ] || exit 85",
		"  [ \"$7\" = \"label=com.skyhigh.lifecycle-id=" + lifecycleID + "\" ] || exit 86",
		"  printf '%s\\n' \"" + id + "\"",
		"  exit 0",
		"fi",
		"if [ \"$1\" = \"inspect\" ]; then",
		"  [ \"$4\" = \"" + id + "\" ] || exit 87",
		"  printf '%s' '{\"Id\":\"" + id + "\",\"Config\":{\"Labels\":{\"com.skyhigh.runner-instance\":\"" + runnerID + "\",\"com.skyhigh.lifecycle-id\":\"" + lifecycleID + "\"}},\"State\":{\"Status\":\"exited\"}}'",
		"  exit 0",
		"fi",
		"exit 88",
	}, "\n")
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	found, err := (OSCommandDockerCLI{Binary: binary}).DiscoverByLifecycleLabels(context.Background(), runnerID, lifecycleID)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].ID != id || found[0].State != "exited" {
		t.Fatalf("discovered containers = %+v", found)
	}
}

func TestOSCommandDockerCLIDiscoveryRejectsInspectLabelMismatch(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Docker lifecycle discovery targets Linux hosts")
	}
	binary := filepath.Join(t.TempDir(), "docker-stub")
	id := strings.Repeat("a", 64)
	runnerID := strings.Repeat("b", 32)
	lifecycleID := strings.Repeat("c", 32)
	script := strings.Join([]string{
		"#!/bin/sh",
		"if [ \"$1\" = \"ps\" ]; then printf '%s\\n' \"" + id + "\"; exit 0; fi",
		"if [ \"$1\" = \"inspect\" ]; then printf '%s' '{\"Id\":\"" + id + "\",\"Config\":{\"Labels\":{\"com.skyhigh.runner-instance\":\"" + runnerID + "\",\"com.skyhigh.lifecycle-id\":\"wrong\"}},\"State\":{\"Status\":\"exited\"}}'; exit 0; fi",
		"exit 89",
	}, "\n")
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := (OSCommandDockerCLI{Binary: binary}).DiscoverByLifecycleLabels(context.Background(), runnerID, lifecycleID)
	if !errors.Is(err, ErrDockerCommandFailed) {
		t.Fatalf("mismatched inspect labels error = %v, want ErrDockerCommandFailed", err)
	}
}
