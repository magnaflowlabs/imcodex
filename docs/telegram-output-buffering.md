# Telegram Output Buffering

## Status

| Item | Value |
| --- | --- |
| Scope | Main chat replies forwarded from Codex to Telegram |
| Status | Implemented |
| Goal | Reduce fragmented Telegram messages without making the first visible response feel slow |

## v1.1.10 Hardening

| Area | Result |
| --- | --- |
| Dispatch boundary | New prompt dispatch is deferred if boundary capture fails or still reports busy, preventing prior-tail truncation |
| Reset merge | Buffer reset path now merges with existing unsent tail instead of replacing it |
| Detached queue retry | Detached chunks are retained and retried in-order for both 429 and transient non-429 failures |
| Long-output integrity | Added regressions for very long replies, boundary newlines, and busy/idle boundary retries |
| Scheduler logging safety | Fixed concurrent stdout/stderr-to-combined writer race found by `go test -race` |

## v1.1.14 Stability Follow-Up

| Area | Result |
| --- | --- |
| 429 edit backoff behavior | `defer` now applies only during active backoff windows; once backoff expires, editable sync resumes immediately (no wait-for-idle lock) |
| Watchdog + editable backoff | Short backoff windows keep editable body buffered; repeated 429s now degrade to plain sends so Telegram output does not appear stalled forever |
| Replay risk under 429 | Deferred buffering now reconciles against the last published body so plain-send fallback forwards only the unsent tail |
| Prompt echo cleanup | Normalization drops prompt continuation lines too, preventing wrapped long inputs from being forwarded back as reply text |
| Verification harness | Added stronger mismatch diagnostics (head/tail/context/containment) for `tools/tgstub_e2e` |

## v1.1.13 Run-State Hardening

| Area | Result |
| --- | --- |
| Silent long-think runs | If a run is still in flight but tmux briefly shows no visible body, `imcodex` keeps the run busy for a `20m` grace window instead of declaring completion too early |
| Reconnect during in-flight run | Transient tmux capture/session failures no longer clear the active run state; pending tail output survives reconnect |
| Recovery bookkeeping | Recovered in-flight runs preserve `busySince`, so long-think protection continues to apply after reconnect instead of expiring immediately |
| Busy detection | `tmuxctl.IsBusy()` now checks the prompt-adjacent working chrome window instead of depending only on the pane tail layout |
| Verification | Added regressions for silent runs, capture-failure recovery, grace expiry, and prompt-adjacent busy detection |

## Problem

| Symptom | Cause |
| --- | --- |
| One Codex answer appears as many Telegram messages | `imcodex` flushes too soon when the tmux snapshot briefly goes idle |
| Users feel the bot is slow if nothing appears quickly | Waiting for the full answer before posting removes early feedback |
| Users see only `[working]` for too long while Codex is still generating | Body text is buffered until idle, so long busy runs can hide already captured output |

## Decision

| Topic | Decision |
| --- | --- |
| First visible feedback | Send one short `working` status quickly |
| Telegram reply mode | Prefer `editMessageText` on one active Telegram message |
| Body flush | Use a longer debounce for idle completion, plus a maximum visible delay while Codex is still busy |
| Flush trigger | Flush body after idle debounce, or sooner if buffered body text has been hidden for too long during a busy run |
| Request boundary | Refresh the tmux output baseline immediately before dispatching a new prompt |
| Boundary safety gate | If boundary capture fails or still shows busy state, defer new prompt dispatch and retry |
| Before next user message dispatch | Try flush first; if edit path is blocked (backoff/edit-invalid), detach unsent tail to send queue and dispatch next prompt immediately |
| Delivery identity | Track each Codex answer with `run_id` and monotonic `cursor` for ordering and observability |
| Outbound execution model | Use one per-group event loop to serialize all Telegram send/edit/retry operations |
| Capture/session recovery | Retain buffered body text across transient tmux capture/session failures and retry flush after reconnect |
| Telegram length limit | When the active message approaches a soft limit, roll over to a new Telegram message |
| Telegram API 429 | Respect `retry_after`; keep buffered body text through the first backoff window, then fall back to plain sends if repeated edit 429s keep blocking visibility |
| Detached send failures | Keep the detached queue head and retry with bounded backoff; do not drop on transient non-429 failures |
| Watchdog | Force drain if buffered output or detached queue stays pending too long |

## Target Behavior

| Phase | Behavior |
| --- | --- |
| User sends prompt | Prompt is forwarded to Codex immediately |
| Codex is still thinking | After a short delay, send one status message such as `[working] Codex is processing.` and keep its Telegram `message_id` as the active output message |
| Codex starts producing content | Buffer reply text; do not send each short pause as a separate Telegram message |
| Codex keeps running for a while | If buffered body text has not been shown for too long, edit the active Telegram message with the latest partial body |
| Codex becomes idle | Flush buffered reply text immediately on busy→idle transition (idle debounce remains as a fallback path) |
| Active message nears Telegram limit | Finalize the current message and create a new continuation message, then continue editing the new one |
| New user prompt arrives while previous reply text is buffered | Flush buffered text first, then dispatch the new prompt |
| Edit path stays blocked while next prompt arrives | New prompt still dispatches immediately; unsent tail is sent asynchronously from detached queue |
| A send/edit operation succeeds | Commit telemetry cursor and continue from the next detached chunk in order |

## Proposed Timing

| Parameter | Proposed default | Notes |
| --- | --- | --- |
| `working_after` | `1s` | Fast feedback so the group knows the bot is alive |
| `busy_flush_after` | `15s` | Maximum time buffered body text can stay hidden while Codex is still busy |
| Idle flush debounce | `24` polls (`~12s` at 500ms polling) | Cuts edit frequency materially while keeping long runs visibly alive |
| `edit_rollover_at` | `2800` runes | Soft limit for Telegram message editing |
| `output_watchdog_after` | `8s` | Maximum time buffered output can remain pending before forced drain/detach |
| `detached_watchdog_after` | `15s` | Maximum age of detached queue head before forced retry |
| Hard per-message limit | `3000` runes | Keep existing safe limit below Telegram's hard ceiling |
| Poll interval | Keep current polling model | Effective flush delay should be derived from polling interval |

## Message Lifecycle

| Step | Action |
| --- | --- |
| 1 | No message is sent if Codex answers before `working_after` |
| 2 | If Codex is still busy at `working_after`, send one `[working]` message and store its Telegram `message_id` |
| 3 | While Codex is generating, append deltas to an internal reply buffer |
| 4 | If Codex stays busy and buffered body text has been hidden for `busy_flush_after`, merge the latest buffered text into the active Telegram message with `editMessageText` |
| 5 | On busy→idle transition, merge buffered text into the active Telegram message immediately (with idle tick debounce fallback for edge cases) |
| 6 | If the merged text would exceed `edit_rollover_at`, keep the current message as-is, send a new continuation message, and continue edits there |
| 7 | If a new user prompt arrives before the buffered text is flushed, flush first, then dispatch the new prompt |
| 8 | Right before dispatch, refresh the tmux baseline so the next reply cannot replay stale tail output from the prior run |
| 9 | If boundary capture still shows busy or capture fails, keep the pending user message queued and retry dispatch on the next loop |
| 10 | If editable flush cannot complete at the boundary (for example retry-backoff or stale editable message), detach the unsent tail into a plain send queue and continue dispatch without blocking |
| 11 | Detached chunks carry `(run_id, cursor)` and are retried in order until successful delivery |
| 12 | If output buffer age exceeds `output_watchdog_after`, force drain; short editable-backoff windows keep body buffered, but repeated edit 429s switch the run to plain-send fallback |

## Rationale

| Choice | Why |
| --- | --- |
| Quick `working` message | Keeps response latency low for humans waiting in Telegram |
| Busy-time partial edits | Prevents long-running Codex replies from looking stalled when output is already available |
| Slower idle flush | Most fragmentation comes from short internal pauses, not from true reply completion |
| Edit the active message | Avoids flooding the group with near-duplicate partial updates |
| Soft rollover before hard limit | Preserves a safety margin and keeps continuation behavior predictable |
| Flush before next dispatch | Prevents the previous answer from being lost or visually reordered |

## Non-Goals

| Not included | Reason |
| --- | --- |
| Cross-platform output differences for Lark/Feishu | This proposal is only for Telegram forwarding behavior |
| Rewriting Codex prompt format | The issue is transport behavior, not prompt content |
| Per-token live editing | Too chatty and defeats the debounce goal |

## Config Surface

| Field | Type | Default | Purpose |
| --- | --- | --- | --- |
| `working_after` | duration | `1s` | Delay before sending the first status message |
| `busy_flush_after` | duration | `15s` | Maximum hidden-body delay while Codex is still generating |
| `flush_idle_ticks` | integer | `24` | Polls required before flushing buffered reply text |
| `output_watchdog_after` | duration | `8s` | Force drain buffered output if no successful forward for too long |
| `detached_watchdog_after` | duration | `15s` | Force retry detached queue head if it remains pending too long |
| `edit_rollover_at` | integer | `2800` | Soft threshold for starting a new Telegram message |

Current implementation uses internal constants for these values. YAML exposure is a follow-up.

## Acceptance Criteria

| Case | Expected result |
| --- | --- |
| Codex pauses briefly mid-reply | Telegram receives one combined body message instead of several small ones |
| Codex thinks for a while before answering | Telegram gets one early `working` status |
| Codex keeps generating for a long time | Telegram message body is refreshed periodically instead of staying on `[working]` until idle |
| A new prompt arrives right after the prior reply ends | The previous buffered reply is sent before the new prompt starts |
| Tmux state advances between polls right before a new prompt | The next Telegram reply starts from the refreshed boundary and does not replay stale prior output |
| Reply fits within one message | The initial `working` message is edited into the final body message |
| Reply exceeds Telegram safe size | Telegram shows a small number of continuation messages, each maintained with edit-in-place until rollover |
| Telegram returns repeated `429 Too Many Requests` during edit | Buffered body waits through backoff first, then the unsent tail is forwarded via plain sends instead of staying invisible |
| Telegram path retries and reconnects under pressure | `(run_id, cursor)` remains monotonic and detached chunks are retried in order until delivery |
| Tmux capture fails temporarily while body is buffered | Buffered body survives reconnect and is delivered before the next prompt dispatch |

## Follow-Ups

1. Expose `working_after`, `busy_flush_after`, `flush_idle_ticks`, watchdog parameters, and `edit_rollover_at` through YAML if runtime tuning is needed.
2. Add a small telemetry panel/command for `(group_id, run_id, cursor, buffer_len, detached_len)` to simplify production debugging.

## Real E2E Harness

Use a local Telegram API stub plus a real tmux+Codex session to verify no truncation or replay:

```bash
go run ./tools/tgstub_e2e \
  -group-id -5125916641 \
  -cwd /home/vical/your_project \
  -session imcodex-your-session \
  -prompt "请先思考至少90秒且不要输出任何正文；思考完成后仅输出一行：E2E-STUB-CHECK-DONE"
```

Notes:

1. The harness requires the target tmux session to already exist by default (`-require-existing=true`) and uses the real Codex pane in that session.
2. The harness injects one inbound Telegram message via `getUpdates`, records all `sendMessage`/`editMessageText` calls from `imcodex`, and compares aggregated forwarded body with tmux final delta.
3. Exit code is non-zero on mismatch; logs include first diff index, output tails, and the last send/edit events for diagnosis.
4. 429 stress can be injected with `-send-429`, `-edit-429`, and `-retry-after`.
