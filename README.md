# imcodex

`imcodex` bridges a Lark, Feishu, or Telegram group chat to long-lived Codex
sessions running in `tmux`.

## Runtime Model

`v2.2` supports two Codex runtimes:

- `host-codex`
  This is now the default runtime.
- `docker-codex`
  This is opt-in and preferred when you want a pinned, isolated Docker-based
  Codex environment.

`docker-codex` no longer needs `runtime`, `runtime_config_dir`, or
`session_command` in YAML. `imcodex` now manages the Docker launcher
internally and auto-builds the local `stable` image when needed unless you
override it with a custom Docker image.

## Requirements

Always required:

- `tmux`
- a Lark / Feishu bot or a Telegram bot

Required for the default `host-codex` runtime:

- `nodejs`
- `npm`
- `@openai/codex`
- `bubblewrap`

Required only for `--runtime docker-codex`:

- `docker`
- the runtime user must be able to run `docker` without `sudo`

## Install On Ubuntu

Base install for the default host runtime:

```bash
sudo apt update
sudo apt install -y tmux bubblewrap nodejs npm
sudo npm install -g @openai/codex
codex --version
codex login
```

If you also want the optional Docker runtime:

```bash
sudo apt install -y docker.io
sudo usermod -aG docker "$USER"
```

Log out and log back in so the `docker` group change takes effect, then verify:

```bash
docker --version
docker run --rm hello-world
tmux -V
```

## Build

```bash
go build -o imcodex .
```

To build the GitHub release artifacts locally:

```bash
make release
```

That writes standalone versioned binaries plus a checksum file to
`build/release/`. Release assets are not tar-packed, so downloading a binary
does not risk changing the extraction directory's permissions.

## Configuration

If `-config` is omitted, `imcodex` looks for config files in this order:

1. `./imcodex.yaml`
2. `~/.imcodex.yaml`

See [config.example.yaml](config.example.yaml).

Key fields:

| Field | Meaning |
| --- | --- |
| `platform` | `lark` or `telegram` |
| `docker_image` | Optional custom image for `docker-codex`; when set, `imcodex` runs that image directly instead of rebuilding the managed local `stable` image |
| `interrupt_on_new_message` | If `true`, a new group message interrupts the current main-session run and keeps only the newest pending message |
| `groups[].group_id` | Group ID or Telegram chat ID |
| `groups[].cwd` | Working directory mapped to that group |
| `groups[].session_name` | Optional override for the tmux session name |
| `groups[].jobs[].name` | Job name shown in job result posts |
| `groups[].jobs[].schedule` | Standard 5-field cron expression |
| `groups[].jobs[].prompt_file` | Markdown prompt file for agent-driven jobs; relative paths resolve from the config file directory |
| `groups[].jobs[].command` | Shell command for deterministic CLI jobs; runs in `cwd` |
| `groups[].jobs[].artifacts_dir` | Optional base dir for per-run logs; relative paths resolve from `cwd` |
| `groups[].jobs[].summary_file` | Optional file whose content is posted on success; relative paths resolve from `cwd` |
| `groups[].jobs[].session_name` | Optional override for a `prompt_file` job session name |

Each job must set exactly one of `prompt_file` or `command`.

Path fields support:

- absolute paths
- `~/...`
- `$HOME/...`
- `${HOME}/...`

## Run

Before starting `imcodex` or any unattended `codex` pane, disable Codex's
interactive update notifier in your login shell so sessions do not self-upgrade
mid-run:

```bash
echo 'export NO_UPDATE_NOTIFIER=1' >> ~/.zshrc
```

Restart the shell, or run `export NO_UPDATE_NOTIFIER=1` in the current shell,
then start `imcodex`. The managed Codex launcher also defaults this variable to
`1` as a safety net.

Default host runtime:

```bash
./imcodex -config /srv/imcodex/imcodex.yaml
```

Explicit host runtime:

```bash
./imcodex -config /srv/imcodex/imcodex.yaml --runtime host-codex
```

Optional Docker runtime:

```bash
./imcodex -config /srv/imcodex/imcodex.yaml --runtime docker-codex
```

Docker runtime with a non-default Codex config directory:

```bash
./imcodex -config /srv/imcodex/imcodex.yaml --runtime docker-codex --codex-config-dir ~/.codex
```

Docker runtime with a custom prebuilt image:

```bash
./imcodex -config /srv/imcodex/imcodex.yaml --runtime docker-codex
```

```yaml
docker_image: ghcr.io/acme/imcodex-go:1.24
```

## Docker Runtime Behavior

When `imcodex` runs in `docker-codex` mode:

- it uses the current `imcodex` binary itself as the tmux pane launcher
- if `docker_image` is unset, it ensures a local image tagged `imcodex-codex:stable` exists
- if that managed image is missing or stale, it rebuilds it automatically
- if `docker_image` is set, it runs that prebuilt image directly and skips managed-image rebuild checks
- it mounts only the configured group `cwd` into the container as `/workspace`
- it copies the host Codex config directory into container-local `/home/agent/.codex`
- it first tries `codex resume --last`
- if that resume attempt exits non-zero, it falls back to a fresh Codex session with:

```bash
codex -a never -s danger-full-access --no-alt-screen -C /workspace
```

The pinned Docker Codex CLI version is `0.120.0`.

If you want to prebuild the same image manually:

```bash
docker build \
  --build-arg CODEX_VERSION=0.120.0 \
  --build-arg IMCODEX_IMAGE_REVISION=2.2.17 \
  -t imcodex-codex:stable \
  -f tools/runtime/Dockerfile.codex .
```

Custom images should provide the same runtime contract:

- `sh`
- `codex`
- `su-exec` or an equivalent privilege-drop wrapper
- writable `/home/agent`
- `/workspace` as the mounted workspace path

## Runtime Caveat

`host-codex` is now the default because it matches local toolchains more
directly and avoids forcing everyone through Docker.

If you need a pinned, isolated Codex CLI for unattended use, prefer explicit
`--runtime docker-codex`.

When Codex changes model metadata, for example from `gpt-5.4` to `gpt-5.5`, an
old resumed pane may stop at the stale session prompt. Update or pin a Codex CLI
version that knows the model, then start a fresh Codex session with `/new` (or
reset the tmux session) before putting the group back under unattended traffic.

## Compatibility

`v2.2` removes these YAML fields:

- `runtime`
- `runtime_config_dir`
- `session_command`

If they still appear in config, `imcodex` now fails fast with a migration
error instead of silently mixing old and new runtime behavior.

Existing message routing, buffered Telegram output handling, scheduled jobs,
and `tmux` session reuse continue to work the same way.

## Message Delivery

`v2.2.17` keeps host runtime as the default and further hardens Telegram delivery
behavior without changing the public config
surface:

- outbound send/edit/delete/chat-action calls now use bounded request timeouts
- tmux output capture now polls at 100ms by default while visible body sync is
  paced independently at a 1-second default
- detached plain output remains available for non-editable transports, but
  Telegram backpressure no longer creates a detached catch-up backlog
- host runtime now waits for an actual Codex prompt before the first input is
  pasted into a tmux session
- editable or detached `429`, delivery timeout, and oversized run output now
  drop the current run body instead of retrying, writing spill files, or later
  replaying a backlog
- detached delivery tracks per-run observed baselines so pane reset/rewrite
  jitter cannot enqueue the same already-observed body again before a safety
  drop is triggered
- editable reply sync no longer bypasses the normal edit throttle on every
  busy-to-idle transition
- watchdog retries no longer rewrite an editable body into plain detached body
  sends
- recovery after `429` means the next run starts cleanly; dropped body text is
  intentionally not replayed
- Telegram/Lark attachments are downloaded with a bounded body-size limit before
  being written to `.imcodex/inbox`
- delivery tracing now logs why buffered output is waiting, blocked, or
  committed

Current operator-facing behavior is documented in
[docs/telegram-output-buffering.md](docs/telegram-output-buffering.md).

The longer-term simplification plan remains in
[docs/message-delivery-redesign.md](docs/message-delivery-redesign.md).

## Runtime Docs

More detailed runtime notes:

- [docs/runtime-v2-docker-tmux.md](docs/runtime-v2-docker-tmux.md)
- [docs/runtime-v2-examples.md](docs/runtime-v2-examples.md)
- [docs/telegram-output-buffering.md](docs/telegram-output-buffering.md)
- [docs/message-delivery-redesign.md](docs/message-delivery-redesign.md)

## Example Startup Log

```text
imcodex 2.2.17 started: config=/srv/imcodex/imcodex.yaml platform=telegram runtime=host-codex groups=1 jobs=1 base=https://api.telegram.org
```
