# Telegram Output Buffering

## Status

Current behavior for `v2.2.23`. This document describes what ships today, not a
future proposal.

## Goals

- avoid Telegram `429` amplification caused by our own retry behavior
- avoid replaying already delivered body text
- prefer dropping current-run body text over storing files or replaying backlog
- keep long-running replies visible without turning every small delta into a new
  message

## Current Behavior

### Observation vs Delivery Clocks

- tmux snapshot capture now polls every 100ms by default
- active output buffering now attempts a visible body flush every 1 second by
  default instead of waiting 5 seconds
- editable body sync uses its own 1-second cadence
- detached plain-message output for non-editable transports uses its own
  1-second per-chat send spacing
- these clocks are separate; shared `retry_after` backoff can still pause chat
  transport temporarily, but it no longer changes the capture cadence

### Editable Body Path

- Telegram replies still prefer editable messages when the messenger supports
  them.
- Body updates respect the normal `editableSyncEvery` cadence; the default body
  sync interval is now 1 second, including the busy-to-idle transition.
- Editable `429`, delivery timeout, or oversized body output drops the current
  run body. Later chunks from that same run are discarded until a new run starts.
- Dropped body text is not written to files and is not replayed after backoff.
- A short `[working]` status message may appear first and is cleaned up
  independently from body delivery.

### Detached Queue Path

- Plain detached chunks are queued in order.
- Detached `429` or delivery timeout drops the detached backlog instead of
  resuming it after backoff.
- Detached queues have conservative item and rune caps; exceeding either cap
  drops the current run body.
- Per-run detached baselines record what has already been accepted into the
  queue, so later pane reset/rewrite snapshots only enqueue the unsent tail.
- Consecutive queued chunks from the same run may be sent as one larger plain
  message when they still fit within Telegram's safe message size, but batching
  now preserves the exact queued bytes in order.
- A 1-second per-chat spacing is applied even when Telegram is not currently
  rate-limiting.
- Non-`429`, non-timeout transport failures keep the queue head and retry with
  bounded backoff.

### Shared Transport Safety

- send, edit, delete, and chat-action calls all use bounded request timeouts
- Telegram `retry_after` is honored
- a detached `429` drops detached backlog and pauses chat transport during the
  same backoff window
- an editable `429` drops the current run body and pauses chat transport during
  the same backoff window
- delivery tracing logs why buffered output is waiting, dropped, or committed

## Drop Policy

- Drop scope is the current Codex run body, not the user request queue.
- A new user request starts a fresh run and clears the drop state.
- Previously synced Telegram messages are preserved; only unsent body tail is
  discarded.
- This is intentional: losing verbose output is safer than sending hundreds of
  catch-up messages after Telegram backpressure.

## Behaviors Removed In `v2.2.4`

- detached backlog drain loops that send many chunks immediately after a retry
  window
- watchdog-triggered mutation from editable body delivery to plain detached body
  delivery
- forced editable flushes that bypass the nominal sync interval every time a run
  becomes idle
- body transport calls made with `context.Background()`
- `429` fallback from editable delivery into detached catch-up messages

## Known Limits

- delivery state is still kept in memory; there is intentionally no persisted
  output journal or spill file
- a timed-out request can still be ambiguous if Telegram received it but the
  client did not receive the response
- tmux snapshot tracking and delivery tracking are still more coupled than they
  should be

## Related Docs

- [message-delivery-redesign.md](message-delivery-redesign.md): next-step
  simplification plan
- [runtime-v2-docker-tmux.md](runtime-v2-docker-tmux.md): runtime model
