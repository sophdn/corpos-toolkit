# sys introspection — read-only contract (self-defined)

The read-only introspection actions of the sys surface — `sys.ps`, `sys.ports`,
`sys.units`, `sys.containers`. These are **self-defined**: the harness has no
introspection tools (it shells out via Bash), so there is no source oracle.
Their contracts are designed from first principles to return **structured rows**
rather than raw shell text, pinned by the nets in `../*_test.go`.

All four are **read-only and ungated** — observation cannot mutate host state, so
the read-only/gated security split (see the chain's exec gate) puts them on the
ungated side. Each is built as a **sans-IO pure parser** (unit-tested against
captured real tool output) plus a thin command runner that fails soft when the
underlying tool is absent.

---

## sys.ps — processes (parsed from /proc)

Enumerates processes by reading `/proc` directly (no `ps` dependency).

- Params: `contains` (optional substring filter on the command), `limit`
  (optional cap; 0 = all).
- Row (`ProcessRow`): `pid`, `ppid`, `state` (single-letter Linux process state),
  `command` (full cmdline, space-joined; the `[comm]` bracket form for kernel
  threads with an empty cmdline), `rss_kb` (resident set size in KiB).
- `/proc/<pid>/stat` is parsed by splitting off `pid (comm)` at the **last** `)`
  (comm may contain spaces and parens), then reading state, ppid, and the rss
  page count from the fixed-offset remainder; `rss_kb = rss_pages × pagesize/1024`.

## sys.ports — listening sockets (`ss -tulnpH`)

Lists listening TCP and bound UDP sockets, ss-equivalent.

- Params: `proto` (optional `tcp`|`udp` filter).
- Row (`PortRow`): `proto`, `state`, `local_addr`, `local_port`, `pid`,
  `process`. The owning pid/process is extracted from ss's
  `users:(("<name>",pid=<n>,fd=<m>))` column when present (own sockets; other
  users' sockets show an empty owner without root).
- `local_addr`/`local_port` split the `address:port` column on its **last** `:`
  (so IPv6 `[::]:8080` and scoped `127.0.0.53%lo:53` parse correctly).

## sys.units — systemd-user units (`systemctl --user … -o json`)

Lists systemd **user** units.

- Params: `type` (unit type, default `service`), `active_only` (default false →
  include inactive units via `--all`).
- Row (`UnitRow`): `unit`, `load`, `active`, `sub`, `description` — unmarshaled
  directly from systemctl's JSON output.

## sys.containers — containers (podman + docker)

Lists containers from both runtimes, each row tagged with its `runtime`.

- Params: `running_only` (default false → include stopped containers, `-a`).
- Row (`ContainerRow`): `runtime` (`podman`|`docker`), `id` (short, 12 chars),
  `image`, `names` (comma-joined), `state`, `status`, `ports`.
- podman ps `--format json` is a JSON array (Names is an array, Ports an array of
  mapping objects rendered as `host:hostport->containerport/proto`); docker ps
  `--format '{{json .}}'` is newline-delimited objects (Names/Ports are strings).
  Each runtime is probed; an absent runtime contributes no rows (fail-soft), and
  the result notes which runtimes were queried.

## Errors / fail-soft

- A missing underlying tool (`ss`, `systemctl`, `podman`, `docker`) is **not** a
  hard error where another source can still contribute (containers) or where the
  surface should degrade gracefully; `sys.ports`/`sys.units` return an error only
  when their single required tool is absent. Parsing is tolerant: a malformed row
  is skipped, not fatal.
