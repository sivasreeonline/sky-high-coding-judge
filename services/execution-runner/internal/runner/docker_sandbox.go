package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sivasreeonline/orbit-engine/services/execution-runner/internal/protocol"
	"github.com/sivasreeonline/orbit-engine/services/execution-runner/internal/sandboxjournal"
)

var (
	ErrDockerCommandFailed = errors.New("docker sandbox command failed")
	containerIDPattern = regexp.MustCompile(`^[a-f0-9]{12,64}$`)
	imageIDPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	runnerInstanceIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)
)

const compileSuccessMarker = "__SKYHIGH_COMPILE_OK__\n"

type DockerContainerState struct {
	Status    string `json:"Status"`
	ExitCode  int    `json:"ExitCode"`
	OOMKilled bool   `json:"OOMKilled"`
}

// DockerCLI isolates the Docker command boundary so policy and lifecycle
// behavior can be tested without running untrusted code on the host.
type DockerCLI interface {
	Preflight(context.Context, []string) error
	Create(context.Context, []string) (string, error)
	StartAttached(context.Context, string, io.Reader, io.Writer, io.Writer) (int, error)
	Inspect(context.Context, string) (DockerContainerState, error)
	Remove(context.Context, string) error
}

type OSCommandDockerCLI struct {
	Binary string
}

func (cli OSCommandDockerCLI) binary() string {
	if cli.Binary != "" {
		return cli.Binary
	}
	return "docker"
}

// Preflight verifies that the local daemon exposes runsc and that the exact
// digest-pinned Java/Python images are already present. It never pulls images.
func (cli OSCommandDockerCLI) Preflight(ctx context.Context, imageRefs []string) error {
	if len(imageRefs) != 2 {
		return ErrDockerCommandFailed
	}
	command := exec.CommandContext(ctx, cli.binary(), "info", "--format", "{{json .Runtimes}}")
	output, err := command.Output()
	if err != nil {
		return ErrDockerCommandFailed
	}
	var runtimes map[string]struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(output, &runtimes); err != nil {
		return ErrDockerCommandFailed
	}
	runsc, ok := runtimes["runsc"]
	if !ok || strings.TrimSpace(runsc.Path) == "" {
		return ErrDockerCommandFailed
	}

	seen := make(map[string]struct{}, len(imageRefs))
	for _, imageRef := range imageRefs {
		if !imageReferencePattern.MatchString(imageRef) {
			return ErrDockerCommandFailed
		}
		if _, exists := seen[imageRef]; exists {
			continue
		}
		seen[imageRef] = struct{}{}
		inspect := exec.CommandContext(ctx, cli.binary(), "image", "inspect", "--format", "{{.Id}}", imageRef)
		imageID, err := inspect.Output()
		if err != nil || !imageIDPattern.MatchString(strings.TrimSpace(string(imageID))) {
			return ErrDockerCommandFailed
		}
	}
	return nil
}

func (cli OSCommandDockerCLI) Create(ctx context.Context, args []string) (string, error) {
	command := exec.CommandContext(ctx, cli.binary(), append([]string{"create"}, args...)...)
	output, commandErr := command.Output()
	id := strings.TrimSpace(string(output))
	if !containerIDPattern.MatchString(id) {
		return "", ErrDockerCommandFailed
	}
	if commandErr != nil {
		cleanupContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = cli.Remove(cleanupContext, id)
		cancel()
		return "", ErrDockerCommandFailed
	}
	return id, nil
}

func (cli OSCommandDockerCLI) StartAttached(
	ctx context.Context,
	id string,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
) (int, error) {
	if !containerIDPattern.MatchString(id) {
		return 0, ErrDockerCommandFailed
	}
	command := exec.CommandContext(ctx, cli.binary(), "start", "--attach", "--interactive", id)
	command.Stdin = stdin
	command.Stdout = stdout
	command.Stderr = stderr
	err := command.Run()
	if err == nil {
		return 0, nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) && ctx.Err() == nil {
		return exitError.ExitCode(), nil
	}
	return 0, ErrDockerCommandFailed
}

func (cli OSCommandDockerCLI) Inspect(ctx context.Context, id string) (DockerContainerState, error) {
	if !containerIDPattern.MatchString(id) {
		return DockerContainerState{}, ErrDockerCommandFailed
	}
	command := exec.CommandContext(
		ctx,
		cli.binary(),
		"inspect",
		"--format",
		"{{json .State}}",
		id,
	)
	output, err := command.Output()
	if err != nil {
		return DockerContainerState{}, ErrDockerCommandFailed
	}
	var state DockerContainerState
	if err := json.Unmarshal(output, &state); err != nil {
		return DockerContainerState{}, ErrDockerCommandFailed
	}
	return state, nil
}

func (cli OSCommandDockerCLI) Remove(ctx context.Context, id string) error {
	if !containerIDPattern.MatchString(id) {
		return ErrDockerCommandFailed
	}
	command := exec.CommandContext(ctx, cli.binary(), "rm", "--force", id)
	if err := command.Run(); err != nil {
		return ErrDockerCommandFailed
	}
	return nil
}

type DockerSandbox struct {
	cli DockerCLI
	cleanupTimeout time.Duration
	journal *sandboxjournal.Journal
	runnerInstanceID string
}

// NewDockerSandbox configures the OCI CLI adapter. Source bytes are streamed
// into a container-owned tmpfs; no host scratch directory is required.
func NewDockerSandbox(cli DockerCLI) (*DockerSandbox, error) {
	if cli == nil {
		return nil, ErrSandboxNotConfigured
	}
	return &DockerSandbox{
		cli:            cli,
		cleanupTimeout: 5 * time.Second,
	}, nil
}


 // NewJournaledDockerSandbox creates an execution-capable sandbox. RunCase
 // refuses to execute unless every container lifecycle is durably journaled.
func NewJournaledDockerSandbox(cli DockerCLI, journal *sandboxjournal.Journal, runnerInstanceID string) (*DockerSandbox, error) {
	if cli == nil || journal == nil || !runnerInstanceIDPattern.MatchString(runnerInstanceID) {
		return nil, ErrSandboxNotConfigured
	}
	return &DockerSandbox{cli: cli, cleanupTimeout: 5 * time.Second, journal: journal, runnerInstanceID: runnerInstanceID}, nil
}

// Preflight is required before the artifact executor may report ready.
// It checks local prerequisites only and intentionally does not pull images.
func (sandbox *DockerSandbox) Preflight(
	ctx context.Context,
	runtimes RuntimeConfiguration,
) error {
	if sandbox == nil || sandbox.cli == nil {
		return ErrSandboxNotConfigured
	}
	if !imageReferencePattern.MatchString(runtimes.JavaImageRef) ||
		!imageReferencePattern.MatchString(runtimes.PythonImageRef) {
		return ErrInvalidRuntimeConfig
	}
	return sandbox.cli.Preflight(ctx, []string{
		runtimes.JavaImageRef,
		runtimes.PythonImageRef,
	})
}

func (sandbox *DockerSandbox) RunCase(
	parent context.Context,
	request SandboxRequest,
) (SandboxObservation, error) {
	if sandbox == nil || sandbox.cli == nil || sandbox.journal == nil || !runnerInstanceIDPattern.MatchString(sandbox.runnerInstanceID) {
		return SandboxObservation{}, ErrSandboxNotConfigured
	}
	if err := validateSandboxRequest(request); err != nil {
		return SandboxObservation{}, err
	}

	wallClock := time.Duration(request.Limits.WallTimeMillis) * time.Millisecond
	runContext, cancel := context.WithTimeout(parent, wallClock)
	defer cancel()
	output := newBoundedOutput(request.Limits.OutputBytes, cancel)

	args, err := dockerCreateArguments(request)
	if err != nil {
		return SandboxObservation{}, err
	}
	lifecycleID, err := sandbox.journal.Begin(sandbox.runnerInstanceID, request.ExecutionID)
	if err != nil {
		return SandboxObservation{}, fmt.Errorf("persist sandbox create intent: %w", err)
	}
	args = append(args,
		"--label", "com.skyhigh.runner-instance="+sandbox.runnerInstanceID,
		"--label", "com.skyhigh.lifecycle-id="+lifecycleID,
	)
	containerID, err := sandbox.cli.Create(runContext, args)
	if err != nil {
		if parent.Err() != nil {
			return SandboxObservation{}, parent.Err()
		}
		if runContext.Err() != nil {
			return SandboxObservation{}, runContext.Err()
		}
		return SandboxObservation{}, ErrDockerCommandFailed
	}
	if !containerIDPattern.MatchString(containerID) {
		return SandboxObservation{}, ErrDockerCommandFailed
	}
	if err := sandbox.journal.Created(lifecycleID, containerID); err != nil {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), sandbox.cleanupTimeout)
		removeErr := sandbox.cli.Remove(cleanupContext, containerID)
		cleanupCancel()
		return SandboxObservation{}, errors.Join(
			fmt.Errorf("persist created sandbox container: %w", err),
			wrapDockerCleanupError(removeErr),
		)
	}
	containerRemoved := false
	defer func() {
		if !containerRemoved {
			cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), sandbox.cleanupTimeout)
			removeErr := sandbox.cli.Remove(cleanupContext, containerID)
			cleanupCancel()
			if removeErr == nil {
				containerRemoved = true
				_ = sandbox.journal.Cleaned(lifecycleID)
			}
		}
	}()

	// Source bytes are streamed directly into the container's private /workspace
	// tmpfs. The fixed wrapper consumes exactly len(SourceBytes), then the
	// student's test input remains on the same stdin stream for the program.
	startedAt := time.Now()
	exitCode, startErr := sandbox.cli.StartAttached(
		runContext,
		containerID,
		io.MultiReader(bytes.NewReader(request.SourceBytes), bytes.NewReader(request.StandardInput)),
		output.StdoutWriter(),
		output.StderrWriter(),
	)
	elapsed := time.Since(startedAt)

	// Inspect before removal only when the command completed normally. For
	// cancellation/timeout, removal itself is the kill operation.
	state := DockerContainerState{}
	var inspectErr error
	if startErr == nil && runContext.Err() == nil {
		inspectContext, inspectCancel := context.WithTimeout(context.Background(), sandbox.cleanupTimeout)
		state, inspectErr = sandbox.cli.Inspect(inspectContext, containerID)
		inspectCancel()
	}

	cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), sandbox.cleanupTimeout)
	removeErr := sandbox.cli.Remove(cleanupContext, containerID)
	cleanupCancel()
	if removeErr == nil {
		containerRemoved = true
		if journalErr := sandbox.journal.Cleaned(lifecycleID); journalErr != nil {
			removeErr = fmt.Errorf("persist sandbox cleanup confirmation: %w", journalErr)
		}
	}

	cleanupSucceeded := removeErr == nil
	standardOutput := output.StdoutBytes()
	compiled, standardOutput := removeCompileSuccessMarker(standardOutput)
	observation := SandboxObservation{
		StandardOutput:    standardOutput,
		StandardError:     output.StderrBytes(),
		CompilationStatus: CompilationNotStarted,
		ElapsedMillis:     elapsed.Milliseconds(),
		CleanupSucceeded:  cleanupSucceeded,
	}
	if compiled {
		observation.CompilationStatus = CompilationSucceeded
	}
	if parent.Err() != nil {
		return observation, parent.Err()
	}
	if !cleanupSucceeded {
		return observation, ErrDockerCommandFailed
	}
	if output.Exceeded() {
		observation.Status = SandboxOutputLimit
		return observation, nil
	}
	if errors.Is(runContext.Err(), context.DeadlineExceeded) {
		observation.Status = SandboxTimeLimit
		return observation, nil
	}
	if runContext.Err() != nil {
		return observation, runContext.Err()
	}
	if startErr != nil || inspectErr != nil || state.Status != "exited" || state.ExitCode != exitCode {
		return observation, ErrDockerCommandFailed
	}
	if state.OOMKilled {
		observation.Status = SandboxMemoryLimit
		return observation, nil
	}

	switch {
	case exitCode == 86 && !compiled:
		observation.Status = SandboxCompileError
		observation.CompilationStatus = CompilationFailed
	case exitCode == 87 && !compiled:
		return observation, ErrDockerCommandFailed
	case exitCode == 0:
		if !compiled {
			return observation, ErrDockerCommandFailed
		}
		observation.Status = SandboxSucceeded
	default:
		if !compiled {
			return observation, ErrDockerCommandFailed
		}
		observation.Status = SandboxRuntimeError
	}
	return observation, nil
}

func wrapDockerCleanupError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("remove container after lifecycle journal failure: %w", err)
}

func validateSandboxRequest(request SandboxRequest) error {
	if !containerSafeID(request.ExecutionID) ||
		(request.Language != protocol.LanguageJava && request.Language != protocol.LanguagePython) ||
		!imageReferencePattern.MatchString(request.ImageRef) {
		return protocol.ErrInvalidRequest
	}
	expectedName := sourceFileName(request.Language)
	if request.SourceName != expectedName || !runtimeVersionPattern.MatchString(request.RuntimeVersion) ||
		len(request.SourceBytes) == 0 || len(request.SourceBytes) > maximumSourceBytes {
		return protocol.ErrInvalidRequest
	}
	return validateExecutionLimits(request.Limits)
}

func containerSafeID(value string) bool {
	return artifactCaseIDPattern.MatchString(value)
}

func dockerCreateArguments(request SandboxRequest) ([]string, error) {
	if !imageReferencePattern.MatchString(request.ImageRef) ||
		len(request.SourceBytes) == 0 || len(request.SourceBytes) > maximumSourceBytes {
		return nil, ErrInvalidRuntimeConfig
	}
	cpuValue := strconv.FormatFloat(float64(request.Limits.CPUQuotaMillis)/1000, 'f', 3, 64)
	memoryValue := strconv.FormatInt(request.Limits.MemoryBytes, 10)
	processValue := strconv.FormatInt(request.Limits.ProcessLimit, 10)
	tmpfsSize := "64m"
	if request.Limits.MemoryBytes < 128*1024*1024 {
		tmpfsSize = "16m"
	}

	script := runtimeScript(request.Language, request.RuntimeVersion, len(request.SourceBytes))
	args := []string{
		"--runtime=runsc",
		"--network=none",
		"--ipc=none",
		"--read-only",
		"--tmpfs", "/workspace:rw,noexec,nosuid,nodev,size=" + tmpfsSize + ",uid=65532,gid=65532,mode=0700",
		"--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=" + tmpfsSize,
		"--memory=" + memoryValue,
		"--memory-swap=" + memoryValue,
		"--cpus=" + cpuValue,
		"--pids-limit=" + processValue,
		"--cap-drop=ALL",
		"--security-opt=no-new-privileges",
		"--user=65532:65532",
		"--ulimit", "core=0:0",
		"--interactive",
		"--env=HOME=/tmp",
		"--workdir=/workspace",
		"--label", "com.skyhigh.execution-id=" + request.ExecutionID,
		"--entrypoint=/bin/sh",
		request.ImageRef,
		"-c",
		script,
	}
	return args, nil
}

func removeCompileSuccessMarker(output []byte) (bool, []byte) {
	marker := []byte(compileSuccessMarker)
	if !bytes.HasPrefix(output, marker) {
		return false, output
	}
	return true, append([]byte(nil), output[len(marker):]...)
}

func runtimeScript(language protocol.Language, runtimeVersion string, sourceBytes int) string {
	sourceBootstrap := strings.Join([]string{
		"dd bs=1 count=" + strconv.Itoa(sourceBytes) + " of=/workspace/" + map[protocol.Language]string{protocol.LanguageJava: "Main.java", protocol.LanguagePython: "main.py"}[language] + " 2>/dev/null || exit 87",
		"[ \"$(wc -c < /workspace/" + map[protocol.Language]string{protocol.LanguageJava: "Main.java", protocol.LanguagePython: "main.py"}[language] + ")\" -eq " + strconv.Itoa(sourceBytes) + " ] || exit 87",
		"chmod 0444 /workspace/" + map[protocol.Language]string{protocol.LanguageJava: "Main.java", protocol.LanguagePython: "main.py"}[language] + " || exit 87",
		"chmod 0555 /workspace || exit 87",
	}, "\n")
	// RuntimeVersion is validated against runtimeVersionPattern before this
	// value enters the fixed shell program; student source is never interpolated.
	switch language {
	case protocol.LanguageJava:
		return strings.Join([]string{
			"set -eu",
			sourceBootstrap,
			"command -v javac >/dev/null 2>&1 || exit 87",
			"command -v java >/dev/null 2>&1 || exit 87",
			"java_version=\"$(java -version 2>&1)\" || exit 87",
			"case \"$java_version\" in *'version \"" + runtimeVersion + "'*) ;; *) exit 87 ;; esac",
			"mkdir -p /tmp/classes || exit 87",
			"if javac -J-XX:MaxRAMPercentage=50 -proc:none -d /tmp/classes /workspace/Main.java; then",
			"printf '" + compileSuccessMarker + "'",
			"  exec java -XX:MaxRAMPercentage=75 -Djava.io.tmpdir=/tmp -cp /tmp/classes Main",
			"else",
			"  exit 86",
			"fi",
		}, "\n")
	case protocol.LanguagePython:
		return strings.Join([]string{
			"set -eu",
			sourceBootstrap,
			"command -v python3 >/dev/null 2>&1 || exit 87",
			"python_version=\"$(python3 --version 2>&1)\" || exit 87",
			"case \"$python_version\" in 'Python " + runtimeVersion + "'*) ;; *) exit 87 ;; esac",
			"python3 -I -B -c 'import pathlib; compile(pathlib.Path(\"/workspace/main.py\").read_bytes(), \"/workspace/main.py\", \"exec\")' || exit 86",
			"printf '" + compileSuccessMarker + "'",
			"exec python3 -I -B /workspace/main.py",
		}, "\n")
	default:
		return "exit 87"
	}
}

type boundedOutput struct {
	mu       sync.Mutex
	stdout   bytes.Buffer
	stderr   bytes.Buffer
	total    int64
	maximum  int64
	exceeded bool
	cancel   context.CancelFunc
}

type boundedStream struct {
	parent   *boundedOutput
	isStderr bool
}

func newBoundedOutput(maximum int64, cancel context.CancelFunc) *boundedOutput {
	return &boundedOutput{maximum: maximum, cancel: cancel}
}

func (writer *boundedOutput) StdoutWriter() io.Writer {
	return boundedStream{parent: writer}
}

func (writer *boundedOutput) StderrWriter() io.Writer {
	return boundedStream{parent: writer, isStderr: true}
}

func (stream boundedStream) Write(value []byte) (int, error) {
	writer := stream.parent
	writer.mu.Lock()
	defer writer.mu.Unlock()
	originalLength := len(value)
	remaining := writer.maximum - writer.total
	if remaining > 0 {
		toWrite := value
		if int64(len(toWrite)) > remaining {
			toWrite = toWrite[:remaining]
		}
		if stream.isStderr {
			_, _ = writer.stderr.Write(toWrite)
		} else {
			_, _ = writer.stdout.Write(toWrite)
		}
		writer.total += int64(len(toWrite))
	}
	if int64(originalLength) > remaining && !writer.exceeded {
		writer.exceeded = true
		writer.cancel()
	}
	return originalLength, nil
}

func (writer *boundedOutput) StdoutBytes() []byte {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return append([]byte(nil), writer.stdout.Bytes()...)
}

func (writer *boundedOutput) StderrBytes() []byte {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return append([]byte(nil), writer.stderr.Bytes()...)
}

func (writer *boundedOutput) Exceeded() bool {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.exceeded
}
