# imcodex

`imcodex` bridges a Lark, Feishu, or Telegram group chat to a long-running Codex CLI session on your machine.

Each configured group maps to one working directory and one persistent `tmux`-hosted Codex session.

## Overview

| Item | Behavior |
| --- | --- |
| Chat model | One group = one project directory = one Codex session |
| Session host | `tmux` |
| IM backends | Lark / Feishu via outbound WebSocket, Telegram via long polling |
| Public webhook | Not required |
| Inbound port | Not required |
| Multi-line user input | Sent to Codex as bracketed paste |
| Very long text input | Loaded into `tmux` from a temp buffer file |
| Output forwarded to chat | Codex reply text, flushed when the run pauses or completes |
| Output hidden from chat | Codex terminal chrome and input box |

## Safety

`imcodex` starts Codex with these defaults:

| Setting | Value |
| --- | --- |
| Approval policy | `never` |
| Sandbox mode | `danger-full-access` |
| Directory trust prompt | Auto-confirmed |

Use it only on a machine you control and trust.

## Requirements

| Requirement | Notes |
| --- | --- |
| `tmux` | Required at runtime |
| Codex CLI | `codex login` must already be complete |
| Go 1.24+ | Required only if you build locally |
| Lark/Feishu bot app or Telegram bot | Pick one backend |
| Group ID / chat ID | Depends on backend |

## Install

### macOS

```bash
brew install tmux
npm install -g @openai/codex
codex login
```

### Ubuntu 24.04

```bash
sudo apt update
sudo apt install -y tmux
sudo npm install -g @openai/codex
codex login
```

### Verify the toolchain

```bash
go version
tmux -V
codex --version
```

## Lark or Feishu setup

1. Create or open your bot app.
2. Enable bot capability.
3. Subscribe to `im.message.receive_v1`.
4. Add the bot to the target group.
5. Copy the group `group_id` from the group settings UI.

## Telegram setup

1. Open `@BotFather`.
2. Create a bot with `/newbot`.
3. Save the bot token.
4. Open `/setprivacy` in `@BotFather` and disable privacy mode for the bot.
5. Add the bot to the target group or supergroup.
6. Get the Telegram `chat_id` for that group.
7. Make sure the machine running `imcodex` can reach `https://api.telegram.org`, or provide `HTTPS_PROXY` / a self-hosted Bot API endpoint through `telegram_base_url`.

## Configuration

If `-config` is not provided, `imcodex` looks for config files in this order:

1. `./imcodex.yaml`
2. `~/.imcodex.yaml`

Create a config file from the example:

```bash
cp config.example.yaml imcodex.yaml
```

Lark / Feishu config:

```yaml
platform: lark
lark_app_id: cli_xxx
lark_app_secret: your_app_secret
lark_base_url: https://open.larksuite.com
interrupt_on_new_message: true
groups:
  - group_id: oc_xxx
    cwd: /srv/my-project
```

For Feishu China, set:

```yaml
lark_base_url: https://open.feishu.cn
```

Telegram config:

```yaml
platform: telegram
telegram_bot_token: 123456:ABCDEF
telegram_base_url: https://api.telegram.org
interrupt_on_new_message: true
groups:
  - group_id: -1001234567890
    cwd: /srv/my-project
```

Optional environment variables:

```bash
export IMCODEX_PLATFORM=lark
export LARK_APP_ID=cli_xxx
export LARK_APP_SECRET=your_app_secret
export LARK_BASE_URL=https://open.larksuite.com
export TELEGRAM_BOT_TOKEN=123456:ABCDEF
export TELEGRAM_BASE_URL=https://api.telegram.org
export HTTPS_PROXY=http://127.0.0.1:7003
```

| Field | Meaning |
| --- | --- |
| `platform` | `lark` or `telegram` |
| `lark_app_id` | Bot app ID |
| `lark_app_secret` | Bot app secret |
| `lark_base_url` | API base URL for Lark or Feishu |
| `telegram_bot_token` | Telegram bot token from `@BotFather` |
| `telegram_base_url` | Telegram Bot API base URL |
| `interrupt_on_new_message` | If `true`, a new user message interrupts the current Codex run and keeps only the newest pending message |
| `groups[].group_id` | Lark group ID or Telegram chat ID mapped to Codex |
| `groups[].cwd` | Working directory for that group |

Add more entries under `groups:` to connect more projects.

## Build

| Command | Output |
| --- | --- |
| `make` | Local binary in `build/` |
| `make linux` | Linux `amd64` binary in `build/`, packed with `upx` |
| `make test` | Unit tests, including `-race` |

Examples:

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
imcodex started: config=imcodex.yaml platform=lark groups=1 base=https://open.larksuite.com
```

## Runtime behavior

After startup, send messages in the configured group as if you were talking directly to Codex.

| Behavior | Details |
| --- | --- |
| Plain text | Forwarded to Codex |
| Images and file-like attachments | Saved under `cwd/.imcodex/inbox/` and forwarded as a short prompt with the saved path |
| Slash commands | Forwarded as-is, for example `/new`, `/compact`, `/status` |
| Multi-line messages | Preserved as one pasted input |
| Group queue | One active run per group |
| Interrupt on new message | Enabled by default; sends `Esc`, then `Ctrl-C` if Codex stays busy |
| Startup backlog | While a session is starting or recovering, only the latest pending message is kept |
| Multiple groups | Run independently |
| Restarts | Existing `tmux` sessions are reused |
| Single instance | One running `imcodex` process per config file |
| Working notice | Sends `[working]` only if a request stays busy for a few seconds |
| Replies | Sent after Codex pauses or finishes, chunked when needed |
| Telegram message size | Replies are split before Telegram's `sendMessage` limit |

## Inspect the session

```bash
tmux ls
tmux attach -r -t <session-name>
```

## Troubleshooting

### Messages are not forwarded

Check:

1. The bot is in the correct group.
2. `group_id` matches the real group or Telegram `chat_id`.
3. Each configured `cwd` exists on the machine running `imcodex`.
4. `codex login` has already completed.
5. `tmux` and `codex` are in `PATH`.
6. The startup log shows the config file you expected.
7. For Telegram, privacy mode is disabled in `@BotFather`.
8. For Telegram, the host running `imcodex` can reach `api.telegram.org`, or `HTTPS_PROXY` / `telegram_base_url` is configured correctly.

If Telegram privacy mode is still enabled, group messages may only reach the bot when they are commands, mentions, or replies to the bot.

### Messages are duplicated

Run only one `imcodex` process for the same config file. A second process with the same groups will forward the same Codex output again.

### Images and files are not inspected

`imcodex` downloads supported attachments into `cwd/.imcodex/inbox/` and sends Codex a short text prompt with the saved path. Lark / Feishu supports `image` plus file-like attachments (`file`, `audio`, `video`, `media`). Telegram supports photos plus file-like attachments such as documents, audio, video, and voice notes.

### The session disappeared after a restart

If the `tmux` session no longer exists, `imcodex` recreates it when the next group message arrives.

### I want more than one project

Add more entries under `groups:`. Each entry is one group and one working directory.

## License

MIT. See [LICENSE](LICENSE).
