# Quadlet units — containerized toolkit-server (rootless, systemd-user)

These run the **toolkit-server backend** as a rootless-Podman container managed by
systemd user units, via Quadlet (`.container`/`.network`/`.volume` — the supported
mechanism; **not** the deprecated `podman generate systemd`). Part of Phase C
(chain `corpos-podman-deploy`); coordinates with chain 315 (`auto-startup-dev-services`).

## Files (canonical here; installed into `~/.config/containers/systemd/`)
- `corpos.network` — shared net `corpos-net` (netavark+aardvark DNS) for container-to-container MCP.
- `toolkit-server-data.volume` — the container's OWN DB volume `tks-data` (DB strategy c).
- `toolkit-server.container` — the backend daemon; host `:3001` → container `:3000`.

## Install
```sh
scripts/install-quadlet-units.sh          # copy units + daemon-reload (+ try enable-linger)
scripts/install-quadlet-units.sh --start  # also start toolkit-server.service
# or manually:
cp deploy/quadlet/*.{network,volume,container} ~/.config/containers/systemd/
systemctl --user daemon-reload
systemctl --user start toolkit-server.service
```
`daemon-reload` makes Quadlet generate `toolkit-server.service` (+ the network/volume
services). **Linger** (`loginctl enable-linger $USER`, may need sudo) is required only
for boot-time / post-logout start; `systemctl --user start` works without it.

Verify:
```sh
curl -s http://localhost:3001/healthz   # {"status":"ok"}
curl -s http://localhost:3001/version   # {"git_sha":"<sha>",...}
systemctl --user status toolkit-server.service
```

## image-lags-HEAD (the deliberate stability boundary — chain req 3)
The unit runs `localhost/toolkit-server:dev` **as built**. Editing repo source does
NOT change the running container. Advance it explicitly:
```sh
scripts/build-toolkit-image.sh && systemctl --user restart toolkit-server.service
```
This is the point of containerizing the surface: served behavior changes only on an
explicit image rebuild + restart, never mid-session.

## CLI vs daemon
- **toolkit-server = daemon** → this Quadlet unit (long-running).
- **corpos = CLI (oneshot)** → deliberately NO unit. It runs a turn and exits; daemonizing
  it waits for the interactive REPL/TUI (Phase F). Run it on demand:

```sh
# Anthropic Haiku, pure container-to-container over corpos-net DNS.
# Key via a podman SECRET (type=env) — never baked, never in argv:
podman secret create corpos-anthropic-key - <<<"$ANTHROPIC_API_KEY"   # once; or from the key file
podman run --rm --network corpos-net \
  --secret corpos-anthropic-key,type=env,target=ANTHROPIC_API_KEY \
  localhost/corpos:dev -provider anthropic \
  -mcp-url http://toolkit-server:3000 \
  -prompt "Use the work surface to list open chains and say how many."

# Local Qwen: llama is bound to 127.0.0.1:8081 (host loopback), unreachable from
# the bridge — use --network=host until llama is reachable from corpos-net
# (suggestion: make-local-llama-reachable-from-corpos-net-rebind-or-containerize):
podman run --rm --network=host localhost/corpos:dev \
  -mcp-url http://localhost:3001 -model-url http://localhost:8081/v1 \
  -prompt "Call the work surface action chain_status with {} and report the count."
```

## Post-commit advisor reconciliation
The mcp-servers post-commit advisor rebuilds + restarts the **native :3000** daemon.
The containerized **:3001** surface is image-lags-HEAD by design, so the advisor must
NOT auto-restart it — and it doesn't (it targets the native binary/daemon, not this
unit). No advisor change is needed while we're in the coexist/validation phase (native
:3000 canonical). **When the containerized surface is flipped to canonical**, the
advisor's "rebuild binary + restart :3000" step is replaced by "rebuild image +
`systemctl --user restart toolkit-server.service`" — track that flip on the chain; do
not change the advisor preemptively.

## Teardown
```sh
systemctl --user stop toolkit-server.service
rm ~/.config/containers/systemd/{corpos.network,toolkit-server-data.volume,toolkit-server.container}
systemctl --user daemon-reload
podman volume rm tks-data    # discard the container's DB (native :3000 ledger is untouched)
```
