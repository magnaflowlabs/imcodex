# imcodex

`imcodex` connects a Lark/Feishu group chat to a long-running Codex CLI session inside `tmux`.

In practice:

- send a text message in a configured group
- `imcodex` forwards it to Codex in the matching working directory
- Codex output is captured from `tmux` and sent back to the group

Each configured group gets its own `tmux` session and its own Codex context.

## Requirements

- Go 1.24+
- `tmux` in `PATH`
- `codex` in `PATH`
- a Lark/Feishu app with bot capability
- `lark_app_id` and `lark_app_secret`

`upx` is optional. It is only needed for `make linux`.

## Install

### macOS

Install runtime dependencies:

```bash
brew install tmux
npm install -g @openai/codex
codex login
```

Optional build dependency:

```bash
brew install upx
```

If you want to use the CLI bundled inside the Codex desktop app instead of the npm package:

```bash
echo 'export PATH="/Applications/Codex.app/Contents/Resources:$PATH"' >> ~/.zshrc
source ~/.zshrc
codex --version
```

### Ubuntu 24.04

Install runtime dependencies:

```bash
sudo apt update
sudo apt install -y tmux nodejs npm
sudo npm install -g @openai/codex
codex login
```

Optional build dependency:

```bash
# Install UPX only if you plan to run `make linux`.
```

### Verify the toolchain

```bash
go version
tmux -V
codex --version
```

## Lark / Feishu Setup

1. Create or open your app in Lark/Feishu Open Platform.
2. Enable bot capability.
3. Subscribe to the message receive event `im.message.receive_v1`.
4. Add the bot to every target group.
5. Copy the target group ID from the group settings UI.

Recommended permissions:

- group message receive permission, if you want all group messages
- or at-bot message receive permission, if you only want messages that mention the bot

Group ID lookup:

1. Open the target group.
2. Open Group Settings / Chat Settings.
3. Copy the Group ID or Chat ID shown by the client.
4. Put that value into `groups[].group_id`.

## Configuration

The default config path is `./imcodex.yaml`. You can override it with `-config /path/to/imcodex.yaml`.

Start from the example:

```bash
cp config.example.yaml config.yaml
$EDITOR config.yaml
```

Minimal example:

```yaml
lark_app_id: cli_xxx
lark_app_secret: your_app_secret
lark_base_url: https://open.larksuite.com
groups:
  - group_id: oc_xxx
    cwd: /srv/my-project
  - group_id: oc_yyy
    cwd: /srv/another-project
```

Config fields:

| Key | Description |
|---|---|
| `lark_app_id` / `LARK_APP_ID` | Lark/Feishu app ID |
| `lark_app_secret` / `LARK_APP_SECRET` | Lark/Feishu app secret |
| `lark_base_url` / `LARK_BASE_URL` | `https://open.larksuite.com` for Lark, `https://open.feishu.cn` for Feishu China |
| `groups[].group_id` | target group ID |
| `groups[].cwd` | working directory for the Codex session behind that group |

You can also keep secrets in the environment:

```bash
export LARK_APP_ID=cli_xxx
export LARK_APP_SECRET=your_app_secret
export LARK_BASE_URL=https://open.larksuite.com
```

Do not commit real secrets.

## Build

```bash
make
```

This creates:

```text
./build/imcodex-<goos>-<goarch>
```

Other build targets:

- `make linux`: build a compressed Linux release binary
- `make test`: run `go test ./...` and `go test -race ./...`

## Run

From source:

```bash
go run . -config config.yaml
```

From a built binary:

```bash
./build/imcodex-$(go env GOOS)-$(go env GOARCH) -config config.yaml
```

On startup, `imcodex` opens one long connection to Lark/Feishu and lazily creates `tmux` sessions when the first message arrives for each group.

## Daily Use

What happens when you use it:

- plain text sent in a configured group is forwarded to Codex
- slash commands such as `/new`, `/compact`, and `/status` are passed through as normal text
- each group is serialized through its own queue
- different groups run independently
- if `imcodex` restarts, existing `tmux` sessions are reused

The generated `tmux` session name looks like:

```text
imcodex-<cwd-base>-<group-id>
```

Useful commands:

```bash
tmux ls
tmux attach -r -t <session>
tmux capture-pane -pJ -S -200 -t <session>:0.0
```

Use `tmux attach -r` for observation. Avoid typing into the same session while the bridge is active.

## Troubleshooting

### Nothing is forwarded from the group

Check:

- the bot is in the group
- the group ID in `config.yaml` matches the real group
- the app subscribed to `im.message.receive_v1`
- the app has the right message receive permission

### `tmux` session is not created

Check:

- `tmux` is installed
- `tmux` is in `PATH`
- the configured `cwd` exists

### Codex does not start

Check:

- `codex` is installed
- `codex` is in `PATH`
- `codex login` already succeeded on that machine

### I want to inspect what Codex is doing

Use:

```bash
tmux ls
tmux attach -r -t <session>
```

Read-only attach is recommended. If you type into the same session, you are sharing the terminal with the bridge.

## Development

```bash
make
make linux
make test
```

## License

MIT. See [LICENSE](LICENSE).

## References

- [Codex CLI overview](https://openai.com/codex/)
- [Codex CLI getting started](https://help.openai.com/en/articles/11096431-openai-codex-cli-getting-started)
- [Codex CLI sign-in with ChatGPT](https://help.openai.com/en/articles/11381614-codex-cli-and-sign-in-with-chatgpt)
- [Lark Developer](https://open.larksuite.com/?lang=en-US)
- [Receive message event: `im.message.receive_v1`](https://open.feishu.cn/document/server-docs/im-v1/message/events/receive)
- [Create message API: `POST /open-apis/im/v1/messages`](https://open.feishu.cn/document/server-docs/im-v1/message/create)
