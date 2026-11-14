package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/sivasreeonline/orbit-engine/services/execution-runner/internal/sandboxjournal"
)

const (
	runnerInstanceLabel = "com.skyhigh.runner-instance"
	lifecycleIDLabel    = "com.skyhigh.lifecycle-id"
)

// DiscoveredDockerContainer contains only identity and state metadata. It never
// includes student source, stdin, stdout, stderr or hidden-test material.
type DiscoveredDockerContainer struct {
	ID              string
	RunnerInstanceID string
	LifecycleID     string
	State           string
}

// DockerLifecycleDiscoverer must query by both exact Sky High ownership labels.
type DockerLifecycleDiscoverer interface {
	DiscoverByLifecycleLabels(context.Context, string, string) ([]DiscoveredDockerContainer, error)
}

// LifecycleDiscovery is a dry-run result. No method in this file removes or
// changes a Docker container.
type LifecycleDiscovery struct {
	Lifecycle  sandboxjournal.Lifecycle
	Containers []DiscoveredDockerContainer
	Assessment string
}

// DiscoverPendingDockerLifecycles correlates pending journal entries with
// containers carrying both exact ownership labels. The caller must first open
// the journal successfully; its exclusive journal lock prevents concurrent
// journal owners. CREATE_INTENT records with no container ID are still reported
// for operator review, never treated as authorization to delete.
func DiscoverPendingDockerLifecycles(
	ctx context.Context,
	journal *sandboxjournal.Journal,
	discoverer DockerLifecycleDiscoverer,
	runnerID string,
) ([]LifecycleDiscovery, error) {
	if journal == nil || discoverer == nil || !runnerInstanceIDPattern.MatchString(runnerID) {
		return nil, ErrSandboxNotConfigured
	}
	pending, err := journal.Pending()
	if err != nil {
		return nil, fmt.Errorf("read pending sandbox lifecycles: %w", err)
	}
	results := make([]LifecycleDiscovery, 0, len(pending))
	for _, lifecycle := range pending {
		if lifecycle.RunnerInstanceID != runnerID {
			return nil, errors.New("pending lifecycle belongs to a different runner instance")
		}
		containers, err := discoverer.DiscoverByLifecycleLabels(ctx, runnerID, lifecycle.LifecycleID)
		if err != nil {
			return nil, fmt.Errorf("discover lifecycle %s: %w", lifecycle.LifecycleID, err)
		}
		result := LifecycleDiscovery{Lifecycle: lifecycle, Containers: containers}
		switch {
		case len(containers) == 0:
			result.Assessment = "NO_CONTAINER_MATCH — operator review required"
		case len(containers) > 1:
			result.Assessment = "AMBIGUOUS_MULTIPLE_MATCHES — operator review required"
		case lifecycle.ContainerID != "" && containers[0].ID != lifecycle.ContainerID:
			result.Assessment = "CONTAINER_ID_MISMATCH — operator review required"
		case lifecycle.ContainerID == "":
			result.Assessment = "CREATE_INTENT_MATCH — crash-window candidate; operator review required"
		default:
			result.Assessment = "PENDING_CONTAINER_MATCH — operator review required"
		}
		results = append(results, result)
	}
	return results, nil
}

// DiscoverByLifecycleLabels performs read-only Docker discovery. It first
// filters on both exact labels, then independently inspects every returned
// full container ID and verifies both labels before reporting a match.
func (cli OSCommandDockerCLI) DiscoverByLifecycleLabels(
	ctx context.Context,
	runnerID string,
	lifecycleID string,
) ([]DiscoveredDockerContainer, error) {
	if !runnerInstanceIDPattern.MatchString(runnerID) || !runnerInstanceIDPattern.MatchString(lifecycleID) {
		return nil, ErrDockerCommandFailed
	}
	command := exec.CommandContext(
		ctx,
		cli.binary(),
		"ps", "--all", "--no-trunc",
		"--filter", "label="+runnerInstanceLabel+"="+runnerID,
		"--filter", "label="+lifecycleIDLabel+"="+lifecycleID,
		"--format", "{{.ID}}",
	)
	output, err := command.Output()
	if err != nil {
		return nil, ErrDockerCommandFailed
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return []DiscoveredDockerContainer{}, nil
	}
	seen := make(map[string]struct{}, len(lines))
	containers := make([]DiscoveredDockerContainer, 0, len(lines))
	for _, id := range lines {
		id = strings.TrimSpace(id)
		if len(id) != 64 || !containerIDPattern.MatchString(id) {
			return nil, ErrDockerCommandFailed
		}
		if _, exists := seen[id]; exists {
			return nil, ErrDockerCommandFailed
		}
		seen[id] = struct{}{}
		inspect := exec.CommandContext(ctx, cli.binary(), "inspect", "--format", "{{json .}}", id)
		inspected, err := inspect.Output()
		if err != nil {
			return nil, ErrDockerCommandFailed
		}
		var item struct {
			ID string `json:"Id"`
			Config struct {
				Labels map[string]string `json:"Labels"`
			} `json:"Config"`
			State struct {
				Status string `json:"Status"`
			} `json:"State"`
		}
		if err := json.Unmarshal(inspected, &item); err != nil {
			return nil, ErrDockerCommandFailed
		}
		if item.ID != id ||
			item.Config.Labels[runnerInstanceLabel] != runnerID ||
			item.Config.Labels[lifecycleIDLabel] != lifecycleID ||
			strings.TrimSpace(item.State.Status) == "" {
			return nil, ErrDockerCommandFailed
		}
		containers = append(containers, DiscoveredDockerContainer{
			ID: id, RunnerInstanceID: runnerID, LifecycleID: lifecycleID, State: item.State.Status,
		})
	}
	return containers, nil
}
