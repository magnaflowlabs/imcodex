# Telegram Output Buffering

## Status

| Item | Value |
| --- | --- |
| Scope | Main chat replies forwarded from Codex to Telegram |
| Status | Implemented |
| Goal | Reduce fragmented Telegram messages without making the first visible response feel slow |

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
| Before next user message dispatch | Force-flush any buffered body text first |
| Capture/session recovery | Retain buffered body text across transient tmux capture/session failures and retry flush after reconnect |
| Telegram length limit | When the active message approaches a soft limit, roll over to a new Telegram message |
| Telegram API 429 | Respect `retry_after`; keep buffered body text and retry edit after backoff |

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

## Proposed Timing

| Parameter | Proposed default | Notes |
| --- | --- | --- |
| `working_after` | `1s` | Fast feedback so the group knows the bot is alive |
| `busy_flush_after` | `5s` | Maximum time buffered body text can stay hidden while Codex is still busy |
| `body_flush_after` | `4s` | Long enough to absorb short pauses in Codex output |
| `edit_rollover_at` | `2800` runes | Soft limit for Telegram message editing |
| Hard per-message limit | `3000` runes | Keep existing safe limit below Telegram's hard ceiling |
| Poll interval | Keep current polling model | Effective flush delay should be derived from polling interval |

## Message Lifecycle

| Step | Action |
| --- | --- |
| 1 | No message is sent if Codex answers before `working_after` |
| 2 | If Codex is still busy at `working_after`, send one `[working]` message and store its Telegram `message_id` |
| 3 | While Codex is generating, append deltas to an internal reply buffer |
| 4 | If Codex stays busy and buffered body text has been hidden for `busy_flush_after`, merge the latest buffered text into the active Telegram message with `editMessageText` |
| 5 | On busy→idle transition, merge buffered text into the active Telegram message immediately (with `body_flush_after` as fallback for edge cases) |
| 6 | If the merged text would exceed `edit_rollover_at`, keep the current message as-is, send a new continuation message, and continue edits there |
| 7 | If a new user prompt arrives before the buffered text is flushed, flush first, then dispatch the new prompt |
| 8 | Right before dispatch, refresh the tmux baseline so the next reply cannot replay stale tail output from the prior run |

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
| `busy_flush_after` | duration | `5s` | Maximum hidden-body delay while Codex is still generating |
| `body_flush_after` | duration | `4s` | Idle window before flushing buffered reply text |
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
| Telegram returns `429 Too Many Requests` during edit | Buffered body is retained and next edit attempt waits at least `retry_after` seconds |
| Tmux capture fails temporarily while body is buffered | Buffered body survives reconnect and is delivered before the next prompt dispatch |

## Follow-Ups

1. Expose `working_after`, `busy_flush_after`, `body_flush_after`, and `edit_rollover_at` through YAML if runtime tuning is needed.
2. Decide whether Lark / Feishu should keep the current send-only behavior or gain a similar edit model later.
