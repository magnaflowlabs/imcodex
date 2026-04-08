# imcodex

`imcodex` bridges a Lark, Feishu, or Telegram group chat to long-lived agent
sessions running inside `tmux`.

## Model

| Item | Behavior |
| --- | --- |
| Main chat session | One group = one `cwd` = one persistent agent session |
| Scheduled jobs | Declared in YAML; each job is either a `prompt_file` agent session or a `command` shell run |
| Job visibility | Jobs post final results or failures back to the same group |
| Context isolation | The main chat session and job sessions do not share Codex context |
| Reconfiguration | Edit YAML and prompt `.md` files, then restart `imcodex` |

## Requirements

| Requirement | Notes |
| --- | --- |
| `tmux` | Required at runtime |
| Agent runtime | Default path: host `codex`; optional enhancement: Docker `codex` or `claude` |
| Docker | Optional; only needed for `docker-*` runtime |
| Go 1.24+ | Needed only for local builds |
| Lark / Feishu bot or Telegram bot | Pick one backend |

## Install

### Host Prerequisites

macOS:

```bash
brew install tmux
brew install --cask docker
```

Ubuntu 24.04:

```bash
sudo apt update
sudo apt install -y tmux docker.io
```

Verify:

```bash
tmux -V
docker --version
```

### Default Runtime: Host Codex

If you want the old 1.x behavior, install Codex on the host:

```bash
npm install -g @openai/codex
codex login
codex --version
```

Host Codex mode is still supported in 2.x. If both `runtime` and
`session_command` are omitted from the config, `imcodex` launches `codex`
directly in `tmux`, which is the same compatibility path as 1.x.

If Codex asks for `npm install -g @openai/codex@latest` during startup, do not
let the live runtime self-upgrade in the middle of production traffic. Upgrade
it during a maintenance window instead, or switch the deployment to a pinned
Docker runtime.

### Optional Runtime: Docker Sandbox

If you want workspace-only isolation, install Docker and the wrapper script in
`PATH`, then use the higher-level `runtime:` shortcut.

Build the example images shipped in this repo:

```bash
docker build --build-arg CODEX_VERSION=0.118.0 -t imcodex-agent-codex:0.118.0 -f tools/runtime/Dockerfile.codex .
docker build --build-arg CLAUDE_CODE_VERSION=2.1.92 -t imcodex-agent-claude:2.1.92 -f tools/runtime/Dockerfile.claude .
```

This path keeps the agent confined to one workspace mount instead of running
directly on the host.

Recommended update policy:

- do not use `latest` in long-lived runtime images
- pin Codex and Claude Code to known-good versions
- rebuild and roll out images only during a maintenance window
- treat CLI upgrades as an explicit release step, not something the live agent
  decides during startup

If you want the runtime to stop getting interrupted by upgrade prompts, prefer
this Docker mode over host-installed CLIs.

### Optional Host Install: Claude Code

If you want to run Claude on the host instead of inside Docker:

```bash
npm install -g @anthropic-ai/claude-code
claude
claude --version
```

The 2.0 runtime design still recommends running Claude inside Docker rather
than on the host.

If you do run Claude on the host and want to stop startup auto-update prompts,
Anthropic documents two supported options:

```bash
claude config set autoUpdates false --global
export DISABLE_AUTOUPDATER=1
```

## Runtime 2.0 Quickstart

The Docker shortcut topology is:

- host `imcodex`
- host `tmux`
- host `docker`
- containerized `codex` or `claude`
- one mounted workspace per group session

The repo includes a host wrapper script for this model:

```bash
chmod +x tools/runtime/imcodex-agent-run
bash -n tools/runtime/imcodex-agent-run
```

The shipped wrapper defaults to pinned image tags:

- `imcodex-agent-codex:0.118.0`
- `imcodex-agent-claude:2.1.92`

Install it somewhere in `PATH`, for example:

```bash
install -m 0755 tools/runtime/imcodex-agent-run /usr/local/bin/imcodex-agent-run
```

Codex group session example:

```yaml
platform: telegram
telegram_bot_token: 123456:ABCDEF
runtime: docker-codex

groups:
  - group_id: -1001234567890
    cwd: /srv/my-project
```

Claude group session example:

```yaml
platform: telegram
telegram_bot_token: 123456:ABCDEF
runtime: docker-claude

groups:
  - group_id: -1001234567890
    cwd: /srv/my-project
```

If you already have host config you want to reuse without mounting your whole
home directory, use the optional read-only config seed:

```yaml
runtime: docker-codex
runtime_config_dir: ~/.codex
```

Key properties of this model:

- `tmux` stays on the host so sessions remain easy to inspect
- the agent process runs inside Docker
- only the configured `cwd` is mounted into `/workspace`
- an optional read-only config dir can be copied into the container-local home
- the host home directory is not mounted by default
- existing 1.x configs continue to work if both `runtime` and `session_command` are omitted

## Compatibility

`v2.1.1` keeps the old config behavior:

- if both `runtime` and `session_command` are omitted, `imcodex` uses the legacy host-side `codex`
  launch path
- existing 1.x configs without any 2.0 runtime fields remain valid
- adding `runtime: docker-*` is the simple opt-in switch to the Docker-backed runtime
- `session_command` remains the low-level escape hatch

## Group IDs

### Lark / Feishu

Copy the group ID directly from the group settings UI.

### Telegram

Add the bot to the target group, then obtain that group's `chat_id`. Supergroups usually look like `-1001234567890`.

## Configuration

If `-config` is not provided, `imcodex` looks for config files in this order:

1. `./imcodex.yaml`
2. `~/.imcodex.yaml`

See [config.example.yaml](config.example.yaml).

Key fields:

| Field | Meaning |
| --- | --- |
| `platform` | `lark` or `telegram` |
| `runtime` | Optional high-level runtime selector: `host-codex`, `docker-codex`, or `docker-claude`; defaults to `host-codex` |
| `runtime_config_dir` | Optional read-only config seed copied into the container-local agent home for `docker-*` runtimes |
| `session_command` | Optional low-level launch command template; if set, it overrides `runtime` |
| `interrupt_on_new_message` | If `true`, a new group message interrupts the current main-session run and keeps only the newest pending message |
| `groups[].group_id` | Group ID or Telegram chat ID |
| `groups[].cwd` | Working directory mapped to that group |
| `groups[].session_name` | Optional override for the tmux session name |
| `groups[].jobs[].name` | Job name shown in job result posts |
| `groups[].jobs[].schedule` | Standard 5-field cron expression |
| `groups[].jobs[].prompt_file` | Markdown prompt file for agent-driven jobs; relative paths are resolved from the config file directory |
| `groups[].jobs[].command` | Shell command for deterministic CLI jobs such as `hl_stack`; runs in `cwd` |
| `groups[].jobs[].artifacts_dir` | Optional base dir for per-run logs; relative paths are resolved from `cwd` |
| `groups[].jobs[].summary_file` | Optional file whose content is posted on success; relative paths are resolved from `cwd` |
| `groups[].jobs[].session_name` | Optional override for a `prompt_file` job session name |

Each job must set exactly one of `prompt_file` or `command`.

Lark / Feishu:

```yaml
platform: lark
lark_app_id: cli_xxx
lark_app_secret: your_app_secret
lark_base_url: https://open.larksuite.com
runtime: host-codex
interrupt_on_new_message: true

groups:
  - group_id: oc_xxx
    cwd: /srv/my-project
    jobs:
      - name: hourly_review
        schedule: "1 * * * *"
        prompt_file: ./prompts/hourly_review.md
      - name: hl_stack_cycle
        schedule: "6 * * * *"
        command: |
          ./ops/run_dry_cycle.sh &&
          printf 'hl_stack dry cycle completed.\nartifacts: %s\n' "$IMCODEX_JOB_ARTIFACTS_DIR" > "$IMCODEX_JOB_SUMMARY_FILE"
```

For Feishu China tenants:

```yaml
lark_base_url: https://open.feishu.cn
```

Telegram:

```yaml
platform: telegram
telegram_bot_token: 123456:ABCDEF
telegram_base_url: https://api.telegram.org
runtime: docker-claude
interrupt_on_new_message: true

groups:
  - group_id: -1001234567890
    cwd: /srv/my-project
    jobs:
      - name: hourly_review
        schedule: "1 * * * *"
        prompt_file: ./prompts/hourly_review.md
      - name: hl_stack_cycle
        schedule: "6 * * * *"
        command: |
          ./ops/run_dry_cycle.sh &&
          printf 'hl_stack dry cycle completed.\nartifacts: %s\n' "$IMCODEX_JOB_ARTIFACTS_DIR" > "$IMCODEX_JOB_SUMMARY_FILE"
```

Supported environment-variable overrides:

```bash
export IMCODEX_PLATFORM=lark
export LARK_APP_ID=cli_xxx
export LARK_APP_SECRET=your_app_secret
export LARK_BASE_URL=https://open.larksuite.com
export TELEGRAM_BOT_TOKEN=123456:ABCDEF
export TELEGRAM_BASE_URL=https://api.telegram.org
```

## Scheduled Jobs

| Item | Behavior |
| --- | --- |
| Job types | `prompt_file` sends Markdown into a persistent agent session; `command` runs `sh -lc` in `cwd` |
| Relationship to main chat | Fully isolated; no shared Codex context |
| Trigger behavior | At the scheduled time, `imcodex` sends the prompt or executes the command |
| Group output | `prompt_file` jobs post final visible output; `command` jobs post `summary_file` when present, otherwise a tail of captured output |
| Overlap policy | If a job is still running when the next trigger arrives, the new trigger is skipped |

### Command Job Artifacts

| Item | Behavior |
| --- | --- |
| Default artifact root | `cwd/.imcodex/jobs/<job-name>/<run-id>/` |
| Auto-written logs | `stdout.log`, `stderr.log`, `combined.log` |
| Success summary | Defaults to `$IMCODEX_JOB_ARTIFACTS_DIR/summary.md` when `summary_file` is not set |
| Failure hint | If command output contains stage lines such as `[2/3] cache record-once`, the last stage is echoed in the failure post |

Injected environment variables:

| Variable | Meaning |
| --- | --- |
| `IMCODEX_JOB_NAME` | Job name |
| `IMCODEX_JOB_GROUP_ID` | Group / chat ID |
| `IMCODEX_JOB_RUN_ID` | Unique run ID |
| `IMCODEX_JOB_CWD` | Job working directory |
| `IMCODEX_JOB_ARTIFACTS_DIR` | Per-run artifact directory |
| `IMCODEX_JOB_SUMMARY_FILE` | Summary file path to write |
| `IMCODEX_JOB_STDOUT_FILE` | Captured stdout log path |
| `IMCODEX_JOB_STDERR_FILE` | Captured stderr log path |
| `IMCODEX_JOB_COMBINED_FILE` | Combined stdout/stderr log path |

### `hl_stack`

| Recommendation | Why |
| --- | --- |
| Prefer `command` jobs | `hl_stack` is primarily a CLI toolchain |
| Keep stage markers like `[1/3] doctor` in scripts | `imcodex` can surface the last stage on failure |
| Write a short summary to `$IMCODEX_JOB_SUMMARY_FILE` | Keeps group posts concise while full logs stay in artifacts |
| Save JSON outputs under `$IMCODEX_JOB_ARTIFACTS_DIR` | Preserves `decision_context.json`, `plan.json`, `execution.json` and similar artifacts |

## Build

| Command | Result |
| --- | --- |
| `make` | Builds the local binary under `build/` |
| `make linux` | Builds the Linux `amd64` binary under `build/` and packs it with `upx` |
| `make test` | Runs unit tests plus `-race` |

Run:

```bash
make
./build/imcodex-$(go env GOOS)-$(go env GOARCH) -config imcodex.yaml
```

If you use `./imcodex.yaml` or `~/.imcodex.yaml`, `-config` is optional:

```bash
./build/imcodex-$(go env GOOS)-$(go env GOARCH)
```

Expected startup log:

```text
imcodex 2.1.1 started: config=/srv/imcodex/imcodex.yaml platform=lark groups=1 jobs=1 base=https://open.larksuite.com
```

## Runtime Behavior

| Scenario | Behavior |
| --- | --- |
| Plain text | Forwarded to the group's main agent session |
| Multi-line input | Preserved as one pasted input |
| Slash commands | Forwarded as-is, for example `/new` or `/compact` |
| Images / files | Downloaded into `cwd/.imcodex/inbox/`, then forwarded as a short text prompt with the saved path |
| Telegram live output | Sends periodic typing actions while the first visible reply is pending, keeps a separate working status, and updates segmented body messages at a low frequency |
| Telegram edit rate limit | If Telegram returns repeated `429` on `editMessageText`, buffered text first waits through `retry_after`, then degrades to plain sends so replies do not stay invisible indefinitely |
| Telegram forwarding identity | Each Codex run is tracked by `(run_id, cursor)` for ordering and debug telemetry |
| New prompt while prior tail is blocked on edit/backoff | New prompt dispatch proceeds immediately; unsent prior tail is detached to an internal send queue and delivered asynchronously |
| Boundary capture safety | Before dispatching a new prompt, if boundary capture still shows busy or capture fails, dispatch is deferred so prior tail output is not dropped |
| Telegram forwarding watchdog | If buffered output or detached queue head stays pending too long, a watchdog forces drain/retry; prolonged editable backoff falls back into the detached plain-send path instead of stalling in memory |
| Tmux capture/session transient failure | Pending buffered output is retained and retried after reconnect before dispatching the next prompt |
| Silent long-think protection | If a run is still in flight but tmux temporarily shows no visible body, `imcodex` keeps the run busy for a grace window instead of prematurely declaring completion |
| Busy detection | Busy state is derived from the agent working chrome near the prompt, reducing false idle transitions when the pane footer layout shifts |
| New message while main session is busy | Interrupts the current run and keeps only the newest pending message by default |
| Job execution | Posts only the final result, not live incremental output |
| Restart | Reuses existing `tmux` sessions when they still exist |

Current Telegram defaults are internal constants: `working` after about `1s`, editable body refresh at most every `5s` while a run is still busy, idle flush after `24` polling ticks (`~12s` at 500ms polling), output watchdog around `8s`, detached-queue watchdog around `15s`, and rollover near `2800` runes. See [docs/telegram-output-buffering.md](docs/telegram-output-buffering.md).

For more detail on the 2.0 runtime design that keeps host `tmux` while moving
Codex or Claude execution into a workspace-confined Docker runtime, see
[docs/runtime-v2-docker-tmux.md](docs/runtime-v2-docker-tmux.md). Existing
1.x configs remain valid; `runtime` is the new high-level shortcut and
`session_command` remains optional as an escape hatch. Example
wrapper and Docker image assets live in
[docs/runtime-v2-examples.md](docs/runtime-v2-examples.md).

### Codex Or Claude Keeps Asking To Upgrade

Recommended handling:

1. Do not use `latest` in the runtime image or wrapper defaults.
2. Pin a known-good CLI version in Docker.
3. Rebuild and redeploy only during a maintenance window.
4. For host-side Claude, disable automatic updater prompts if needed.
5. For host-side Codex, treat upgrades as manual maintenance, not an
   interactive runtime action.

Operationally, the simplest stable approach is:

- host `imcodex`
- host `tmux`
- pinned Docker image for `codex` or `claude`
- no live `npm install -g ...@latest` inside production sessions

## Inspect Sessions

```bash
tmux ls
tmux attach -r -t <session-name>
```

Session names are generated automatically for both main chat sessions and job sessions.

## Troubleshooting

### Main chat messages are not reaching Codex

Check:

1. The bot is in the correct group.
2. `group_id` / `chat_id` matches the real target group.
3. `cwd` exists on the host running `imcodex`.
4. If you use host mode, `tmux` and `codex` are in `PATH`, and `codex login` has already completed.
5. If you use `runtime: docker-*`, `tmux`, `docker`, and `imcodex-agent-run` are in `PATH`.
6. For Telegram, privacy mode is disabled if you expect normal group messages to reach the bot.

### Scheduled jobs are not firing

Check:

1. `schedule` is a valid 5-field cron expression.
2. `prompt_file` exists and is not empty, or `command` is set.
3. For `prompt_file` jobs, the job's `tmux` session is not still stuck in an earlier run.
4. For `command` jobs, the command runs successfully from `cwd` when invoked with `sh -lc`.

## License

MIT. See [LICENSE](LICENSE).
