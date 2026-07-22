# sys.exec — command-execution contract

The behavioral contract the owned exec **runner** implements. It is pinned by the
characterization net in `../../exec_runner_test.go` and the sandbox net in
`../../sandbox_test.go`. This is our own specification, restated in our own terms;
it is the model-agnostic parity floor for the gated `sys.exec` action (the
allowlist + rationale gate is layered on top in the action, not here — the runner
is the mechanism, the action is the policy).

The runner is **model-agnostic**: behavior intrinsic to running a command is
ported (working directory, environment, timeout, output capture, exit code,
working-dir persistence, output truncation); behavior coupled to a specific
agent harness is dropped (harness-injected env vars, image-data-URI capture, a
host-specific permission engine). Those carve-outs are called out below.

---

## Invocation model

A command is a single shell command string, run via the system shell as
`<shell> -c <command>`. The shell is resolved once per runner:

1. `$SHELL` if it names a `bash` or `zsh` and is executable;
2. else `/bin/bash` if executable;
3. else `/bin/sh`.

The runner is **stateful in one dimension only**: the working directory. A
fresh runner starts at a caller-supplied directory (default: the process CWD).

## Working directory & persistence

- Each `Run` executes in the runner's current working directory unless the call
  supplies an explicit per-call `Cwd`, which overrides it for that call only.
- **Persistence:** a `cd` performed *inside* a command that runs in the
  persistent directory carries forward to subsequent calls. This is implemented
  by capturing the shell's final `pwd` after the command and adopting it as the
  new current directory. (`Run("cd sub")` then `Run("pwd")` reports `…/sub`.)
- A per-call `Cwd` override is a **one-shot detour**: it never mutates the
  persistent directory, even if the command `cd`s within the detour. (Likewise a
  sandboxed run is per-invocation and carries no persistence.)
- **Recovery:** if the current directory no longer exists on disk at call time
  (e.g. a previous command deleted it), the runner falls back to the runner's
  origin directory rather than failing to spawn.

## Environment

- The child inherits the parent process environment, then per-call `Env` entries
  are applied on top (override-or-add). No harness-specific variables are
  injected — this is the model-agnostic carve-out (a specific agent's
  `CLAUDECODE` / `GIT_EDITOR` / session-id injection is **not** ported).

## Timeout

- `TimeoutMS` bounds wall-clock time. Default **120000** (2 min) when absent or
  `<= 0`; clamped to a maximum of **600000** (10 min).
- On timeout the command's entire process group is killed (SIGKILL to the group,
  so children spawned by the command die too). The result is marked `timed_out`,
  carries whatever output was produced before the kill, and reports a non-zero
  exit code.

## Output capture

- stdout and stderr are captured **combined and interleaved chronologically**
  into a single stream (they share one sink, so ordering reflects write order,
  not stream).
- **Blank-line trim:** leading and trailing lines that contain only whitespace
  are stripped from the combined output (interior blank lines are preserved).
- **Truncation:** after trimming, if the output exceeds `MaxOutputChars`
  (default **30000**, clamped to a maximum of **150000**) it is truncated to the
  first `MaxOutputChars` characters followed by `"\n\n... [<n> lines truncated]
  ..."`, where `<n>` is the number of newline-delimited lines in the discarded
  tail (counted from the cut point), plus one. `truncated` is set true.
- A model-coupled output token cap is intentionally **not** part of the contract
  (model-agnostic surface ⇒ the char cap is the only size guard).

| combined output (after trim) | MaxOutputChars | result Output                                   | truncated |
|-------------------------------|----------------|--------------------------------------------------|-----------|
| `hi`                          | 30000          | `hi`                                             | false     |
| 40000 chars, 800 lines        | 30000          | first 30000 chars + `\n\n... [<n> lines truncated] ...` | true |
| `\n\n  \nreal\n  \n\n`        | 30000          | `real`                                           | false     |

## Exit code

- `exit_code` is the command's exit status: `0` on success, the process exit
  code on failure, `124` on timeout (the conventional timeout exit code), and
  `-1` when the process could not be started or was terminated by a signal with
  no code.

## Sandboxing (opt-in, defense-in-depth)

The runner can wrap the command in an OS-level sandbox via a pluggable
`SandboxProvider`. Sandboxing is **off by default** (parity floor = a plain
child process); a caller opts in by selecting a backend.

- **none** — no isolation (the default).
- **bwrap** — bubblewrap: read-only bind of the host root, a writable tmpfs on
  `/tmp`, a private `/proc` + `/dev`, the working directory bound writable, and
  PID/IPC/UTS namespaces unshared. Lightweight, shares the host. **Availability
  is probed at runtime** — on hosts where unprivileged user namespaces are
  AppArmor-restricted (Ubuntu 23.10+ default), bwrap cannot start until an
  AppArmor profile granting it `userns` is loaded; the runner reports the
  backend unavailable rather than silently running unsandboxed.
- **podman** — rootless container: `--rm --read-only` with the working directory
  bind-mounted writable and a tmpfs `/tmp`. Heavier (per-call container + base
  image), full isolation, no host privilege required.

A sandboxed command observes the **same** stdout/stderr/exit-code/timeout
contract as an unsandboxed one; only the filesystem/namespace visibility
changes. Selecting an unavailable backend is an error (the call fails closed —
it never downgrades to unsandboxed).

## Deliberately out of contract (model-agnostic / harness carve-outs)

- Harness-injected environment variables (a specific agent's session vars).
- Capturing an image data-URI from stdout and returning it as an image block.
- A host-specific shell-command permission/allow engine — the owned action
  replaces that with the allowlist + rationale + dispatch-policy gate (the
  `sys.exec` action layer), not the runner.
- A model token cap on output (byte/char cap is the only size guard).
