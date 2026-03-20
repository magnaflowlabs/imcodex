# imcodex

`imcodex` connects a Lark group chat to a long-running Codex CLI session on your machine and sends new Codex output back to the group.

Use it when:

- one Lark group should map to one project directory
- Codex should stay alive in the background
- people should talk to Codex directly from the group

It does not need a public webhook or an inbound port.

## Important

`imcodex` starts Codex in automatic mode:

- approval policy: `never`
- sandbox mode: `danger-full-access`
- directory trust prompt: auto-confirmed
- command approval prompt: auto-confirmed if it still appears

Only run this on a machine you control and trust.

## Requirements

You do not need to know Go to use it, but you do need these tools installed:

- `tmux`
- Codex CLI, with `codex login` already completed
- Go 1.24+ to build the binary locally
- a Lark bot app with `app_id` and `app_secret`
- the target group `group_id`

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

## Lark setup

1. Create or open your Lark bot app.
2. Enable bot capability.
3. Subscribe to the `im.message.receive_v1` event.
4. Add the bot to the group you want to use.
5. Copy the group `group_id` from group settings.

## Config file

The default config file name is `imcodex.yaml`.

Start from the example:

```bash
cp config.example.yaml imcodex.yaml
```

Minimal config:

```yaml
lark_app_id: cli_xxx
lark_app_secret: your_app_secret
lark_base_url: https://open.larksuite.com
groups:
  - group_id: oc_xxx
    cwd: /srv/my-project
```

For Feishu China, use:

```yaml
lark_base_url: https://open.feishu.cn
```

You can also keep secrets in environment variables:

```bash
export LARK_APP_ID=cli_xxx
export LARK_APP_SECRET=your_app_secret
export LARK_BASE_URL=https://open.larksuite.com
```

Field meanings:

- `lark_app_id`: Lark bot app ID
- `lark_app_secret`: Lark bot app secret
- `lark_base_url`: API base URL for Lark or Feishu
- `groups[].group_id`: group mapped to Codex
- `groups[].cwd`: project directory for that group

Add more entries under `groups:` if you want to connect multiple groups.

## Start

Build and run:

```bash
make
./build/imcodex-$(go env GOOS)-$(go env GOARCH) -config imcodex.yaml
```

If you use the default file name `imcodex.yaml`, `-config` is optional:

```bash
./build/imcodex-$(go env GOOS)-$(go env GOARCH)
```

Successful startup looks like:

```text
imcodex started: config=imcodex.yaml groups=1 base=https://open.larksuite.com
```

## Daily use

After startup, just send messages in the configured Lark group.

- plain text is forwarded to Codex
- commands such as `/new`, `/compact`, and `/status` are forwarded as-is
- messages in the same group are queued
- different groups run independently
- existing `tmux` sessions are reused after restart

## View the tmux session

List sessions:

```bash
tmux ls
```

Attach read-only:

```bash
tmux attach -r -t <session-name>
```

## Troubleshooting

### Messages are not forwarded

Check:

- the bot is in the target group
- `group_id` matches the real group
- `codex login` is complete
- `tmux` and `codex` are in `PATH`
- `imcodex.yaml` is the file actually being loaded

### The tmux session is missing after restart

If the `tmux` session does not exist, `imcodex` recreates it when the next message arrives.

### I want multiple projects

Add multiple entries under `groups:`. One entry is one group and one working directory.

## License

MIT. See [LICENSE](LICENSE).
