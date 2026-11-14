# Sky High Execution Runner — M0 Contract Skeleton

This is the initial Go runner foundation. It is intentionally **not an execution service**.

The canonical Go module path is `github.com/sivasreeonline/orbit-engine/services/execution-runner`.

## Current capabilities
- Loopback-only HTTP health endpoint: `GET /healthz`.
- Local append-only acceptance journal with fsync-before-send semantics, restart recovery and pending-receipt replay support.
- Outbound Ed25519-signed claim/accept/start/heartbeat/artifact/completion HTTP client with strict bounded response validation.
- Artifact responses are independently checked for source SHA-256, cross-language test-bundle digest, action scope, fixture visibility, ordering and size limits.
- Readiness endpoint returns HTTP 503 until a sandbox executor is configured.
- Versioned internal execution request types and strict structural validation.
- An executor interface with a safe unconfigured implementation.
- Unit tests for health/readiness, request validation and disabled execution.
- Go vulnerability analysis is performed by govulncheck v1.9.0 in GitHub Actions.

## Explicit safety boundary
- No endpoint accepts source code or starts a process.
- The acceptance journal stores only opaque IDs, lease identity and receipt lifecycle metadata; it never stores source code, hidden tests or execution artifacts.
- A single runner process must own a journal file. Journal files are created with owner-only permissions (0600), and incomplete trailing records are truncated during recovery.
- The default `cmd/runner` health process never invokes Docker, containerd, gVisor or host execution commands.
- The sandbox adapter is an injectable library component and is not constructed by `cmd/runner`.
- The unconfigured executor always returns `ErrSandboxNotConfigured`.
- The HTTP server refuses non-loopback bind addresses until an authenticated private transport is implemented.
- Student source code is referenced by an opaque artifact ID and SHA-256 digest; it is not included in the runner request body.
- This skeleton must not be deployed as a production runner.

## Local development

```powershell
go test ./...
go vet ./...
gofmt -w .
```

Run the health-only process:

```powershell
go run ./cmd/runner
```

The default listener is `127.0.0.1:8080`.

- `GET http://127.0.0.1:8080/healthz` returns process health.
- `GET http://127.0.0.1:8080/readyz` returns HTTP 503 with `sandbox_executor_not_configured`.

Health does not imply readiness.

## Acceptance journal

The local journal is a write-ahead record for the future runner protocol:

1. Persist a `PREPARED` receipt event and fsync the journal.
2. Only after that succeeds, send the signed acceptance receipt to the control plane.
3. After receiving the durable API acknowledgement, append a `CONFIRMED` event and fsync again.
4. After a restart, retry every prepared-but-unconfirmed receipt using the same acceptanceId and receipt body, but a fresh signed HTTP nonce.

The journal is append-only JSONL with a 64 MiB initial size ceiling. It rejects malformed complete records and only truncates an incomplete final line left by a crash. A definitive signed `409 RUNNER_LEASE_STALE` response appends an `ABANDONED` event for that exact acceptance receipt; the record remains auditable but is excluded from future retries. Transport failures, 5xx responses and other unknown errors are never treated as abandonment.

## Acceptance journal process ownership

The acceptance journal enforces one process owner per canonical journal path. Opening a journal atomically creates a sibling `<journal-path>.lock` directory with a private owner record. A second process is rejected with `ErrJournalLocked` until the owner closes cleanly. Relative paths and symlinked parent-directory aliases are canonicalized before the lock is acquired.

A clean close flushes the journal, closes its file descriptor, verifies the lock's private owner record and token, and then removes the lock directory. If the process crashes or lock ownership becomes uncertain, the lock is deliberately left in place. The runner never guesses that a lock is stale and never removes it automatically. An operator must first verify that no runner process still owns the journal, then remove the stale `.lock` directory before restarting. Do not delete a lock directory while a runner may still be active.

The lock directory and owner record are created with owner-only permissions. Linux directory fsync is used to persist lock creation and release. Windows remains a local-development target only; its directory-sync helper is intentionally a no-op, and production runner durability remains Linux-only.

## Machine identity private-key file

The `runnerauth.LoadPrivateKeyFile` helper provides the strict Linux file-loading boundary for the runner's Ed25519 machine identity. The file contains the standard-base64 encoding of exactly one 64-byte Ed25519 private key, optionally followed by a single LF newline. It must be a regular non-symlink file, owned by the runner's effective user, have exactly one hard link, and grant no group/other permissions. Owner-read-only (0400) and owner-read/write (0600) modes are accepted. The loader opens without following symlinks, verifies the opened file matches the inspected path, and checks that the public half matches the seed.

Provision the file through a trusted local secret-management process. Never commit it, print it, include it in diagnostics or pass the key contents through command-line arguments. Non-Linux builds deliberately return `ErrPrivateKeyFileUnsupported`; they do not weaken the production file checks. This helper is not yet wired into `cmd/runner`, which remains health-only.

## Typed runner identity configuration

The `internal/runnerconfig` package validates the environment contract for a future foreground runner mode:

- `RUNNER_CONTROL_PLANE_URL`: an HTTPS origin; HTTP is accepted only for a loopback development endpoint.
- `RUNNER_KEY_ID`: a bounded key identifier matching the signed-request protocol.
- `RUNNER_PRIVATE_KEY_FILE`: an absolute path to a private key file accepted by the strict Linux `runnerauth.LoadPrivateKeyFile` loader.
- `RUNNER_JOURNAL_PATH`: an absolute, non-root path for the append-only acceptance journal.
- `RUNNER_INSTANCE_ID`: a stable, installation-specific 32-character lowercase-hex identifier. It is distinct from the signing key ID and is used to scope sandbox lifecycle labels and recovery ownership.

All five settings are mandatory. The loader rejects credentialed URLs, URL paths, query strings, fragments, invalid key IDs and relative file paths. It never returns key contents in errors. Call `Config.Destroy()` when the configuration is no longer needed; it overwrites the in-memory private-key bytes and clears the reference.

This package is a validated configuration boundary. The foreground process loads and validates the stable runner instance ID alongside its signing identity. Provision the ID once using a cryptographically secure random generator, store it in a root/runner-owned environment file with mode `0600`, and preserve it across restarts. Do not generate a new ID on every process start. Example on the dedicated Linux host:

```bash
install -d -m 0700 /etc/skyhigh
umask 077
printf 'RUNNER_INSTANCE_ID=%s\\n' "$(openssl rand -hex 16)" > /etc/skyhigh/runner-instance.env
chmod 0600 /etc/skyhigh/runner-instance.env
```

Load that file through the service manager's environment-file mechanism. Back it up as installation metadata (not as a signing secret); replacing the runner installation requires an explicit identity-rotation and orphan-reconciliation procedure. The ID is currently validated at foreground startup but is not yet connected to an enabled Docker executor or automatic cleanup. This does not enable student execution.

## Current runner control-plane client

The Go client exposes separate methods for:
1. Claim metadata-only dispatches.
2. Persist and submit durable acceptance receipts.
3. Request the lease-fenced `CLAIMED -> RUNNING` transition.
4. Renew a started lease with a stable heartbeat ID (reuse the same ID after a lost acknowledgement).
5. Fetch source and the action-scoped test bundle using the exact live dispatch lease.
6. Submit an allowlisted aggregate terminal result against the same dispatch and lease.

The client verifies the returned source hash and canonical test-bundle hash. Completion retries preserve the result body and use a fresh signed nonce; the control plane returns the existing completion receipt when the result is identical. It rejects hidden tests in a `RUN` artifact and permits hidden fixtures only for the trusted runner's `SUBMIT` artifact. The methods are individually testable and are not yet wired into a continuously polling process.

## Single-dispatch lifecycle coordinator

The Go runner now includes an injectable `runner.Worker` coordinator with a single-dispatch `RunOnce` operation. It orders the control-plane operations as follows:

1. Replay locally prepared, unconfirmed acceptance receipts.
2. Verify that the injected artifact executor explicitly reports ready.
3. Claim one dispatch.
4. Persist and submit the acceptance receipt.
5. Request the lease-fenced start transition.
6. Fetch and verify the private source/test artifact.
7. Execute through the injected artifact-executor interface while renewing the lease.
8. Stop on a fenced/expired heartbeat; never submit a result for a lease the worker no longer owns.
9. Submit the allowlisted aggregate result, retrying transient failures with the exact same result payload.

A heartbeat retry reuses the same heartbeat ID and the HTTP client creates a fresh signed nonce for each transport attempt. The last acknowledged lease expiry is carried forward to artifact and completion calls. Completion retries keep the result immutable.

This coordinator is a library operation, not a polling daemon. It claims at most one dispatch per invocation and does not create goroutines that poll forever. It refuses to claim work unless an executor reports ready. `UnconfiguredArtifactExecutor` always reports not ready and returns `ErrSandboxNotConfigured`.

The `cmd/runner` entry point remains health-only and loopback-bound. It does not construct a runner identity, open the acceptance journal for production work, start the coordinator, or execute student code. The coordinator is not yet connected to a real OCI/gVisor executor.

## Draft artifact-backed runsc sandbox adapter

The Go library now includes `SandboxArtifactExecutor`, which adapts the authenticated `ExecutionArtifact` returned by the runner client into the existing execution contract. It rechecks source integrity, binds fixtures to the exact execution/attempt/problem version and uses separately configured resource limits.

The `DockerSandbox` adapter creates a fresh container per testcase with:
- the `runsc` runtime, network and IPC disabled, read-only root filesystem;
- a non-root UID/GID (65532:65532), all Linux capabilities dropped and no-new-privileges;
- bounded memory/swap, CPU quota, PID count, wall time and combined stdout/stderr;
- a non-executable, nodev/nosuid `/workspace` tmpfs and bounded `/tmp` tmpfs;
- forced container removal is attempted after completion, timeout, cancellation or output overflow; failures are surfaced. A read-only exact-label discovery library correlates pending journal entries with independently inspected container labels, but it does not delete containers and is not wired to a CLI or startup path.

Student source is not written to a host temporary file or bind-mounted from the host. The runner sends the source bytes first over the container's attached stdin; a fixed wrapper consumes exactly the known source-byte count into `/workspace`, verifies the byte count, makes the file and workspace directory read-only, then leaves the remaining stdin stream for the testcase input. Student source is never interpolated into shell text.

The execution adapter currently uses exact-byte output comparison. The artifact contract does not yet carry a versioned comparison mode, so whitespace-normalizing comparison is deliberately not inferred.

**This remains a draft, not a live sandbox proof.** Execution requires `NewJournaledDockerSandbox` with a durable sandbox lifecycle journal and a stable runner-instance ID. The Docker CLI and execution contract tests use fakes. CI does not start Docker or run a gVisor container. Runtime images have not been built, signed or qualified. Peak-memory sampling is not implemented. The sandbox adapter is disabled unless its caller explicitly sets `Enabled: true`; enabling it also requires a successful Docker/runsc/image preflight. It is not wired into the health-only command. Do not set that flag for student workloads until the real Linux/runsc and adversarial gates below pass.

## M0 next gates
1. Add reviewed machine-key file loading and verify owner-only key permissions on the supported Linux host.
2. Integrate claim → accept → start → heartbeat → artifact retrieval → execute → complete in a bounded runner lifecycle with backoff, cancellation and graceful shutdown.
3. Select a supported Linux host and architecture.
4. Provision a disposable supported Linux host with a pinned Docker/OCI engine and install/verify the exact `runsc` runtime.
5. Build, sign and pin Java/Python runtime images; verify their digests and runtime versions on the host.
6. Run real Java/Python smoke tests and the full adversarial matrix: network/DNS, host filesystem traversal, process exhaustion, memory exhaustion, infinite loops, output floods, cancellation and cleanup failures.
7. Verify container cleanup and lease-loss cancellation under real process execution; implement measured peak-memory sampling.
8. Complete an independent security review and cleanup attestation design. Keep the adapter disabled until all gates pass.


## Opt-in real runsc qualification suite

The default CI suite uses a fake Docker boundary and never runs student code. A separate Linux-only integration suite is available for an operator-controlled host with gVisor `runsc` and both digest-pinned runtime images already loaded locally.

The suite verifies, inside real containers:
- UID 65532 (non-root execution);
- outbound IP and DNS isolation;
- absence of the Docker daemon socket from the workload;
- read-only container root filesystem and student workspace after source staging;
- bounded output and wall-clock cancellation;
- process-count enforcement and cleanup;
- memory-pressure containment under the configured container memory ceiling, with cleanup after the workload is killed.

It never pulls images. Supply the exact digest references and versions that were independently reviewed:

```bash
export SKYHIGH_JAVA_IMAGE_REF='registry.example/sky-high/java@sha256:<64-hex-digest>'
export SKYHIGH_PYTHON_IMAGE_REF='registry.example/sky-high/python@sha256:<64-hex-digest>'
export SKYHIGH_JAVA_VERSION='25'
export SKYHIGH_PYTHON_VERSION='3.14'
go test -tags=sandboxqualification -run '^TestRunscHostileWorkload' ./internal/runner -count=1 -v
```

Run this only on a disposable, dedicated Linux qualification host. It invokes the local Docker daemon with the exact `runsc` runtime. The test is not run by ordinary CI and does not itself authorize student execution. A successful run is one evidence input; memory-pressure behavior, host escape review, kernel/runtime patch level, cleanup after daemon failure, and independent security review remain release gates.
