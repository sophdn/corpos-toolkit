// Package sys implements the owned system meta-tool — gated command execution
// plus read-only introspection of live host state (processes, listening ports,
// systemd-user units, containers). It is the owned, substrate-native counterpart
// to the harness Bash tool's shell-and-introspection half: the read-only half is
// ungated (observation is safe), and arbitrary execution is gated behind an
// allowlist + required rationale + dispatch policy.
//
// ## Intended use
//
// **Workflow served:** agents need to run commands (git / build / run) and to
// observe what is happening on the host — which processes exist, what is
// listening on which ports, which systemd-user units and containers are up. The
// harness ships a single all-powerful Bash tool for this; owning it lets the
// dangerous capability (arbitrary exec) be gated and optionally sandboxed, while
// the safe capability (introspection) is structured, ungated, and substrate-native.
//
// **Invocation pattern:** dispatched via the sys meta-tool; each action takes a
// typed params struct and returns a named result struct. The exec action runs a
// shell command through a model-agnostic Runner (working dir, environment,
// timeout, combined output capture, exit code, working-dir persistence, output
// truncation — see testdata/parity/EXEC_CONTRACT.md) with an optional pluggable
// OS sandbox (bwrap or rootless podman). Introspection actions shell out to the
// host's standard tools and parse their output into structured rows.
//
// **Success shape:** a JSON object matching the action's named result struct —
// e.g. RunResult.Output carries the combined, truncation-capped command output
// with its exit code; the introspection actions carry typed row slices.
//
// **Non-goals:** the runner does not implement a command permission engine — the
// allowlist + rationale gate lives in the exec action and dispatch policy, not
// here; introspection never mutates host state; this surface does not own task /
// bug / vault state (see internal/work, internal/knowledge) or the filesystem
// read/search family (see internal/fs).
package sys
