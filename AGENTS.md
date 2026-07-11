# AGENTS.md

`NOTE: Agents, agentic coding tools, and LLMs must read this document before modifying or advising on this project.`

# Subscription Bridge: Guidance for Agents

Subscription Bridge is a privacy-preserving payment-processor abstraction service. It sits between a consumer application and payment processors and translates processor-specific subscription behavior into Subscription Bridge Protocol v1.

The bridge owns processor credentials, processor-native identifiers, recurring payment state, provider webhooks, callback delivery, and the Adyen recurring scheduler. The consumer application owns user identity, account authorization, feature gates, and its local plan catalog.

V1 requires complete Stripe and Adyen adapters. Each bridge deployment serves exactly one consumer application, one consumer webhook URL, one pairing-root set, and one consumer protocol namespace. Multi-consumer and public multi-merchant operation are out of scope.

It is vital to preserve the privacy boundary and the transactional and idempotency guarantees described in `SPEC.md`.

## Required Reading

Before making changes, read:

- `README.md`
- `SPEC.md`
- Relevant database migrations
- Relevant protocol, engine, notifier, scheduler, and adapter tests

`SPEC.md` is normative. Do not change protocol fields, signatures, HKDF labels, state transitions, or retry semantics without updating the specification, golden vectors, consumer contract tests, and cross-repository integration tests together.

## Greenfield Application

This project is greenfield and has no production deployments unless the developer explicitly states otherwise.

Backward compatibility is generally not required during this stage. Prefer a clean and correct design over deprecated aliases, fallback paths, transitional fields, duplicated APIs, or compatibility layers.

Immediately flag deprecated, disabled, stub, incomplete, unreachable, or compatibility-only code. Do not preserve technical debt merely because it already exists.

Do not describe compatibility behavior as necessary unless an actual deployed dependency has been identified.

## Privacy Boundary

The bridge must never receive or store consumer usernames, consumer account IDs, or other consumer identity fields.

The only cross-system join identifiers are:

- `checkout_id`
- `subscription_ref`

Both identifiers must be opaque and must not encode usernames, email addresses, sequential account IDs, tenant names, or other identifying information. `plan_id` is a shared catalog key. `event_id` is an idempotency and audit identifier. Neither should be described as an identity join key.

Every protocol occurrence of `checkout_id`, `subscription_ref`, or `event_id` must use its exact `subchk_`, `sub_`, or `evt_` prefix, a non-empty suffix containing only ASCII letters, digits, `_`, or `-`, and a maximum total length of 160 characters.

Processor metadata must contain only the minimum opaque identifiers required by the protocol. Never place a username, consumer account ID, or application-specific user profile in processor metadata.

Do not claim that the architecture makes correlation impossible. An operator with access to both databases can correlate opaque identifiers. The design goal is data minimization and separation of operational access.

## Financial Data and Provider References

Payment processors necessarily handle financial identity and payment instruments. Treat all processor customer IDs, subscription IDs, payment IDs, shopper references, and stored-payment-method references as sensitive operational data.

Do not:

- Log provider credentials or webhook secrets.
- Log pairing roots or derived HMAC keys.
- Log stored-payment-method references.
- Log processor shopper references.
- Log full provider customer or subscription objects.
- Return provider-native identifiers to consumer applications.
- Include raw provider payloads in ordinary logs or HTTP errors.
- Store card numbers, security codes, or un-tokenized payment credentials.

Adyen stored-payment-method references must be encrypted at the application layer using the configured data-encryption key or an approved managed KMS envelope key.

## Protocol Invariants

Subscription Bridge Protocol v1 uses one consumer pairing root and three HKDF-SHA256-derived keys.

The labels and salt in `SPEC.md` are protocol constants. Do not alter them locally or introduce alternative derivation paths.

The configured pairing root is exactly 64 lowercase hexadecimal characters representing 32 bytes. Reject uppercase, whitespace, non-hex characters, prefixes, and every other length; hex-decode before HKDF. The pairing root must never be used directly as an HMAC key. V1 has exactly one active root and no two-root verification overlap or previous-root fallback.

Every callback must include a stable `event_id` and a strictly monotonic `state_version`.

For each `subscription_ref`:

- State versions begin at 1.
- Every consumer-visible state change increments the version exactly once.
- A state version identifies one immutable outbound payload.
- `(subscription_ref, state_version)` must be unique.
- Retried delivery must reuse the exact stored event ID and callback body bytes.
- Delivery retries must not generate a new state version.
- Duplicate or late processor events must not regress subscription state.
- `state_changed_at` is the UTC transaction time at which the canonical state for that version committed; it is never request, retrieval, or delivery time.
- Only `expired` is terminal. `canceled` remains effective through its period end and may transition back to `active` only through a provider-authoritative `subscription.renewed` transition.

Unknown fields, malformed identifiers, unsupported versions, invalid timestamps, and invalid event/status combinations must fail closed.

Cryptographic comparisons must use constant-time comparison functions.

## Transactional State Changes

A consumer-visible state change and its outbound event must be committed in the same PostgreSQL transaction.

Never:

- Update subscription state and enqueue its callback in separate transactions.
- Send a callback before the transaction commits.
- Increment `state_version` without inserting the corresponding immutable outbound event.
- Mark a provider webhook processed before its state effects are durable.
- Perform a second charge because the result of the first request is uncertain.

Use row locks, unique constraints, stable idempotency keys, leases, and `FOR UPDATE SKIP LOCKED` where defined by the specification.

External API calls cannot be made atomic with PostgreSQL. Design each workflow so process termination before, during, or after an external call can be recovered without duplicate state transitions or duplicate charges.

## Outbound Callback Delivery

The outbound event table is an immutable transactional outbox.

The notifier must:

- Claim due rows with a bounded lease and conditional ownership.
- Send the authoritative stored callback bytes without reconstructing the payload from JSONB.
- Sign each attempt with the derived callback key.
- Treat any 2xx response as delivered.
- Retry only the response classes defined in `SPEC.md`.
- Dead-letter deterministic protocol/configuration 4xx responses.
- Record bounded, non-sensitive error classifications.
- Permit safe replay through the operator CLI.

The authoritative callback body is `BYTEA`, not JSONB. Delivery state is explicitly `pending`, `delivered`, `dead_lettered`, or `abandoned`; terminal rows are not due. Requeue and abandonment are audited and preserve the event ID, state version, and exact body.

Notifier claims use random claim tokens and monotonically increasing fencing tokens. Claim, reclaim, delivery completion, retry scheduling, and terminal transitions must conditionally verify ownership so an expired notifier cannot commit a stale result.

Do not hold a database transaction open during network delivery.

## Processor Adapter Boundaries

Provider-neutral lifecycle logic belongs in the engine, not in Stripe or Adyen HTTP handlers.

Adapters may:

- Verify provider-native signatures.
- Parse provider payloads.
- Call provider APIs.
- Return normalized events and authoritative provider state.
- Translate provider-specific error classes.

Adapters must not independently mutate bridge subscription state or construct consumer callbacks.

The engine is the only component that maps normalized provider events into protocol states and outbound callbacks.

Provider-specific identifiers and status names must not leak into the consumer protocol.

Both Stripe and Adyen must pass the same black-box conformance suite.

## Stripe Requirements

Stripe owns the recurring subscription schedule.

Do not infer successful payment from `checkout.session.completed` alone. Retrieve authoritative Stripe subscription state before activation or transition.

Late Stripe events must not regress state. Insert the provider event idempotently, retrieve current provider state, lock the local subscription, and calculate the transition from authoritative state.

Do not hold a transaction or row lock while retrieving Stripe. Stripe processing leases use random claim tokens and monotonically increasing fencing tokens. Every claim or expired-lease reclaim increments the fence. A worker may commit only when the processing action ID, `running` state, claim token, and fencing token still match; otherwise it discards its observation.

Unknown Stripe price IDs must be quarantined for operator action. Never silently map an unknown price to a local plan.

Use stable Stripe idempotency keys for checkout creation and operator-initiated mutations.

## Adyen Requirements

Subscription Bridge owns the Adyen recurring schedule.

The initial checkout must use a bridge-generated shopper reference and must not use consumer identity.

Recurring charges must use:

- `shopperInteraction=ContAuth`
- `recurringProcessingModel=Subscription`
- A stored payment-method reference
- A stable charge-attempt reference
- A stable Adyen idempotency key

The scheduler must tolerate process termination at every step without causing a duplicate charge.

A transport timeout produces an `uncertain` attempt. Persist the exact canonical request before calling, encrypt it with authenticated envelope encryption and key-version metadata, and replay only those exact plaintext bytes with the same idempotency key within the verified Adyen retention window. Do not claim lookup by idempotency key or merchant reference without a verified supported API. Do not issue another attempt until a definitive refusal. Unresolved attempts become `manual_review`, block all later automatic charging, and leave consumer-visible state `past_due` until audited resolution.

Persist the provider endpoint/API version, merchant account, amount and currency, stable attempt and shopper references, interaction models, idempotency key, canonical request fingerprint, and encrypted exact body. Compute the fingerprint over the normative canonical plaintext before encryption. Stored-payment-method references must never exist in plaintext at rest or logs.

Calendar periods are computed in UTC from the original activation anniversary. Monthly periods clamp to the final day of shorter months.

Refusal classifications and dunning delays must come from centralized configuration or provider policy code, not scattered literals.

Renewals and canceled-subscription expiry use generalized durable scheduled actions. Each action has a stable key derived from subscription, action type, and target period/transition. Every claim increments a monotonically increasing fence. Completion requires a conditional match on action ID, `running` state, claim token, and fence, so an expired worker cannot commit after a newer claimant.

## Database Requirements

PostgreSQL is the production and integration-test database. Do not substitute SQLite for database behavior tests.

Use explicit migrations. Production startup checks schema compatibility but does not silently apply migrations.

Database constraints are part of the security and correctness model. Preserve and test:

- Primary and foreign keys
- Provider-event uniqueness
- Event ID uniqueness
- `(subscription_ref, state_version)` uniqueness
- Charge-attempt idempotency uniqueness
- Valid status constraints
- Scheduler due-row indexes
- Outbound-delivery indexes
- Scheduled-action key uniqueness, due indexes, claim tokens, and fencing tokens
- Provider processing leases and fencing tokens
- Explicit outbox terminal-state constraints

Migrations must be reviewed for lock duration, failure recovery, downgrade implications, and handling of sensitive columns.

Do not add a username column or equivalent consumer identity column to any bridge table.

## Function Review Sanity Checks

When implementing, updating, or reviewing a function, ask:

- Is this function required?
- Is it implemented in a standard and secure way?
- Is it complete, or merely a stub?
- Is it in the correct package?
- Is it reachable and used?
- Can it be deleted or simplified?
- Does it preserve protocol invariants?
- Does it preserve privacy boundaries?
- Is it idempotent where retries are possible?
- Can process termination leave inconsistent state?
- Can concurrent workers execute it safely?
- Could it leak secrets or provider identifiers?
- Does it belong in provider-neutral code or a provider adapter?

Flag questionable behavior rather than silently preserving it.

## Key Configurations and Constants

Protocol constants, HKDF labels, replay windows, callback limits, retry schedules, status mappings, and plan mappings must have one authoritative source.

Do not duplicate security-sensitive constants across packages.

`fixtures/protocol-v1.json` is the canonical machine-readable protocol fixture. Any consumer copy must be byte-identical and identify the source bridge commit or release.

The current fixture source is commit `28c2c9965d32a44fe2ea572c89fbc4f15662f371`. Protocol conformance tests must consume the fixture directly rather than duplicate expected keys, signatures, headers, or JSON bytes.

Plan amount and currency are immutable for an existing `plan_id`. Changing either requires a new plan ID.

Provider selection must come from trusted configuration. Do not accept provider selection from user-controlled query parameters.

Secrets must be read from environment variables, secret files, or an approved secret manager. Never commit real secrets or place them in example configuration.

## HTTP and Input Validation

Apply bounded request bodies, bounded response bodies, server timeouts, client timeouts, and strict JSON decoding.

Reject unknown JSON fields on protocol endpoints.

Reject duplicate and missing JSON fields and trailing JSON values. Token encoding and callback/reconciliation HMAC headers must follow the exact canonical grammar in `SPEC.md`: no Base64 padding, uppercase signature hex, whitespace, reordered fields, duplicate fields, unknown components, leading-zero timestamps, or alternate authentication schemes.

Start and portal tokens require integer `iat` and `exp`, `exp > iat`, a maximum 15-minute lifetime, bounded future issue time, and the specification's clock-skew validation. The first accepted checkout ID is immutably bound to its plan, normalized return URL, processor, and request fingerprint. Conflicting reuse fails; exact replay resumes with the same provider idempotency key.

Public and consumer callback URLs must use HTTPS except for explicit loopback development URLs.

Do not follow unexpected redirects in authenticated server-to-server requests.

Do not expose existence or provider state through materially different unauthenticated response behavior.

Authentication and signature checks must occur before parsing or acting on untrusted provider events, except for the minimum bounded parsing required by a provider’s signature scheme.

Do not retain raw provider webhook payloads by default. After verification, persist only event identity, type, payload hash, processing state, timestamps, and minimum normalized recovery/audit fields. Any diagnostic quarantine is opt-in, separately access-controlled, authenticated-encrypted, automatically deleted within the specification's short maximum retention, and excluded from normal logs, metrics, errors, and CLI output.

HTTP errors returned to callers must be concise and must not include raw provider bodies, SQL errors, secrets, or internal identifiers.

## Logging and Observability

Use structured logs with stable event names and bounded fields.

Log opaque bridge IDs only when operationally necessary. Avoid logging complete callback payloads.

Do not log:

- Pairing roots
- Derived keys
- Provider API keys
- Webhook secrets
- Database passwords
- Stored-payment-method references
- Raw provider payloads
- Authorization headers
- Signed browser tokens

Metrics should use low-cardinality labels. Never use `checkout_id`, `subscription_ref`, event IDs, processor IDs, or error messages as metric labels.

Health endpoints must not disclose configuration or credentials.

## Testing Requirements

Maintain:

- Protocol unit tests
- HKDF and HMAC golden vectors
- State-transition matrix tests
- Duplicate and out-of-order event tests
- Transaction rollback tests
- Outbound retry tests
- Stripe fixture tests
- Adyen fixture tests
- Scheduler crash-point tests
- Stripe and scheduled-action stale-worker fencing tests
- Adyen uncertain-attempt, exact replay, and manual-review tests
- Checkout conflict and provider-timeout idempotency tests
- Callback byte-identity and delivery terminal-state tests
- Token lifetime and pairing-root representation tests
- Canceled-subscription expiry tests
- PostgreSQL integration tests
- Common adapter conformance tests
- Cross-repository consumer tests

Run the established repository commands rather than inventing alternate build paths. Once the repository scaffolding exists, the expected baseline is:

```bash
go test ./...
go test -race ./...
go vet ./...
```
