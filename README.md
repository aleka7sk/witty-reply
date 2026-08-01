# Witty Reply

Telegram-first AI co-author with two distinct scenarios. Send a private message you need to answer, or a post/screenshot you want to comment on. Witty Reply detects the scenario, shows its interpretation, and returns three short candidates that can be copied and refined without resending the source.

The product supports witty teasing and firm boundaries. It is not an automated harassment bot: threats, doxxing, hate, blackmail, and targeted humiliation are filtered and replaced with safe, confident alternatives.

## What is ready

- Telegram long polling for local/single-instance use and secret-protected webhooks for production.
- Text, forwarded text, validated screenshots/image documents, and optional voice transcription.
- Anthropic Messages API with native structured output, images, retries, deadlines, token usage, and Claude Sonnet 5 low-effort defaults.
- Isolated Claude CLI adapter for local/internal API-key-backed evaluation.
- Deterministic fake provider for a zero-cost end-to-end smoke test.
- Auto-first scenario routing with an explicit `↩️ Reply` or `🔥 Comment` label, a one-tap correction, and a two-button clarification only when the source is genuinely ambiguous.
- Exactly three native Telegram-copyable candidates per generation.
- Reply refinements: funnier, sharper, softer, shorter, more, and meme.
- Public-comment refinements: funnier, subtler, bolder, more absurd, shorter, a genuinely different angle, and three more.
- Original 1080×1080 Cyrillic PNG meme cards rendered locally with DejaVu — no scraping or unlicensed template catalog.
- Operator-only Belcanto Threads Copilot: `/threads` asks for a publication goal and optional real material, runs a three-stage scenario/editorial pipeline, then shows one exact school-post preview that can be refined, illustrated, or explicitly published.
- Two-step official Threads API publishing with durable draft state and an atomic claim that blocks duplicate publication after double taps, Telegram redelivery, or a process restart.
- Consent gate, user-owned signed callbacks, atomic daily quotas, feedback, saved style examples, reset, and complete profile deletion.
- AES-256-GCM encrypted durable Telegram inbox with per-user ordering, at-least-once processing, retries, lease recovery, dead-lettering, and payload scrubbing at a terminal state.
- PostgreSQL production persistence and an in-memory development store.
- Raw source text, screenshots, voice bytes, transcripts, Telegram file URLs, and usernames are excluded from application logs. Belcanto's separate operator-only editorial audit logs only locally safe AI-generated finalists so the winner can be compared; it can be reduced to hashes/scores with `metadata` or disabled with `off`. The encrypted update envelope can temporarily contain Telegram-supplied names/usernames, but they are not copied into profile or generation tables and disappear when the terminal queue payload is scrubbed.
- Health, readiness, Prometheus-format metrics, retention cleanup, Docker, Compose, GitHub Actions, race tests, and human-readable architecture/operations docs.

## Quick start

Prerequisites: Go 1.25+ for a native run, or Docker Compose.

### 1. Create the Telegram bot

Create a bot through `@BotFather`, copy its token, and disable group joining with `/setjoingroups`. Witty Reply v2 intentionally works only in private chats.

### 2. Configure

```bash
cp .env.example .env
```

Set at least:

```dotenv
TELEGRAM_BOT_TOKEN=123456:replace-me
TELEGRAM_CALLBACK_SECRET=a-random-secret-at-least-32-characters
AI_PROVIDER=fake
```

`AI_PROVIDER=fake` exercises the entire Telegram experience without making paid AI calls. Unlike a static stub, it has separate reply/comment banks and changes output after refinement buttons.

The fake AI and fake Threads publisher still use the real Telegram Bot API. `TELEGRAM_BOT_TOKEN` must therefore be a valid BotFather token; a placeholder cannot become healthy.

Docker Compose reads `.env` automatically. The Go binary deliberately does not, so a native run must export it first.

### 3. Run

With Docker:

```bash
docker compose config --quiet
docker compose up --build -d
docker compose ps
curl -fsS http://127.0.0.1:8080/readyz
```

The included Compose file is a local-development stack: it starts its own PostgreSQL with `sslmode=disable` and binds the app to loopback. Use a deployment-specific override rather than setting `APP_ENV=production` on this file unchanged.

Or with Go and the in-memory store:

```bash
set -a
. ./.env
set +a
STORE_DRIVER=memory go run ./cmd/witty-reply
```

Open the bot, send `/start`, accept AI processing, then send a message such as:

```text
Тебя вообще никто не спрашивал
```

To exercise the public-comment scenario without a screenshot:

```text
коммент: Многие пациентки не чувствуют шевелений ребёнка
```

The fake provider includes a safe, contextual demo for this example. A real Anthropic provider classifies and writes from the submitted context rather than using the demo bank.

## Two scenarios

| Scenario | Voice | Initial candidates |
|---|---|---|
| `↩️ Reply to the person` | The user answers a specific participant from their own perspective | Smart, playful, firm |
| `🔥 Comment under the post` | An outside reader leaves a standalone public comment | Strongest, subtle, wild |

The default is automatic. Direct address, private-chat structure, forwarded-user metadata, public-post UI, channel/forum metadata, and screenshot layout are used as evidence. Only a non-identifying enum such as `forwarded_channel` reaches the prompt; forwarded names, usernames, and IDs are not used as routing hints.

Explicit leading prefixes override automatic routing:

```text
ответь: <сообщение собеседника>
коммент: <текст публикации>
```

If the model reports low confidence, the bot asks whether to reply or comment. That choice is free and reuses the normalized source already in memory. After an automatically selected result, the first correction to the opposite mode is also free; later switches use the refinement quota.

The source context, including normalized screenshot bytes, is process-local and expires after `SESSION_TTL` of inactivity (30 minutes by default) or a restart. A valid refinement renews that window; stale buttons do not. Mode switches and refinements work without resending while the session exists. An expired button asks the user to submit the source again. Run exactly one application replica until a shared encrypted session store or consistent per-user worker ownership is implemented; sticky HTTP routing alone is insufficient because workers claim jobs from the shared durable inbox.

### 4. Enable Claude in production

```dotenv
AI_PROVIDER=anthropic
ANTHROPIC_API_KEY=sk-ant-...
ANTHROPIC_MODEL=claude-sonnet-5
AI_EFFORT=low
THREADS_AI_MAX_TOKENS=8192
THREADS_AI_CALL_TIMEOUT=90s
THREADS_AI_TIMEOUT=300s
```

For a public product, use a commercial Anthropic API key. Do not route user traffic through an individual Claude Free/Pro/Max OAuth subscription. `claude_cli` exists for operator-controlled development/evaluation and uses secure `--bare` mode with an API key.

## Commands

| Command | Purpose |
|---|---|
| `/start` | Explain processing and request explicit consent |
| `/new` | Forget the current short-lived source context |
| `/cancel` | Cancel the active generation |
| `/style` | View/change the default tone, saved examples, or reset personalization |
| `/plan` | Show plan and current daily use |
| `/privacy` | Show the in-bot privacy summary |
| `/threads` | Choose a goal and material for a publish-ready Belcanto Threads post (operators only) |
| `/delete_me` | Confirm and permanently delete profile data |
| `/help` | Supported inputs and commands |

Recommended BotFather command list:

```text
start - Начать и подтвердить обработку
new - Новый диалог
cancel - Отменить обработку
style - Настроить стиль ответов
plan - Лимиты и тариф
privacy - Приватность
threads - Подготовить пост Belcanto для Threads
delete_me - Удалить мои данные
help - Помощь
```

## Belcanto Threads Copilot

This is a separate, operator-only workspace inside the same Telegram bot. It does not change the ordinary reply/comment flow or consume its quotas.

The operator flow is deliberate and short:

1. Send `/threads` and choose the goal: reach, replies, trust, trial lesson, or community.
2. Send one real text material for the day—a phrase, observation, event, question, verified fact, or teacher note—or explicitly continue without material. The `trial` objective always requires confirmed material because the system cannot invent an offer, date, format, price, capacity, or next step.
3. Receive one exact WYSIWYG preview. Only after this point can the operator refine the text, attach or replace a photo, cancel, or explicitly publish.

The goal and material collection is durable. A Telegram retry reuses the same workflow, and a process restart resumes the current step instead of generating a second draft. The submitted material is treated as quoted, untrusted evidence: it can ground a real scene or fact, but can never issue instructions to the model or authorize invention. For `reach`, `replies`, `trust`, and `community`, continuing without material keeps the post evergreen and inside the small immutable Belcanto fact set; `trial` instead asks for confirmed material before generation.

Anthropic then runs three separate high-effort stages. The concept planner evaluates a deterministic twelve-slot portfolio of market-informed scenarios for the selected goal and material. The writer turns the chosen portfolio into five publication-ready finalists with unique scenario IDs and at least four different mechanisms; generic aphorisms are not a valid portfolio. Finally, a blind editor receives only the safe finalists, applies goal-specific weights, checks grounding and Belcanto fit, and selects the winner. Hidden scratch work and chain-of-thought are neither requested nor logged. Every exposed finalist also passes local fact, language, repetition, length, diversity, delivery-safety, and editorial-quality gates. Only the selected WYSIWYG winner is committed as the current draft and shown in Telegram.

The complete checkable editorial decision is emitted as one structured JSON log event: objective, selected scenario and mechanism, material basis, deterministic and reviewer scores, rejection codes, selected winner, preview delivery status, and a shared `generation_id`. In the default `BELCANTO_REVIEW_LOG_MODE=full`, evergreen generations also include locally safe finalist texts and concise reviewer notes. Material-backed generations automatically suppress every model-authored free-form field and retain only secret-keyed, domain-separated HMAC digests, scores, and bounded IDs so submitted material cannot be echoed or cheaply dictionary-tested from logs. Use `metadata` to apply that content-free shape to every generation or `off` to disable the event. Raw operator material and hidden Anthropic reasoning are never logged. Threads calls use their own `THREADS_AI_MAX_TOKENS=8192` output cap and `THREADS_AI_CALL_TIMEOUT=90s`; the complete planner → writer → reviewer flow has `THREADS_AI_TIMEOUT=300s`. These are safety ceilings, not token-spending targets, so a one- or two-post daily cadence gets enough creative headroom without forcing the provider to consume the cap.

Prometheus counters segment both ready drafts and confirmed publications by bounded objective and `scenario_id`: `belcanto_drafts_ready_objective_*`, `belcanto_drafts_ready_scenario_*`, `belcanto_posts_published_objective_*`, and `belcanto_posts_published_scenario_*`. This makes the editorial mix and publish-through rate measurable without putting material or post text into metric labels.

Pretty-print only these reviews from Compose logs:

```bash
docker compose logs --no-color --no-log-prefix app \
  | jq 'select(.event == "belcanto_threads_editorial_review") | .audit |
    {generation_id, selection_mode, preview_status,
     winner: {id: .selected_winner_id, reason: .decision_reason},
     visual: {mode: .visual_mode, attached: .photo_attached,
       source: .photo_source, asset_id: .photo_asset_id,
       source_page: .photo_source_page, author: .photo_author},
     finalists: [.finalists[] | select(.considered) |
       {rank, id, selected, delivery_safe,
        local: {score: .local_score, flags: .quality_flags},
        review, text}]}'
```

Text-only remains available whenever a photo would be decorative. If the independent editor decides a photographic object or atmosphere adds meaning and `THREADS_PHOTO_PROVIDER=pexels` is configured, it supplies only the visual strategy. The application discards any model-written query and derives a fixed English object-or-space query from the validated `scenario_id`; neither operator material nor generated post text can become a Pexels request. The app then takes the first result that passes its host, metadata, people-hint, size, and decode checks; Anthropic does not inspect or certify that concrete file. The app downloads it from the allowlisted Pexels CDN, re-encodes it as JPEG to remove EXIF/GPS, and stores source/author attribution. A failed search never blocks the winning text. The operator sees the exact combined preview and remains the final editor: the suggested photo can be removed or replaced with a real Belcanto photo. No image is generated by AI, but a public stock API cannot provide a cryptographic guarantee about how every uploaded file was created, so the strictest non-AI option is the school's own verified media library.

The draft also stores that application-derived fallback query when the editor recommends `text_only` or an authentic Belcanto photo. This makes the visual mode a recommendation rather than a lock. Every current preview exposes the applicable controls: `📷 Подобрать фото в Pexels`, `🔄 Другое фото из Pexels`, `🖼 Загрузить своё фото`, and `📝 Только текст`. A manual search or replacement derives the query again from the stored scenario and never changes the post text. The previous preview remains authoritative until the new asset has been downloaded, normalized, and atomically attached; a retry after Telegram delivery failure reuses the exact committed asset instead of searching twice.

Manual visual changes are separate from the immutable generation-time editorial review. Inspect the latest successful override with:

```bash
docker compose logs --no-color --no-log-prefix app \
  | jq -s 'map(select(.event == "belcanto_threads_media_changed")) | last | .audit'
```

An operator-supplied image is resized to the Threads limit, re-encoded as JPEG to remove EXIF/GPS metadata, stored with the owned draft, and never sent to Anthropic. The confirmation label records that Belcanto may use the image and has consent from identifiable people (and a legal representative for children). For a Pexels illustration it additionally asks the operator to confirm that the context does not imply endorsement by a depicted person.

The no-material path is deliberately fact-closed: it knows only the immutable Belcanto facts supplied by the application. A material-backed post may use only the bounded operator evidence from that workflow. Both paths reject unsupported prices, discounts, trial terms, students, teachers, testimonials, results, events, schedules, availability, addresses, promotions, and current happenings.

### Safe local smoke test

```dotenv
BELCANTO_OPERATOR_IDS=123456789
THREADS_PROVIDER=fake
THREADS_PHOTO_PROVIDER=disabled
BELCANTO_REVIEW_LOG_MODE=full
```

Restart the app, accept the normal consent gate, and send `/threads`. Choose each goal at least once, use confirmed text material for `trial`, and test the explicit no-material path with one of the other four goals. Then inspect the exact preview. The fake publisher exercises confirmation and durable idempotency without contacting Meta.

### Connect the Belcanto Threads account

Create a Meta app with the Threads API use case and grant the account at least `threads_basic` and `threads_content_publish`. Configure the long-lived user token and its returned Threads user ID:

```dotenv
BELCANTO_OPERATOR_IDS=123456789
THREADS_PROVIDER=meta
THREADS_USER_ID=17840000000000000
THREADS_ACCESS_TOKEN=TH...
THREADS_API_BASE_URL=https://graph.threads.net/v1.0
# Needed for image posts when TELEGRAM_WEBHOOK_URL is not this public origin:
THREADS_MEDIA_BASE_URL=https://bot.example.com
# Optional licensed illustrations:
THREADS_PHOTO_PROVIDER=pexels
PEXELS_API_KEY=...
```

During a multi-process rolling deployment, enable `THREADS_PHOTO_PROVIDER=pexels` only after every process runs this version. The documented single-replica deployment is unaffected. After restart, even a post whose editorial audit says `visual_mode: "text_only"` can be changed manually from its Telegram preview; `photo_query: null` in an older audit is not a Pexels credential failure.

For an image post, Meta must be able to fetch the normalized JPEG from this deployment. `THREADS_MEDIA_BASE_URL` is an HTTPS origin only (no path); it defaults to `TELEGRAM_WEBHOOK_URL`. It is optional for text-only operation: if neither origin is configured, image publication fails closed without affecting text publication.

After restart, confirmation performs the official two-step flow: create the exact text or image container, wait until it is ready, persist its ID, then publish that exact preview. A short fenced lease makes a crash before the irreversible call safely recoverable. Once `threads_publish` has started, the bot reconciles only through container status and never repeats the call without proof that it is safe. A second tap cannot create a duplicate; an unprovable outcome becomes `unknown` and requires a manual account check.

Meta setup references: [Get started](https://developers.facebook.com/documentation/threads/get-started), [publishing posts](https://developers.facebook.com/documentation/threads/posts), and [access tokens](https://developers.facebook.com/documentation/threads/get-started/get-access-tokens-and-permissions).

## Input limits

| Input | Default |
|---|---:|
| Text | 6,000 Unicode characters |
| Screenshot/image | 10 MiB, 8,000 px per side, 12 MP before normalization |
| Voice | 20 MiB and 5 minutes |
| Copyable reply | 240 characters after safety filtering |

Images are header-checked before full decoding, rejected above 12 MP or 8,000 px on either side, reduced to a 2,000 px long side when necessary, and re-encoded before Claude sees them. This strips EXIF/GPS metadata and prevents the secret Telegram file URL from leaving the service.

## Voice notes

Voice is optional because Claude Messages API does not accept audio directly. Configure an HTTPS OpenAI-compatible transcription endpoint:

```dotenv
SPEECH_PROVIDER=openai_compatible
SPEECH_BASE_URL=https://api.openai.com/v1
SPEECH_API_KEY=replace-me
SPEECH_MODEL=whisper-1
```

With `SPEECH_PROVIDER=disabled`, the bot gives a clear text fallback instead of failing.

## Deployment modes

Local polling:

```dotenv
APP_ENV=development
TELEGRAM_MODE=polling
```

Production webhook:

```dotenv
APP_ENV=production
PRIVACY_URL=https://github.com/aleka7sk/witty-reply/blob/main/docs/privacy-policy.md
TELEGRAM_API_BASE_URL=https://api.telegram.org
TELEGRAM_MODE=webhook
TELEGRAM_WEBHOOK_URL=https://bot.example.com
TELEGRAM_WEBHOOK_PATH=/telegram/webhook
TELEGRAM_WEBHOOK_SECRET=a_bot_api_compatible_secret
STORE_DRIVER=postgres
DATABASE_URL=postgres://user:password@db.example/witty?sslmode=verify-full
AI_PROVIDER=anthropic
ANTHROPIC_BASE_URL=https://api.anthropic.com
```

Production configuration fails closed unless the public privacy URL, official HTTPS Telegram API, secret-protected webhook, TLS-only PostgreSQL, and HTTPS Anthropic API are configured. An enabled speech provider must also use HTTPS. Put TLS termination in front of port `8080`; Telegram requires public HTTPS.

The durable inbox deduplicates intake by Telegram `update_id`, but processing and outbound delivery are intentionally at least once. In the rare case that Telegram accepts a reply and the worker crashes before recording job completion, the retry can send the same reply again; exactly-once delivery is not claimed.

## Endpoints

- `GET /healthz` — process liveness.
- `GET /readyz` — PostgreSQL/store readiness.
- `GET /metrics` — content-free Prometheus exposition.
- `POST /telegram/webhook` — Telegram only; secret header required in webhook mode.

## Development checks

```bash
make check
```

Equivalent explicit commands:

```bash
gofmt -w .
go vet ./...
go test -race ./...
go build ./cmd/witty-reply
```

CI also runs `staticcheck` and `govulncheck` on the current Go runner.

PostgreSQL integration tests use an isolated temporary schema and run whenever `TEST_DATABASE_URL` is set; otherwise they skip locally. CI supplies a PostgreSQL 17 service automatically.

## Documentation

- [Product contract](docs/product-contract.md)
- [Architecture](docs/architecture.md)
- [Privacy policy](docs/privacy-policy.md)
- [Operations runbook](docs/operations.md)
- [Human-readable code map](docs/CODEMAP.md)

## Intentional v2 boundaries

No automatic access to private chats, no autonomous publication, no group operation, no scraped memes, no Telegram Stars billing, no mobile app, and no fine-tuning on Claude outputs. Ordinary Witty Reply never sends or publishes; the operator-only Belcanto workspace publishes only the exact preview whose `✅ Опубликовать` button was explicitly pressed. Personalization uses only explicit profile settings and style examples at prompt time.

The codebase is a modular monolith on purpose. The first product goal is to validate response quality and repeat use; Kubernetes, Kafka, and microservices would add operating cost without improving that experiment.

## License

Copyright © 2026 Alisher Tolegenov. All rights reserved. See [LICENSE](LICENSE).
