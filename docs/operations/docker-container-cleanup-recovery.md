# Sky High — Docker container cleanup and crash recovery

> Status: **DESIGN / OPERATOR SAFETY NOTE — NOT AUTOMATED RECOVERY**
>
> A durable append-only lifecycle journal is now implemented and wired into the draft Docker sandbox adapter. An automated orphan reconciler is **not** implemented. Journal recovery is not proof that an orphan container can be safely deleted.

## Current behavior

The execution-capable `NewJournaledDockerSandbox` constructor requires a durable `sandboxjournal.Journal` and a stable 32-character lowercase-hex runner instance ID. `RunCase` fails closed when the journal is absent. Before Docker create, it fsyncs a `CREATE_INTENT` event and supplies Docker labels for the execution ID, runner instance ID, and unique lifecycle ID. Immediately after Docker returns a full container ID, it fsyncs a `CREATED` event. After Docker removal succeeds, it fsyncs a `CLEANED` event. The journal stores identifiers and timestamps only; it never stores source code, stdin, outputs, or test fixtures.

The journal is versioned append-only JSONL with a 64 MiB total limit, a 1 MiB record limit, strict transition validation, owner-only file permissions, an exclusive process lock, and fsync on every event. On restart it truncates only an incomplete final line; malformed complete records and invalid transitions fail closed. A stale lock is never removed automatically.

These attempts do not cover every failure:
- The runner process can terminate after Docker creates a container and before normal cleanup completes.
- The Docker daemon can be unavailable during removal.
- A deferred retry is best-effort; its own failure cannot be treated as cleanup success.
- The runner instance ID is now a required, validated `RUNNER_INSTANCE_ID` in foreground runner configuration. It must be generated once with a cryptographically secure random generator and persisted in a protected service environment file across restarts. The foreground process validates it but does not yet pass it into an enabled Docker executor; student execution remains disabled.
- A read-only exact-label discovery library now queries both runner-instance and lifecycle labels, then independently inspects each full container ID and verifies the labels. It correlates results with pending journal records and reports mismatches/ambiguity. It is a dry-run library only: there is no CLI, no host-level ownership lock, and no deletion path.
- The local acceptance journal records dispatch receipt lifecycle, not Docker container IDs or sandbox lifecycle transitions.

Consequently, the system cannot yet prove which container belongs to a particular runner instance or automatically distinguish a crashed runner's container from a live runner's container on a shared daemon.

## Mandatory safety rules

1. **No global container deletion.** Never use an unfiltered `docker rm`, `docker container prune`, or `docker rm $(docker ps -aq)` as runner recovery.
2. **No execution-ID-only janitor.** Do not automatically delete containers solely because they have a Sky High execution label or appear old.
3. **No startup cleanup.** Runner startup must not sweep Docker containers. A second live runner or overlapping restart must never have its active workload removed.
4. **Fail closed.** If cleanup cannot be confirmed, the execution result must not be reported as successful and the host must require operator investigation.
5. **Dedicated host boundary.** Until ownership and reconciliation are implemented, real qualification and any future runner execution must use a dedicated host/daemon with no unrelated workloads.
6. **Evidence preservation.** Record the full container ID, execution ID, timestamps, Docker command outcome, and cleanup outcome in the future durable lifecycle record. Do not put student source or test secrets in Docker labels or operational logs.

## Current manual recovery procedure (dedicated host only)

This procedure is for an operator investigating a disposable, dedicated runner host. It is not a substitute for automated recovery and must not be used on a shared Docker daemon.

1. Stop the runner service and prevent automatic restart.
2. Verify at the operating-system level that no runner process remains. Do not infer this only from a health endpoint or stale journal lock.
3. Confirm the host/daemon is dedicated to this Sky High runner and that no other legitimate Sky High runner is active.
4. List only containers carrying Sky High execution labels. When a runner instance ID is known, narrow the listing to that exact instance:

   ```bash
   docker ps -a --no-trunc --filter 'label=com.skyhigh.runner-instance=<RUNNER_INSTANCE_ID>' --filter 'label=com.skyhigh.execution-id'
   ```

5. Inspect each candidate individually and retain its full ID, labels, state, creation time, and inspect output as incident evidence:

   ```bash
   docker inspect <full-container-id>
   ```

6. Only after confirming the runner is stopped and the daemon is dedicated, remove each confirmed orphan by its exact full container ID:

   ```bash
   docker rm --force <full-container-id>
   ```

7. Repeat the filtered listing and verify it returns no Sky High execution containers. Preserve before/after output and Docker daemon logs.
8. Investigate why cleanup failed before restarting the runner. Do not remove the acceptance-journal lock until its separate ownership recovery procedure has been followed. For the sandbox lifecycle journal, remove its exact sibling `.lock` file only after verifying that no process can still own that journal path; never remove it while a runner may be active.

If any step cannot be established, stop and escalate; do not guess which container is safe to remove.

## Required engineering design before automated reconciliation

The implementation must be reviewed as a separate safety change and should include:

- A cryptographically random, installation-specific runner instance ID, generated/provisioned outside student-controlled input.
- Docker labels for both the runner instance and execution ID. Labels are identifiers, not authorization by themselves.
- Complete the handoff of the configured stable runner-instance ID into the future journaled Docker executor, then document and test identity rotation/replacement behavior. The sandbox journal and Docker labels already support this identity.
- Extend the current read-only discovery library into an operator command with retained evidence output. It already queries both exact runner-instance and lifecycle-ID labels, inspects each full ID, and correlates candidates with pending records; the create-intent-without-container-ID window is reported as an operator-review candidate.
- An operator-invoked, dry-run-first reconciler that lists exact owned candidates, requires proof that the previous process is stopped, and refuses ambiguous ownership. No automatic startup janitor.
- An exclusive host-level ownership lock shared by normal execution and reconciliation. Reconciliation must fail closed if lock ownership is ambiguous.
- A manual, dry-run-first reconciliation command that reports candidates and evidence. The current implementation is only a library function and does not remove containers. Deletion must require explicit operator action and exact full container IDs until automated ownership proof has been independently reviewed.
- Tests for daemon unavailable before create, daemon lost after create, process crash at each lifecycle boundary, duplicate runner startup, stale lock, cleanup failure, malformed journal records, and unrelated containers remaining untouched.
- Real Linux/runsc failure-injection evidence and independent security review before any automatic cleanup path is enabled.

## Release gate

Until runner identity provisioning, exact-label discovery, operator reconciliation, crash-window failure injection, and independent security review exist, orphan cleanup remains an explicit sandbox qualification blocker. A green CI run or successful normal-path `docker rm --force` is not sufficient. Student execution remains disabled.
