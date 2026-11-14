//go:build linux && sandboxqualification

package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sivasreeonline/orbit-engine/services/execution-runner/internal/protocol"
	"github.com/sivasreeonline/orbit-engine/services/execution-runner/internal/sandboxjournal"
)

// These tests execute hostile-but-bounded programs only inside a real runsc
// container. They are opt-in and require preloaded, digest-pinned images.
func qualificationRuntime(t *testing.T) (*DockerSandbox, RuntimeConfiguration) {
	t.Helper()
	javaImage := os.Getenv("SKYHIGH_JAVA_IMAGE_REF")
	pythonImage := os.Getenv("SKYHIGH_PYTHON_IMAGE_REF")
	javaVersion := os.Getenv("SKYHIGH_JAVA_VERSION")
	pythonVersion := os.Getenv("SKYHIGH_PYTHON_VERSION")
	if javaImage == "" || pythonImage == "" || javaVersion == "" || pythonVersion == "" {
		t.Fatal("set SKYHIGH_JAVA_IMAGE_REF, SKYHIGH_PYTHON_IMAGE_REF, SKYHIGH_JAVA_VERSION and SKYHIGH_PYTHON_VERSION")
	}
	runtimes := RuntimeConfiguration{
		RunnerVersion:  "qualification",
		JavaVersion:    javaVersion,
		JavaImageRef:   javaImage,
		PythonVersion:  pythonVersion,
		PythonImageRef: pythonImage,
	}
	journal, err := sandboxjournal.Open(filepath.Join(t.TempDir(), "qualification-lifecycle.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := journal.Close(); err != nil {
			t.Errorf("close qualification lifecycle journal: %v", err)
		}
	})
	sandbox, err := NewJournaledDockerSandbox(OSCommandDockerCLI{}, journal, strings.Repeat("b", 32))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := sandbox.Preflight(ctx, runtimes); err != nil {
		t.Fatalf("runsc and local image preflight failed: %v", err)
	}
	return sandbox, runtimes
}

func qualificationLimits() protocol.ResourceLimits {
	return protocol.ResourceLimits{
		CPUQuotaMillis: 1000,
		MemoryBytes:    256 * 1024 * 1024,
		WallTimeMillis: 8000,
		ProcessLimit:   32,
		OutputBytes:    4096,
	}
}

func TestRunscHostileWorkloadIsolation(t *testing.T) {
	sandbox, runtimes := qualificationRuntime(t)
	tests := []struct {
		name     string
		language protocol.Language
		version  string
		image    string
		source   string
		sourceName string
	}{
		{
			name: "python",
			language: protocol.LanguagePython,
			version: runtimes.PythonVersion,
			image: runtimes.PythonImageRef,
			sourceName: "main.py",
			source: `import os, socket
from pathlib import Path
print(os.getuid())
mounts = [line.split() for line in Path("/proc/mounts").read_text().splitlines()]
root = next((fields[3] for fields in mounts if len(fields) > 3 and fields[1] == "/"), "")
print("ROOT_MOUNT_RO" if "ro" in root.split(",") else "ROOT_MOUNT_RW")
try:
    with socket.create_connection(("1.1.1.1", 53), timeout=1):
        print("NETWORK_OPEN")
        raise SystemExit(3)
except OSError:
    print("NETWORK_BLOCKED")
try:
    socket.getaddrinfo("example.com", 443)
    print("DNS_OPEN")
    raise SystemExit(4)
except OSError:
    print("DNS_BLOCKED")
if Path("/var/run/docker.sock").exists():
    print("DOCKER_SOCKET_PRESENT")
    raise SystemExit(5)
print("DOCKER_SOCKET_ABSENT")
try:
    Path("/workspace/escape.txt").write_text("escape")
    print("WORKSPACE_WRITABLE")
    raise SystemExit(6)
except OSError:
    print("WORKSPACE_READ_ONLY")
`,
		},
		{
			name: "java",
			language: protocol.LanguageJava,
			version: runtimes.JavaVersion,
			image: runtimes.JavaImageRef,
			sourceName: "Main.java",
			source: `import java.io.IOException;
import java.net.InetAddress;
import java.net.InetSocketAddress;
import java.net.Socket;
import java.net.UnknownHostException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.regex.Matcher;
import java.util.regex.Pattern;
public class Main {
  public static void main(String[] args) throws Exception {
    String status = Files.readString(Path.of("/proc/self/status"));
    Matcher uid = Pattern.compile("Uid:\\s+(\\d+)").matcher(status);
    System.out.println(uid.find() ? uid.group(1) : "UID_UNKNOWN");
    try (Socket socket = new Socket()) {
      socket.connect(new InetSocketAddress("1.1.1.1", 53), 1000);
      System.out.println("NETWORK_OPEN");
      System.exit(3);
    } catch (IOException expected) {
      System.out.println("NETWORK_BLOCKED");
    }
    String rootMount = Files.readAllLines(Path.of("/proc/mounts")).stream()
      .filter(line -> line.split("\\s+").length > 3 && line.split("\\s+")[1].equals("/"))
      .map(line -> line.split("\\s+")[3])
      .findFirst().orElse("");
    System.out.println(java.util.Arrays.asList(rootMount.split(",")).contains("ro")
      ? "ROOT_MOUNT_RO" : "ROOT_MOUNT_RW");
    try {
      InetAddress.getAllByName("example.com");
      System.out.println("DNS_OPEN");
      System.exit(4);
    } catch (UnknownHostException expected) {
      System.out.println("DNS_BLOCKED");
    }
    if (Files.exists(Path.of("/var/run/docker.sock"))) {
      System.out.println("DOCKER_SOCKET_PRESENT");
      System.exit(5);
    }
    System.out.println("DOCKER_SOCKET_ABSENT");
    try {
      Files.writeString(Path.of("/workspace/escape.txt"), "escape");
      System.out.println("WORKSPACE_WRITABLE");
      System.exit(6);
    } catch (IOException expected) {
      System.out.println("WORKSPACE_READ_ONLY");
    }
  }
}`,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			request := SandboxRequest{
				ExecutionID: "runsc_qualification_" + test.name,
				Language: test.language,
				RuntimeVersion: test.version,
				ImageRef: test.image,
				SourceName: test.sourceName,
				SourceBytes: []byte(test.source),
				Limits: qualificationLimits(),
			}
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			observation, err := sandbox.RunCase(ctx, request)
			if err != nil {
				t.Fatalf("real runsc workload failed: %v; observation=%+v", err, observation)
			}
			if observation.Status != SandboxSucceeded || !observation.CleanupSucceeded {
				t.Fatalf("sandbox did not finish and clean up: %+v", observation)
			}
			output := string(observation.StandardOutput)
			for _, expected := range []string{"65532", "NETWORK_BLOCKED", "DNS_BLOCKED", "DOCKER_SOCKET_ABSENT", "ROOT_MOUNT_RO", "WORKSPACE_READ_ONLY"} {
				if !strings.Contains(output, expected) {
					t.Errorf("output %q does not contain isolation proof %q", output, expected)
				}
			}
			if strings.Contains(output, "NETWORK_OPEN") || strings.Contains(output, "DNS_OPEN") ||
				strings.Contains(output, "DOCKER_SOCKET_PRESENT") ||
				strings.Contains(output, "ROOT_MOUNT_RW") ||
				strings.Contains(output, "WORKSPACE_WRITABLE") {
				t.Fatalf("isolation boundary was violated: %q", output)
			}
		})
	}
}

func TestRunscHostileWorkloadResourceAndCleanupBoundaries(t *testing.T) {
	sandbox, runtimes := qualificationRuntime(t)
	request := SandboxRequest{
		ExecutionID: "runsc_qualification_output",
		Language: protocol.LanguagePython,
		RuntimeVersion: runtimes.PythonVersion,
		ImageRef: runtimes.PythonImageRef,
		SourceName: "main.py",
		SourceBytes: []byte("print('X' * 1000000)"),
		Limits: qualificationLimits(),
	}
	request.Limits.OutputBytes = 1024
	observation, err := sandbox.RunCase(context.Background(), request)
	if err != nil {
		t.Fatalf("output-limit workload: %v; observation=%+v", err, observation)
	}
	if observation.Status != SandboxOutputLimit || !observation.CleanupSucceeded {
		t.Fatalf("output cap or cleanup failed: %+v", observation)
	}

	request.ExecutionID = "runsc_qualification_timeout"
	request.SourceBytes = []byte("while True:\n    pass\n")
	request.Limits.OutputBytes = 4096
	request.Limits.WallTimeMillis = 500
	observation, err = sandbox.RunCase(context.Background(), request)
	if err != nil {
		t.Fatalf("timeout workload: %v; observation=%+v", err, observation)
	}
	if observation.Status != SandboxTimeLimit || !observation.CleanupSucceeded {
		t.Fatalf("wall-time cap or cleanup failed: %+v", observation)
	}

	request.ExecutionID = "runsc_qualification_process_limit"
	request.Limits.WallTimeMillis = 5000
	request.SourceBytes = []byte(`import os
children = []
for _ in range(100):
    try:
        pid = os.fork()
    except OSError:
        break
    if pid == 0:
        os._exit(0)
    children.append(pid)
for pid in children:
    os.waitpid(pid, 0)
print("FORKS", len(children))
`)
	observation, err = sandbox.RunCase(context.Background(), request)
	if err != nil {
		t.Fatalf("process-limit workload: %v; observation=%+v", err, observation)
	}
	if observation.Status != SandboxSucceeded || !observation.CleanupSucceeded {
		t.Fatalf("process limit workload did not terminate cleanly: %+v", observation)
	}
	var forkCount int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(observation.StandardOutput)), "FORKS %d", &forkCount); err != nil {
		t.Fatalf("could not parse process-limit probe output %q: %v", observation.StandardOutput, err)
	}
	if forkCount >= 100 {
		t.Fatalf("pids limit was not enforced; forked %d children", forkCount)
	}
}

func TestRunscMemoryPressureIsContainedAndCleanedUp(t *testing.T) {
	sandbox, runtimes := qualificationRuntime(t)
	request := SandboxRequest{
		ExecutionID:    "runsc_qualification_memory",
		Language:       protocol.LanguagePython,
		RuntimeVersion: runtimes.PythonVersion,
		ImageRef:       runtimes.PythonImageRef,
		SourceName:     "main.py",
		SourceBytes: []byte(`import time
chunks = []
while True:
    # Touch each page so the cgroup accounts for the allocation.
    chunks.append(bytearray(8 * 1024 * 1024))
    time.sleep(0.02)
`),
		Limits: qualificationLimits(),
	}
	request.Limits.MemoryBytes = 96 * 1024 * 1024
	request.Limits.WallTimeMillis = 10000

	observation, err := sandbox.RunCase(context.Background(), request)
	if err != nil {
		t.Fatalf("memory-pressure workload: %v; observation=%+v", err, observation)
	}
	if observation.Status != SandboxMemoryLimit || !observation.CleanupSucceeded {
		t.Fatalf("memory pressure was not contained and cleaned up: %+v", observation)
	}
}
