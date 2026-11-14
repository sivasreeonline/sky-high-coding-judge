# Sky High — Real runsc Host Qualification Record

> Status: **TEMPLATE — NOT EVIDENCE**  
> Do not mark the execution sandbox qualified from this document alone. Complete it only on a disposable, dedicated Linux host. Preserve raw command output and test logs as immutable evidence.

## 1. Qualification identity

| Field | Recorded value |
|---|---|
| Qualification ID | |
| Date/time (UTC) | |
| Operator | |
| Independent reviewer | |
| Host purpose / isolation boundary | |
| Host cloud/provider or hardware | |
| Host architecture | |
| Linux distribution and release | |
| Kernel release | |
| Kernel package/build identifier | |
| cgroup version (v1/v2) | |
| Docker Engine version | |
| Docker API version | |
| containerd version | |
| runsc version | |
| runsc binary SHA-256 | |
| Host rebooted immediately before test? | |
| Host dedicated to qualification only? | |

## 2. Runtime image provenance

Record the complete immutable image reference and the locally inspected image ID. Do not use mutable tags as qualification identities.

| Runtime | Image reference including digest | Local image ID | Runtime version observed | Build/source provenance |
|---|---|---|---|---|
| Java | | | | |
| Python | | | | |

Attach the image build records, SBOMs, vulnerability scan results, signature verification, and provenance attestations. A digest alone does not establish that an image is trustworthy.

## 3. Host preflight

Record exact output for:

- `uname -a`
- `cat /etc/os-release`
- `docker version`
- `docker info`
- `docker info --format '{{json .Runtimes}}'`
- `runsc --version`
- `docker image inspect` for each exact digest-pinned runtime image
- cgroup memory and PID controller availability
- host security mode and relevant kernel/runtime configuration

| Check | Result (PASS/FAIL) | Evidence file / SHA-256 | Notes |
|---|---|---|---|
| Linux host and expected architecture | | | |
| runsc runtime registered and executable | | | |
| Docker daemon healthy | | | |
| Both exact image digests already local | | | |
| cgroup memory controller available | | | |
| cgroup PID controller available | | | |
| No image pull occurred during qualification | | | |
| Host has no unrelated workloads | | | |

## 4. Exact qualification invocation

Record the exact commands, environment variable names (never secret values), working directory, exit code, start/end timestamps, and raw output. The opt-in suite must be run with the approved image digests:

```bash
go test -tags=sandboxqualification -run '^TestRunscHostileWorkload' ./internal/runner -count=1 -v
```

| Test | Expected result | Actual result | Exit code | Evidence file / SHA-256 |
|---|---|---|---|---|
| Java hostile workload isolation | PASS | | | |
| Python hostile workload isolation | PASS | | | |
| Output limit and cleanup | PASS | | | |
| Wall-time limit and cleanup | PASS | | | |
| PID limit and cleanup | PASS | | | |
| Memory pressure containment and cleanup | PASS | | | |

The suite must demonstrate non-root UID 65532, read-only root/workspace, no outbound IP or DNS, no Docker socket, bounded output/time/PIDs/memory, and container cleanup. A test that did not run is NOT PASS.

## 5. Additional failure-injection evidence

The current automated suite is not sufficient for these operator-level checks. Perform them in a disposable host environment and attach reproducible scripts/logs.

| Failure injection | Required observation | Result | Evidence file / SHA-256 |
|---|---|---|---|
| Docker daemon unavailable before create | Preflight fails closed; no work claimed | | |
| Daemon lost after container create | Execution fails closed; orphan cleanup/reconciliation is documented and verified | | |
| Daemon lost during attached execution | No successful result is reported; recovery is deterministic | | |
| Cleanup command fails | Cleanup is reported as failed; execution cannot be marked successful | | |
| Runner process killed during execution | No stale lease/result is accepted; container lifecycle is reconciled | | |
| Host reboot during execution | No orphan workload or false completion after restart | | |
| Memory ceiling exceeded | Workload is contained; host remains healthy | | |
| PID ceiling exceeded | Fork/process storm is contained; host remains healthy | | |
| Output flood | Captured output remains within configured byte cap | | |
| Wall-time exceeded | Workload is terminated and cleanup is verified | | |

## 6. Post-run host and container audit

Record before/after comparisons and evidence:

- No qualification containers remain running or stopped.
- No unexpected mounts, devices, host namespaces, published ports, or privileged containers were created.
- No unexpected Docker volumes, networks, images, or host files were created.
- Host memory, CPU, PID availability, disk space, and daemon health returned to acceptable baselines.
- Docker daemon and kernel logs contain no unexplained errors or security warnings.
- Any orphaned container is treated as a qualification failure until explained and resolved.

| Audit | Result (PASS/FAIL) | Evidence file / SHA-256 | Notes |
|---|---|---|---|
| Container inventory clean | | | |
| No unexpected host resources | | | |
| Host health returned to baseline | | | |
| Daemon/kernel logs reviewed | | | |
| All test evidence checksummed and retained | | | |

## 7. Security review and release decision

Independent reviewer must examine the exact commit, runtime image digests, host configuration, raw test evidence, threat model, failure-injection results, and cleanup behavior. A passing test suite is not a substitute for a host-escape review.

| Approval gate | Reviewer | Result | Date | Evidence / notes |
|---|---|---|---|---|
| Docker argument and mount policy reviewed | | | | |
| runsc / kernel / Docker versions reviewed | | | | |
| Host-escape and privilege-boundary review | | | | |
| Runtime image provenance and vulnerability review | | | | |
| Memory, PID, CPU, output and time controls reviewed | | | | |
| Daemon failure and cleanup behavior reviewed | | | | |
| Evidence integrity and retention reviewed | | | | |
| Final independent security approval | | | | |

**Final disposition:** NOT QUALIFIED / QUALIFIED WITH EXPLICIT SCOPE

**Approved commit SHA:**

**Approved Java image digest:**

**Approved Python image digest:**

**Approved host/kernel/runtime scope:**

**Residual risks and expiry/retest date:**

> Student execution remains disabled unless every mandatory gate is complete and a separately authorized release explicitly enables the runner. Any change to the kernel, Docker/containerd, runsc, runtime images, sandbox flags, or execution wrapper requires impact review and may require requalification.
