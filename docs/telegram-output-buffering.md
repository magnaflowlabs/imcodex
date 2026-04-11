# Telegram Output Buffering

## Status

Current behavior for `v2.2.4`. This document describes what ships today, not a
future proposal.

## Goals

- avoid Telegram `429` amplification caused by our own retry behavior
- avoid replaying already delivered body text
- keep long-running replies visible without turning every small delta into a new
  message

## Current Behavior

### Observation vs Delivery Clocks

- tmux snapshot capture now polls every 100ms by default
- active output buffering now attempts a visible body flush every 1 second by
  default instead of waiting 5 seconds
- editable body sync uses its own 1-second cadence
- detached plain-message backlog uses its own 1-second per-chat send spacing
- these clocks are separate; shared `retry_after` backoff can still pause chat
  transport temporarily, but it no longer changes the capture cadence

### Editable Body Path

- Telegram replies still prefer editable messages when the messenger supports
  them.
- Body updates respect the normal `editableSyncEvery` cadence; the default body
  sync interval is now 1 second, including the busy-to-idle transition.
- Repeated or severe editable `429` responses switch body delivery to detached
  queued chunks instead of indefinitely retrying the same editable body.
- A short `[working]` status message may appear first and is cleaned up
  independently from body delivery.

### Detached Queue Path

- Plain detached chunks are queued in order.
- After backoff expires, the queue resumes in order.
- Consecutive queued chunks from the same run may be sent as one larger plain
  message when they still fit within Telegram's safe message size, but batching
  now preserves the exact queued bytes in order.
- A 1-second per-chat spacing is applied even when Telegram is not currently
  rate-limiting.
- Non-`429` transport failures keep the queue head and retry with bounded
  backoff.

### Shared Transport Safety

- send, edit, delete, and chat-action calls all use bounded request timeouts
- Telegram `retry_after` is honored
- a detached `429` blocks editable sends during the same backoff window
- an editable `429` blocks detached sends during the same backoff window
- delivery tracing logs why buffered output is deferred, blocked, or committed

## Behaviors Removed In `v2.2.4`

- detached backlog drain loops that send many chunks immediately after a retry
  window
- watchdog-triggered mutation from editable body delivery to plain detached body
  delivery
- forced editable flushes that bypass the nominal sync interval every time a run
  becomes idle
- body transport calls made with `context.Background()`

## Known Limits

- delivery state is still kept in memory; there is no persisted send journal yet
- a timed-out request can still be ambiguous if Telegram received it but the
  client did not receive the response
- tmux snapshot tracking and delivery tracking are still more coupled than they
  should be

## Related Docs

- [message-delivery-redesign.md](message-delivery-redesign.md): next-step
  simplification plan
- [runtime-v2-docker-tmux.md](runtime-v2-docker-tmux.md): runtime model
