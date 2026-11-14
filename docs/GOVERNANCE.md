# What Quadstack must always guarantee

These rules hold for every change. A change to one of them is written down here first, with the reason, and is at least a minor version. Each rule names where it is enforced and tested.

## Sign-in

1. **No allow list, no start.** Quadstack refuses to start unless `QUADSTACK_ALLOWED_USERS` or `QUADSTACK_ALLOWED_GROUPS` is set. (`LoadConfig`)
2. **Only verified identities.** An email counts only with `email_verified`; a value containing `@` never matches a username; groups from userinfo count only for the ID token's subject. (`allowed`, `sameSubject`; `TestAllowedVerifiedEmail`, `TestSignIn`)
3. **The sign-in flow cannot be replayed or redirected.** State, nonce and PKCE are checked; after sign-in, only local paths are followed. (`HandleCallback`, `safeNext`; `TestSignIn`, `TestSafeNext`)
4. **Changing requests need the session cookie and `X-Quadstack: 1`;** websockets accept only the public URL's origin. (`requireSession`, `originPatterns`; `TestAPI`)

## Files

5. **Nothing outside the Quadlet directory.** Client paths are cleaned and must name a Quadlet, drop-in or `.env` file; hidden paths are refused. Reads and writes never follow a symlinked file or folder, in the web UI and over MCP alike. (`cleanRel`, `relParam`, `Files.parent`, `readAt`; `TestCleanRel`, `TestMCPRejectsSymlinks`, `TestSymlinksAreReportedNotManaged`)
6. **Nothing is lost without a trace.** A change, move or delete first keeps the previous version; if that fails, the change does not happen. Only an explicit **Forget** removes versions. (`Files.Write`, `Delete`, `Move`; `TestHistory`, `TestMoveKeepsHistory`, `TestDeletedAndForget`)
7. **Validation fails closed.** A file Podman rejects is not saved unless a signed-in person insists; MCP cannot insist. When the generator cannot give a verdict, the check fails. (`Validate`; `TestValidateFailsClosed`, `TestValidateWithQuadlet`)

## Actions

8. **An outcome is never assumed.** A timeout or lost connection is reported as such, not as success, and a mutating request is never retried automatically. A later step does not run after an earlier one failed where it depends on it (no restart after a failed reload, no delete after a failed stop). (`operations.go`, `systemd.go`, `podman_api.go`; `TestOperations*`, `TestSystemd*`, `TestPodmanMutationsAreNotRetried`, `TestDeleteRefusesAfterStopFailure`)
9. **A shared network fails closed.** A container that joins another container's network namespace (`Network=container:<name>` or `Network=<file>.container`, typically a VPN container) has no network of its own. Quadstack stops it before that container is stopped or restarted, starts that container first, and does not start it when that container failed to start or restart. Quadstack never rewrites `Network=`. That it cannot start without the namespace is Podman's guarantee, which Quadstack relies on and does not replace: `podman run --network container:<name>` fails when no such container exists. (`linkNetns`, `planUnits`, `unitTasks`; `TestNetnsEdges`, `TestPlanUnits`, `TestSharedNetworkActions`, `TestDependentsNotStartedAfterParentFailure`)

## AI access (MCP)

10. **Off by default, always ending.** No grant, no access. A grant always has an end of at most `QUADSTACK_MCP_MAX_HOURS`; `0` turns MCP off. (`readGrantReq`, `mcpEndpoint`; `TestMCP`)
11. **Tokens are shown once and stored as a SHA-256 hash.** Deleting a grant ends its token for good. (`Grants.Create`, `ByToken`; `TestMCP`)
12. **Rules apply at once.** Every call reads the grant again, and a queued action is checked again before it runs. (`add`, `operationCheckKey`; `TestMCP`, `TestQueuedOperationRechecksAuthorization`)
13. **Every stack a call touches must be allowed:** the file's stack before and after a change, and the stack of every unit and container. What belongs to no single stack, such as a unit two stacks define, needs **All stacks**. (`checkWrite`, `scopeFor`, `scopeOfUnit`; `TestMCP`, `TestMCPUnitCollisionsAndAudit`)
14. **No audit record, no call.** A call is recorded before it runs and refused if that fails. Logs keep arguments, but file contents only as size and hash, and never secret values. (`add`, `Audit.Write`, `auditArgs`; `TestMCPUnitCollisionsAndAudit`, `TestAuditListsUnfinishedCalls`)

## Secrets and logs

15. **Secret values are never returned or logged.** They travel only in the body of the local Podman API request. (`SetSecret`, `secretList`, `auditArgs`; `TestPodmanSecretWrites`)
16. **Quadstack logs no secrets or bodies.**

## Privileges and supply chain

17. **No root, no extra rights.** Quadstack runs as the Quadlet account, talks to Podman only over the local Unix socket, and its unit is sandboxed as far as a user unit allows. (`deploy/quadstack.service`, `LoadConfig`; `TestPodmanSocketConfiguration`)
18. **Pinned and verified.** Go modules and GitHub Actions are pinned; the installer checks `SHA256SUMS`, releases carry a build provenance attestation, and the community-scripts engine is pinned to a tested commit.
19. **A preview is always explicit.** New installations default to stable releases. Preview builds are prereleases and never become GitHub's latest stable release. Published release tags and assets are never replaced. Only fully validated main builds can publish, after the required `test` check passes; pull requests cannot publish. (`.github/workflows/ci.yml`, `ci/release.py`, `resolve_version`; release and installer tests)
