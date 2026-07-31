# Code map

This file explains behavior in product language so a maintainer can understand the system without following individual lines of Go.

## When a message arrives

The Telegram adapter validates the update, encrypts its JSON with AES-256-GCM, and commits it to the durable inbox before webhook acknowledgement or polling offset advancement. Workers atomically claim the oldest eligible job per user, retry transient failures, and scrub the encrypted payload at a terminal state. This is at-least-once processing: intake is deduplicated by `update_id`, but a reply accepted by Telegram immediately before a worker crash can be delivered again on retry. The bot then refreshes non-sensitive profile metadata, checks private-chat scope and consent, and validates the input. Screenshots and voice notes are downloaded into bounded memory; their secret Telegram URLs are never forwarded in plaintext or retained after the queue job completes.

The quota service atomically reserves the correct daily allowance before the configured AI provider is called. A per-user session keeps the normalized source, scenario state, prior candidates, and revision while marking earlier work obsolete, so a slow old generation cannot appear after newer input. The prompt builder treats the submitted source, saved examples, and previous candidates as untrusted quoted data. Only a fixed enum describing source shape—such as `screenshot` or `forwarded_channel`—is provided as routing evidence; sender identity is not.

The provider must return structured output with a concrete `reply` or `comment` mode and high/low confidence. In auto mode, high confidence produces three candidates with a visible mode label. Low confidence produces a two-button question while retaining the source in memory. Explicit prefixes and mode buttons force the selected mode. Independent application checks enforce mode consistency, exactly three diverse short candidates, and scenario-aware safety. The service stores only the generated result, provider/model identifiers, and a secret-keyed digest of the input; token totals are exported only as content-free aggregate metrics.

## When a refinement button is pressed

The callback signature prevents forged action IDs. The generation/session revision proves that the Telegram user owns the active result. The short-lived session supplies the original context and previous three candidates without requiring plaintext database storage. Reply and comment modes expose different actions. A clarification choice is free; the first correction after an automatic high-confidence selection is also free; normal refinements and later switches use their configured allowance. Every new version makes late previous work obsolete.

## When the mode is switched

The signed callback chooses `reply` or `comment`, starts a new revision, and sends the same normalized text/image bytes to the provider with an explicit mode. Telegram media is not downloaded again and the source digest stays the same. The previous mode's candidates are supplied as untrusted context so the provider can avoid paraphrasing them. This works until the process-local session expires or the process restarts; then the bot asks for the source again.

## When feedback is pressed

Feedback is explicit: Telegram does not reveal native copy-button clicks. A positive or negative callback is stored against the generation only after ownership is checked. Saving a candidate as “my style” is a separate explicit action for reply mode. Comment keyboards omit this global save action so public-joke examples cannot contaminate private-reply style.

## When `/threads` is used

The Belcanto workspace is separate from ordinary reply/comment sessions and quotas. The command first verifies the exact Telegram-ID operator allowlist and normal consent. A dedicated AI contract chooses an evergreen premise and returns one Russian WYSIWYG post; it knows only that Belcanto is a vocal school in Astana. A semantic validator blocks digits, invented mutable facts, direct sales pressure, repetition, and text over 500 Unicode characters.

Every optional refinement writes a new durable revision and makes the previous signed keyboard stale. Pressing `✅ Опубликовать` atomically claims the current owner/revision with a short token-fenced lease before any external call. The Threads adapter creates a text container with the exact preview, stores the container ID, waits for readiness, and writes a separate attempt marker immediately before publishing. An expired lease before that marker can be reclaimed safely; after the marker, only a later `PUBLISHED` status can prove success. Otherwise the draft becomes `unknown`, and the publish request is not repeated. This keeps double taps, stale workers, redelivery, and process crashes from creating a duplicate.

## When data is deleted

The user confirms deletion through a signed callback. Active in-memory context is cancelled, and PostgreSQL cascades deletion through generations, feedback, usage, entitlements, style examples, and Belcanto Threads drafts. The bot sends confirmation only after deletion succeeds.

## How AI providers differ

Anthropic Messages API is the production path. It exposes independent structured contracts for three-candidate reply/comment generation and the one-post Belcanto editor. Claude CLI exists for local/internal reply/comment evaluation and uses one isolated subprocess per request, structured output, no session persistence, and no tools except read-only access to a per-request temporary image when needed. The fake providers make tests and first-run smoke checks deterministic, including a no-network Threads publication path.
