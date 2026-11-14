package runner

import (
	"strings"
	"testing"
)

// Keep the generated Docker command from silently acquiring host access as
// the sandbox evolves. These are policy regression tests, not a substitute
// for the real runsc qualification suite on a dedicated Linux host.
func TestDockerCreateArgumentsRejectHostAccessAndPrivilegeEscalation(t *testing.T) {
	t.Parallel()

	args, err := dockerCreateArguments(dockerSandboxTestRequest())
	if err != nil {
		t.Fatalf("dockerCreateArguments: %v", err)
	}

	forbidden := []string{
		"--privileged",
		"--device",
		"--device=",
		"--volume",
		"--volume=",
		"-v",
		"--mount",
		"--mount=",
		"--volumes-from",
		"--pid=host",
		"--ipc=host",
		"--network=host",
		"--uts=host",
		"--userns=host",
		"--cap-add",
		"--cap-add=",
		"--publish",
		"--publish=",
		"--publish-all",
		"-p",
		"--add-host",
		"--add-host=",
		"--security-opt=apparmor=unconfined",
		"--security-opt=seccomp=unconfined",
		"--security-opt=label=disable",
	}
	for _, argument := range args {
		for _, denied := range forbidden {
			if argument == denied || (strings.HasSuffix(denied, "=") && strings.HasPrefix(argument, denied)) {
				t.Errorf("generated Docker arguments contain forbidden host-access option %q (matched %q): %v", argument, denied, args)
			}
		}
	}

	if !containsArgument(args, "--runtime=runsc") ||
		!containsArgument(args, "--network=none") ||
		!containsArgument(args, "--ipc=none") ||
		!containsArgument(args, "--read-only") ||
		!containsArgument(args, "--cap-drop=ALL") ||
		!containsArgument(args, "--security-opt=no-new-privileges") {
		t.Fatalf("required isolation policy regressed: %v", args)
	}
}

func TestDockerCreateArgumentsAllowOnlyPrivateTmpfsMounts(t *testing.T) {
	t.Parallel()

	args, err := dockerCreateArguments(dockerSandboxTestRequest())
	if err != nil {
		t.Fatalf("dockerCreateArguments: %v", err)
	}

	allowed := map[string]bool{
		"/workspace": false,
		"/tmp":       false,
	}
	for index, argument := range args {
		if argument != "--tmpfs" {
			continue
		}
		if index+1 >= len(args) {
			t.Fatal("--tmpfs has no mount specification")
		}
		specification := args[index+1]
		mountPath, _, _ := strings.Cut(specification, ":")
		if _, ok := allowed[mountPath]; !ok {
			t.Errorf("unexpected tmpfs mount %q; only private /workspace and /tmp are allowed", specification)
			continue
		}
		if allowed[mountPath] {
			t.Errorf("duplicate tmpfs mount for %s", mountPath)
		}
		allowed[mountPath] = true
		if !strings.Contains(specification, "noexec,nosuid,nodev") {
			t.Errorf("tmpfs %s lacks noexec,nosuid,nodev: %q", mountPath, specification)
		}
	}
	for mountPath, found := range allowed {
		if !found {
			t.Errorf("required private tmpfs mount %s is missing", mountPath)
		}
	}
}
