# Owned exec / introspection — the security model

How the `sys` surface (chain `owned-exec-shell-surface`) splits a dangerous
capability from a safe one, and what gates the dangerous half. This is the
documented security split the chain's completion condition requires.

## The read-only-vs-gated-exec split (the spine)

The `sys` surface owns two kinds of capability, on opposite sides of a gate:

| Capability | Actions | Gate | Why |
|---|---|---|---|
| **Read-only introspection** | `sys.ps`, `sys.ports`, `sys.units`, `sys.containers` | **none** (ungated) | Observation cannot mutate host state. Returning structured rows about processes / ports / units / containers is as safe as reading a file, so it carries no rationale requirement. |
| **Command execution** | `sys.exec` | **allowlist + rationale + (optional) sandbox** | Arbitrary command execution is the dangerous half — it can mutate anything the process can. It is gated on every axis below. |

A model that only needs to *observe* (the launch-inventory workflow: "what's
listening and which units/containers own it") never touches the gated path.

## The exec gate — three layers

### 1. Rationale (dispatch-policy, enforced at the dispatch boundary)

`action-manifests/dispatch-policy.toml` carries `[sys.exec] requires_rationale =
true`. When the actor is an **agent**, the dispatcher rejects a `sys.exec` call
whose top-level `rationale` is empty, whitespace, under 6 chars, or boilerplate —
before the handler runs. So every agent-issued command lands a *why* in the
events ledger. (Human/system actors are not subject to the boilerplate/min-length
check, per the existing policy convention.) This is the same gate the mutating
`work`/`knowledge`/`admin` actions use.

### 2. Allowlist (enforced inside the handler)

The command is admitted only if **every command head is allowlisted** and the
command contains **no command substitution**:

- The command is split on the shell operators `; | &`; each segment's head is the
  basename of its first real token (leading `VAR=value` assignments skipped).
- Every head must be in the allowlist: a built-in default set
  (`git`, `go`, `make`, `ls`, `grep`, `rg`, `podman`, … — see
  `defaultExecAllowlist` in `go/internal/sys/exec_action.go`) plus any entries
  added via the **`TOOLKIT_EXEC_ALLOWLIST`** environment variable
  (comma-separated). The allowlist is **never** a per-call parameter — security
  must not be caller-controllable.
- Shells and eval-style binaries (`sh`, `bash`, `zsh`, `env`, `eval`, `exec`) are
  deliberately **excluded** from the default — allowing them would let a model
  run anything through the gate (`bash -c '…'`).
- Command substitution (`$(…)` or backticks) is **rejected** outright, because it
  can smuggle a disallowed command past the head checks (`git $(rm -rf x)`).

This is a deliberately conservative v1. It is **not** a full shell-security engine
(the harness Bash tool ships ~100 KB of command-permission logic); it trades
expressiveness for a small, auditable rule set, leaning on the sandbox (below) for
defense-in-depth.

**Known limitation — the allowlist gates the binary, not its effects.** A head
check admits a command by *which executable* runs, not *what it does*. An
allowlisted command can still mutate the filesystem through shell **redirection**
(`echo payload > ~/.bashrc`) or by acting on dangerous arguments
(`git config --global …`), within whatever the runner's user can write. The
allowlist's job is to stop the model from invoking the *wrong tool* (and to land
a rationale in the ledger), not to sandbox a *trusted tool used badly*. For
commands whose input is not trusted, run with **`sandbox=bwrap`** — the read-only
host root turns those writes into errors. Treat the allowlist + rationale as the
intent gate and the sandbox as the containment boundary; they are complementary,
not redundant.

### 3. Sandbox (optional OS isolation, defense-in-depth)

`sys.exec` accepts a `sandbox` param selecting a runtime-probed backend:

- **`none`** (default) — a plain child process.
- **`bwrap`** — bubblewrap: read-only host root, writable working dir + tmpfs
  `/tmp`, private `/proc`+`/dev`, unshared PID/IPC/UTS namespaces. Because it
  binds the host root read-only, **the allowlisted host toolchains
  (`git`/`go`/`make`/…) are visible inside the sandbox** — so bwrap is the
  practical backend for sandboxing the kinds of commands this surface allows.
- **`podman`** — rootless container, read-only rootfs, working dir bind-mounted.
  Runs in a minimal base image (busybox), so **host toolchains are NOT visible**
  inside it (`git` is absent). podman-isolation is therefore for self-contained
  scripts / shell built-ins, not for running the host dev tools — use bwrap for
  those. (Verified: `podman run busybox … git` → not found.)

Selecting an **unavailable** backend **fails closed** (the call errors) rather
than silently running unsandboxed.

**bwrap availability caveat (Ubuntu 23.10+):** `kernel.apparmor_restrict_
unprivileged_userns=1` blocks bwrap from creating a user namespace until an
AppArmor profile granting it `userns` is loaded. The fix is to load an AppArmor
profile granting bwrap `userns` — **not** to disable the sysctl. This repo ships
the profile at **`docs/bwrap-userns.apparmor`** (the chrome/flatpak `userns`
shape, scoped to `/usr/bin/bwrap`); install it once per host:

```
sudo install -m0644 docs/bwrap-userns.apparmor /etc/apparmor.d/bwrap
sudo apparmor_parser -r /etc/apparmor.d/bwrap
```

(The official `bwrap-userns-restrict` profile from the `apparmor-profiles` package
is a more-hardened alternative — it also blocks *nested* userns inside the
sandbox.) **Status:** loaded + verified on the primary host 2026-06-01
(`sys.exec sandbox=bwrap` runs `git --version` with a read-only root); reapply on
the mini-PC with the command above.

Rootless **podman** needs no such profile (it maps uids via the setuid
`newuidmap`/`newgidmap` helpers). Full background:
`~/.claude/vault/reference/2026-06-01_owned-exec-sandbox-bwrap-userns-ubuntu-2404.md`.

## The runner (model-agnostic mechanics)

Underneath the gate, the command runs through a model-agnostic runner
(`go/internal/sys/exec_runner.go`, contract in
`go/internal/sys/testdata/EXEC_CONTRACT.md`): working directory (persistent across
calls), inherited+override environment, timeout (default 2 min, max 10 min, whole
process group killed on timeout), combined stdout/stderr capture with blank-edge
trim + character-cap truncation, and exit code. Harness-coupled behaviors
(session env injection, image-data-URI capture, the host permission engine) are
deliberately not ported.

## Relationship to the harness Bash deny

Owning `sys.exec` is the prerequisite for eventually denying the harness `Bash`
tool on this repo (a global, path-scoped `permissions.deny` rule). That deny is a
deliberate, user-only, **later** step (the agent cannot revert its own deny), and
is tracked separately — it is **not** landed by this chain.
