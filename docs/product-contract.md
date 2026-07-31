# Witty Reply — product contract v1

## Promise

Witty Reply is a Telegram co-author. A user forwards a message, pastes text, sends a screenshot, or records a voice note and receives three short replies that sound smart, playful, firm, or meme-like. The bot helps the user answer; it never reads private chats automatically and never sends a reply to another person on the user's behalf.

The product is deliberately positioned as “answer without losing face,” not as a harassment or insult generator.

## Supported inputs

| Input | v1 limit | Handling |
|---|---:|---|
| Text or forwarded text | 6,000 Unicode characters | Treated as the message to answer |
| Screenshot/photo | JPEG, PNG, WebP, or GIF; 10 MiB, 8,000 px/side, 12 MP | Header-checked, decoded under a process-wide resource guard, resized to a 2,000 px long side when necessary, and re-encoded before Claude Vision |
| Voice note | 20 MiB and 5 minutes | Transcribed through the configured OpenAI-compatible speech endpoint |
| Photo caption | Telegram limit | Treated as optional context |

Documents that are images are accepted. Video, animation, sticker, album, and group-chat automation are not part of v1.

## Output

Every successful text generation returns exactly three materially different candidates. Each candidate is at most 240 characters after application safety filtering, within Telegram's 256-character native Copy-button limit. The source language is preserved, including Russian/Kazakh code-switching.

Refinement actions are:

- funnier;
- sharper while remaining safe;
- softer;
- shorter;
- more variants;
- meme card.

The original private input exists as an encrypted durable inbox payload until the update reaches a terminal state, then only in a short-lived in-memory session used for refinements. Plaintext input is not written to PostgreSQL or logs; terminal queue payloads are scrubbed.

Inbound updates are deduplicated by Telegram `update_id` and processed in strict per-user order. Processing and outbound Telegram delivery are at least once, not exactly once: a crash or network ambiguity after Telegram accepts a reply but before the job is marked complete can produce a duplicate bot response.

## Personalization

The profile stores a default tone and replies the user explicitly saves as style examples. These examples are injected at request time. This is not model fine-tuning. Generated Claude outputs and private conversations are not a training dataset for a competing model.

## Consent and deletion

Before the first external request, the user must explicitly agree that submitted content will be processed by the configured AI provider and that voice notes, when enabled, first pass through the configured speech-to-text provider. The consent surface links to the deployment's public `PRIVACY_URL`. `/delete_me` uses a confirmation step, cancels active context, and deletes the user's profile, generations, feedback, usage linkage, and style examples.

## Safety contract

Allowed: irony, friendly teasing, assertive refusal, boundaries, and non-targeted mild profanity when the source naturally uses it.

Blocked or rewritten: threats, doxxing, blackmail, sustained bullying, hate or humiliation based on protected/personal traits, sexual content involving minors, encouragement of self-harm, fraud, and disclosure of private contact details. Text inside an incoming message, image, or transcript is always untrusted data and can never override the system policy.

## Entitlements

Usage is atomically reserved before an expensive provider call. Default free limits are configurable and start at:

| Category | Per day |
|---|---:|
| Text generation | 10 |
| Screenshot or voice | 3 |
| Meme | 1 |
| Refinement | 10 |
| Saved style examples | 5 total |

PostgreSQL entitlements can override limits without changing application code. Billing and Telegram Stars are intentionally outside v1.

## Definition of done

The release must prove: consent gating, an encrypted durable at-least-once inbox with deduplicated Telegram updates, ownership-checked callbacks, exact three-candidate output, input/output safety, atomic quotas, explicit feedback, profile reset, complete deletion, polling and secret-protected webhook modes, no raw-content logging, HTTPS-only production processors and database transport, controlled provider failures, unit and PostgreSQL integration tests with the race detector, container build, and a documented production runbook.
