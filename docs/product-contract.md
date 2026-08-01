# Witty Reply — product contract v2

## Promise

Witty Reply is a Telegram co-author with two products inside one frictionless flow:

- **Reply to the person** — write from the user's perspective to a specific conversation participant.
- **Comment under the post** — write as an outside reader leaving a standalone, witty public comment.

A user forwards a message, pastes text, sends a screenshot, or records a voice note. The service selects a scenario automatically, makes the selection visible, and returns three candidates. It never reads private chats automatically. The ordinary reply/comment product never posts or replies on the user's behalf; the isolated Belcanto operator workspace can publish only the exact first-party post explicitly confirmed with its signed publish button.

The product is deliberately positioned as “answer without losing face,” not as a harassment or insult generator.

## Supported inputs

| Input | v2 limit | Handling |
|---|---:|---|
| Text or forwarded text | 6,000 Unicode characters | Classified as a private message or public publication; a leading `ответь:`/`коммент:` overrides auto mode |
| Screenshot/photo | JPEG, PNG, WebP, or GIF; 10 MiB, 8,000 px/side, 12 MP | Header-checked, decoded under a process-wide resource guard, resized to a 2,000 px long side when necessary, and re-encoded before Claude Vision |
| Voice note | 20 MiB and 5 minutes | Transcribed through the configured OpenAI-compatible speech endpoint |
| Photo caption | Telegram limit | Treated as optional context |

Documents that are images are accepted. Video, animation, sticker, album, and group-chat automation are not part of v2.

## Output

Every successful generation resolves to `reply` or `comment` and returns exactly three materially different candidates. Each candidate is at most 240 characters after application safety filtering, within Telegram's 256-character native Copy-button limit. The source language is preserved, including Russian/Kazakh code-switching.

Auto mode is not silent. A high-confidence result displays the selected scenario and a correction button. Low confidence displays only two scenario buttons; the user chooses without resending the source or spending a refinement. The first correction after a high-confidence automatic selection is also free. Explicit prefixes and mode buttons always override model classification.

Reply candidates are smart, playful, and firm. Comment candidates are the strongest overall, a subtler angle, and a wilder angle. Comment generation silently explores multiple comedic mechanisms and ranks them for relevance, surprise, brevity, naturalness, and likeability before exposing the best three.

Reply refinements are:

- funnier;
- sharper while remaining safe;
- softer;
- shorter;
- more variants;
- meme card.

Comment refinements are: funnier, subtler, bolder, more absurd, shorter, a different comedic premise, and three more. Every refinement receives the original source plus the previous three candidates; “different angle” must change the premise rather than paraphrase it.

The original private input exists as an encrypted durable inbox payload until the update reaches a terminal state, then only in a short-lived in-memory session used for refinements and mode switching. Plaintext input is not written to PostgreSQL or logs; terminal queue payloads are scrubbed. The no-resend experience is guaranteed only within the configured session TTL and the same running process.

Inbound updates are deduplicated by Telegram `update_id` and processed in strict per-user order. Processing and outbound Telegram delivery are at least once, not exactly once: a crash or network ambiguity after Telegram accepts a reply but before the job is marked complete can produce a duplicate bot response.

## Personalization

The profile stores a default reply tone and replies the user explicitly saves as style examples. Comment results deliberately do not show the global style-save buttons so public jokes cannot silently contaminate private-reply personalization. This is not model fine-tuning. Generated Claude outputs and private conversations are not a training dataset for a competing model.

## Belcanto Threads workspace

`/threads` is available only to a configured allowlist of Belcanto Telegram operators who have accepted the normal AI-processing consent. It starts a three-step operator flow rather than guessing a post from nothing:

1. choose one publication objective;
2. send up to 6,000 Unicode characters of real text material for the day, or—for every objective except `trial`—explicitly choose `✨ Без материала дня`;
3. inspect one complete WYSIWYG Russian preview before any refinement, media selection, or publication action exists.

The objective choices have stable product meaning:

| Objective | Intended reader outcome |
|---|---|
| `reach` | Stop, recognize the topic, and open the conversation beyond active vocal-school shoppers |
| `replies` | Give a natural, specific reason to answer rather than generic engagement bait |
| `trust` | Understand how Belcanto thinks or works through grounded, useful evidence |
| `trial` | Reduce uncertainty around the next step without pressure or invented commercial terms |
| `community` | See concrete participation, shared music, or belonging rather than an abstract “community” claim |

Material kind is either `text` or `none`. Text may contain a real phrase, observation, event, question, verified fact, or teacher note. It is quoted untrusted evidence, not an instruction channel, and it never permits the system to add unsupported details. The no-material path remains evergreen and fact-closed, and is valid only for `reach`, `replies`, `trust`, and `community`. The `trial` objective requires confirmed text material; its no-material action is rejected before an AI call because the system cannot invent an offer, date, format, price, capacity, or next step. Goal and material collection is an owned durable brief: retries resolve to the same start/material operation, restart resumes the current step, and a committed generation atomically links the brief to exactly one draft.

The editorial engine uses three independent high-effort calls. A concept planner compares twelve market-informed scenario slots against the selected objective and material; a material-backed plan reserves at least four evidence-dependent slots. A portfolio writer returns five publication-ready finalists with unique scenario IDs, at least four different mechanisms, and—when material exists—at least two evidence-backed options. Objective-specific scenario anchors prevent irrelevant portfolios. A blind reviewer applies objective-specific weights, evaluates human voice and grounding, and selects one winner; with material, that winner must be evidence-backed. Material-backed review failure is fail-closed. Local validation rejects unsupported evidence, unsafe or malformed output, near-duplicates, generic AI language, and a portfolio made of interchangeable aphorisms. Only the winner becomes a durable WYSIWYG draft.

Without operator material, the generator may use only immutable Belcanto facts supplied by the application. With material, every mutable claim must be grounded in that bounded evidence. It must not invent or imply prices, discounts, trial terms, students, teachers, testimonials, results, events, schedules, availability, addresses, promotions, or current happenings. A dedicated schema, evidence validator, 500-character safety filter, scenario-diversity gate, and bounded repair policy apply before a draft becomes publishable.

Text-only remains available for every preview. When Pexels is configured, the independent editor may recommend a licensed object-or-space photograph, but the application discards any model-written query and derives a fixed query from the validated scenario ID. The operator can also request a Pexels suggestion manually, request a different asset, remove it, or replace it with a verified Belcanto photo; manual searches use the same application-derived query boundary. A signed control can put the durable draft into an image-waiting state; the next single operator photo is normalized and shown with the exact caption. No image is generated by AI, and no visual is published without the exact preview and explicit rights confirmation. Other signed controls request a new premise, shorter/warmer/wittier treatment, removal of sales cues, or a switch between the Belcanto and Alisher voices.

Every preview is stored as an owned, versioned durable draft. Attached media is owner-scoped, bounded to 8 MiB, re-encoded as metadata-free JPEG, and never sent to the AI provider. Creating a refinement or changing format advances the revision and makes prior keyboards stale. Publishing atomically changes the current draft from `draft` or retryable `failed` to `publishing` and issues a fenced short lease before any Meta call. The official Threads client creates the matching text or image container, persists its ID, waits for readiness, and durably marks the attempt immediately before the irreversible publish call. Meta receives the image through a short-lived signed HTTPS capability URL that contains neither Telegram file credentials nor user identity. A stale worker cannot finalize a newer claim. A crash before the attempt marker is recoverable; a crash or ambiguous result afterward is never blindly retried and becomes `unknown` unless a later container status proves it was published.

With `THREADS_PROVIDER=disabled`, missing Meta credentials do not prevent Witty Reply or draft generation from running. Confirmation fails closed without making an HTTP request. Selecting `meta` is an explicit production configuration and therefore fails startup when its required credentials are incomplete. A public media origin is optional so an existing text-only polling deployment remains compatible; only image publication is unavailable without it. Durable briefs—including raw operator material—generated Threads drafts, attached photos, scenario metadata, and publication identifiers are removed by `/delete_me` and normal content-retention cleanup. Material is never included in logs or metrics.

## Consent and deletion

Before the first external request, the user must explicitly agree that submitted content will be processed by the configured AI provider and that voice notes, when enabled, first pass through the configured speech-to-text provider. The consent surface links to the deployment's public `PRIVACY_URL`. `/delete_me` uses a confirmation step, cancels active context, and deletes the user's profile, generations, feedback, usage linkage, and style examples.

## Safety contract

Allowed: irony, friendly teasing, assertive refusal, boundaries, and non-targeted mild profanity when the source naturally uses it. Public humour should target wording, contradiction, behaviour, or a universal situation.

Blocked or rewritten: threats, doxxing, blackmail, sustained bullying, hate or humiliation based on protected/personal traits, body-shaming, jokes targeting weight/pregnancy/health/disability or another vulnerability, sexual content involving minors, encouragement of self-harm, fraud, and disclosure of private contact details. Text inside an incoming message, image, transcript, saved example, or previous candidate is always untrusted data and can never override the system policy. Safety fallbacks are scenario-aware: a rejected public comment cannot become a defensive private-chat reply.

## Entitlements

Usage is atomically reserved before an expensive provider call. Default free limits are configurable and start at:

| Category | Per day |
|---|---:|
| Text generation | 10 |
| Screenshot or voice | 3 |
| Meme | 1 |
| Refinement | 10 |
| Saved style examples | 5 total |

PostgreSQL entitlements can override limits without changing application code. Billing and Telegram Stars are intentionally outside v2.

## Definition of done

The release must prove: consent gating, an encrypted durable at-least-once inbox with deduplicated Telegram updates, ownership-checked callbacks, exact three-candidate reply/comment output, auto/explicit scenario routing, low-confidence clarification, no-resend mode switching, scenario-specific refinements and fallbacks, input/output safety, atomic/free-correction quota semantics, explicit feedback, profile reset, complete deletion, polling and secret-protected webhook modes, no raw-content logging, HTTPS-only production processors and database transport, controlled provider failures, unit and PostgreSQL integration tests with the race detector, static analysis, container build, and a documented production runbook. The Belcanto slice additionally proves operator authorization; durable, replay-safe goal/material collection; twelve concept slots; five scenario-distinct finalists spanning at least four mechanisms; objective-aware blind review; evidence-grounded output and fail-closed material review; atomic brief-to-draft commit; replay of an already committed preview without a second AI call or duplicate draft; stale-revision rejection; fenced publish leases; recovery before the irreversible phase; no-repeat handling afterward; a persisted Threads container ID; safe disabled configuration; retention/deletion of raw material; and an `unknown` terminal state for an unprovable publication.
