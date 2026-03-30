# imcodex

`imcodex` bridges a Lark, Feishu, or Telegram group chat to long-lived Codex CLI sessions running inside `tmux`.

## Model

| Item | Behavior |
| --- | --- |
| Main chat session | One group = one `cwd` = one persistent Codex session |
| Scheduled jobs | Declared in YAML; each job is either a `prompt_file` Codex session or a `command` shell run |
| Job visibility | Jobs post final results or failures back to the same group |
| Context isolation | The main chat session and job sessions do not share Codex context |
| Reconfiguration | Edit YAML and prompt `.md` files, then restart `imcodex` |

## Requirements

| Requirement | Notes |
| --- | --- |
| `tmux` | Required at runtime |
| Codex CLI | Required; `codex login` must already be completed |
| Go 1.24+ | Needed only for local builds |
| Lark / Feishu bot or Telegram bot | Pick one backend |

## Install

macOS:

```bash
brew install tmux
npm install -g @openai/codex
codex login
```

Ubuntu 24.04:

```bash
sudo apt update
sudo apt install -y tmux
sudo npm install -g @openai/codex
codex login
```

Verify:

```bash
tmux -V
codex --version
```

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
| `interrupt_on_new_message` | If `true`, a new group message interrupts the current main-session run and keeps only the newest pending message |
| `groups[].group_id` | Group ID or Telegram chat ID |
| `groups[].cwd` | Working directory mapped to that group |
| `groups[].jobs[].name` | Job name shown in job result posts |
| `groups[].jobs[].schedule` | Standard 5-field cron expression |
| `groups[].jobs[].prompt_file` | Markdown prompt file for Codex-driven jobs; relative paths are resolved from the config file directory |
| `groups[].jobs[].command` | Shell command for deterministic CLI jobs such as `hl_stack`; runs in `cwd` |
| `groups[].jobs[].artifacts_dir` | Optional base dir for per-run logs; relative paths are resolved from `cwd` |
| `groups[].jobs[].summary_file` | Optional file whose content is posted on success; relative paths are resolved from `cwd` |

Each job must set exactly one of `prompt_file` or `command`.

Lark / Feishu:

```yaml
platform: lark
lark_app_id: cli_xxx
lark_app_secret: your_app_secret
lark_base_url: https://open.larksuite.com
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
| Job types | `prompt_file` sends Markdown into a persistent Codex session; `command` runs `sh -lc` in `cwd` |
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
imcodex 1.1.5 started: config=/srv/imcodex/imcodex.yaml platform=lark groups=1 jobs=1 base=https://open.larksuite.com
```

## Runtime Behavior

| Scenario | Behavior |
| --- | --- |
| Plain text | Forwarded to the group's main Codex session |
| Multi-line input | Preserved as one pasted input |
| Slash commands | Forwarded as-is, for example `/new` or `/compact` |
| Images / files | Downloaded into `cwd/.imcodex/inbox/`, then forwarded as a short text prompt with the saved path |
| Telegram live output | Sends periodic typing actions while the first visible reply is pending, posts one early working status, then edits an active message after a short idle debounce |
| Telegram edit rate limit | If Telegram returns `429` on `editMessageText`, buffered text is preserved and retried after `retry_after` backoff |
| New prompt while prior tail is blocked on edit/backoff | New prompt dispatch proceeds immediately; unsent prior tail is detached to an internal send queue and delivered asynchronously |
| Tmux capture/session transient failure | Pending buffered output is retained and retried after reconnect before dispatching the next prompt |
| New message while main session is busy | Interrupts the current run and keeps only the newest pending message by default |
| Job execution | Posts only the final result, not live incremental output |
| Restart | Reuses existing `tmux` sessions when they still exist |

Current Telegram defaults are internal constants: `working` after about `1s`, partial body refresh at most every `5s` while Codex is still busy, immediate body flush when busy transitions to idle (with an idle debounce fallback around `4s`), and rollover near `2800` runes. See [docs/telegram-output-buffering.md](docs/telegram-output-buffering.md).

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
4. `tmux` and `codex` are in `PATH`.
5. `codex login` has already completed.
6. For Telegram, privacy mode is disabled if you expect normal group messages to reach the bot.

### Scheduled jobs are not firing

Check:

1. `schedule` is a valid 5-field cron expression.
2. `prompt_file` exists and is not empty, or `command` is set.
3. For `prompt_file` jobs, the job's `tmux` session is not still stuck in an earlier run.
4. For `command` jobs, the command runs successfully from `cwd` when invoked with `sh -lc`.

## License

MIT. See [LICENSE](LICENSE).
