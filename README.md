# Subscription Bridge

Subscription Bridge is a small, privacy-preserving service that translates Stripe and Adyen subscription lifecycles into a stable, provider-neutral protocol.
Each v1 deployment sits between exactly one consumer application and both required v1 adapters. The consumer retains usernames, product rules, feature gates, and its local plan catalog. Subscription Bridge retains processor credentials, processor-native identifiers, recurring payment state, and callback delivery state.
The bridge and its processors must never receive consumer usernames or other consumer account identifiers. The only cross-system join identifiers are opaque `checkout_id` and `subscription_ref` values. `plan_id` is a shared catalog key, while `event_id` is an idempotency and audit identifier.

## Project Status

Subscription Bridge is a greenfield project under active development. There are no production deployments and no backward-compatibility requirements unless explicitly documented.
The normative implementation contract is `SPEC.md`. Where this README and `SPEC.md` differ, `SPEC.md` takes precedence.
Arkfile's consumer implementation supports current protocol v1 and passes the canonical cross-repository fixture tests.

## Core Responsibilities

Subscription Bridge:

- Accepts short-lived, HMAC-signed checkout and portal tokens from a consumer application.
- Creates processor-hosted checkout sessions.
- Maintains the canonical processor-facing subscription state.
- Normalizes Stripe and Adyen lifecycle events.
- Delivers immutable, HMAC-signed callbacks to the consumer.
- Exposes an authenticated subscription snapshot endpoint for reconciliation.
- Retries callback delivery safely and idempotently.
- Schedules fenced Adyen renewal actions and provider-neutral canceled-subscription expiry actions without permitting duplicate charges or stale-worker commits.
- Keeps processor credentials and processor-native identifiers off the consumer host.

Subscription Bridge is not a general-purpose billing platform. Version 1 does not provide one-off payments, multiple consumers in one deployment, public merchant signup, tax calculation, coupons, proration, or currency conversion.

## Privacy Boundary

The consumer application may know:

- Username and local account identity
- Local plan catalog
- Feature and storage limits
- `checkout_id`
- `subscription_ref`
- Normalized subscription state

Subscription Bridge may know:

- `checkout_id`
- `subscription_ref`
- Processor customer and subscription identifiers
- Processor payment-method references
- Subscription periods and payment state

One deployment has one consumer webhook URL, one pairing-root set, and one consumer protocol namespace. Separate consumer applications use separate deployments.

The payment processor may know the financial identity and payment instrument required to process payment.
Subscription Bridge and processor metadata must not contain consumer usernames, email addresses used as consumer identifiers, or other consumer account data. An operator with access to both the consumer and bridge databases may correlate opaque identifiers; the architecture reduces unnecessary disclosure but does not claim that such correlation is impossible.

## Protocol

Subscription Bridge Protocol v1 uses:

- `checkout_id` for one checkout attempt
- `subscription_ref` for one ongoing subscription
- `event_id` for one immutable callback event
- `state_version` for strictly ordered state changes
- `state_changed_at` for the commit time of the canonical state represented by that version
- HMAC-SHA256 for tokens, callbacks, and reconciliation authentication
- HKDF-SHA256 to derive purpose-specific keys from one pairing root

The pairing root derives separate keys for:

- Consumer-to-bridge browser tokens
- Bridge-to-consumer callbacks
- Consumer-to-bridge reconciliation requests

The configured pairing root is exactly 64 lowercase hexadecimal characters representing 32 bytes. It is strictly validated, hex-decoded, and never used directly as an HMAC key. Protocol v1 has exactly one active root and no overlapping previous-root verification window.

Opaque `checkout_id`, `subscription_ref`, and `event_id` values use their exact `subchk_`, `sub_`, and `evt_` prefixes, followed by a non-empty ASCII `[A-Za-z0-9_-]+` suffix, with at most 160 characters total. Token encodings and callback/reconciliation HMAC headers are canonical and fail closed on padding, whitespace, reordered or additional components, non-canonical timestamps, or uppercase signatures.

`plan_id` is valid UTF-8, nonempty after Unicode whitespace trimming, and at most 128 bytes in its UTF-8 representation. Ordinary callback and snapshot JSON key order is not canonical; the exact callback bytes first committed to the outbox become authoritative for signing and retries.

See `SPEC.md` for exact payloads, validation rules, key derivation labels, golden vectors, state transitions, and retry behavior.

## Processor Support

Version 1 requires complete, conforming adapters for both:

### Stripe

Stripe owns the subscription schedule. The adapter uses Stripe Checkout in subscription mode, Stripe Billing Portal, authenticated Stripe webhooks, and authoritative subscription retrieval to prevent late events from regressing state.

### Adyen

Subscription Bridge owns the recurring schedule. The adapter uses Adyen Checkout for initial payment and tokenization, stores the resulting payment-method reference in encrypted form, and performs recurring charges through Adyen using stable idempotency keys.

The Adyen implementation includes renewal scheduling, leasing, retry and dunning policy, payment-method replacement, cancellation, and a bridge-hosted portal.

Both adapters must pass the same provider conformance suite.

Provider behavior never leaks into the consumer protocol. `processor_family` is absent from callbacks and snapshots, and consumer entitlement logic must not branch on a provider.

## Lifecycle and delivery guarantees

- `canceled` is non-renewing but remains effective through `current_period_end` and may be authoritatively restored to `active` through `subscription.renewed`; only `expired` is terminal.
- Callbacks and snapshots use the same exact canonical-state fields and `state_changed_at` semantics.
- Authoritative callback bodies are stored as exact bytes and reused unchanged for every retry.
- Delivery has explicit `pending`, `delivered`, `dead_lettered`, and `abandoned` terminal states.
- Callback delivery, Stripe processing, and all scheduled actions use monotonically increasing fencing tokens so expired workers cannot commit.
- Uncertain Adyen attempts reuse the exact encrypted request and idempotency key; unresolved attempts stop in audited manual review.
- Exhausted Adyen dunning creates one fenced expiry action at the bridge's configured billing-termination deadline, independent of consumer access grace.
- Raw provider webhook payloads are discarded after verification and normalization unless an explicitly enabled, encrypted, short-retention diagnostic quarantine is configured.

The canonical cross-repository vectors and exact wire examples are in `fixtures/protocol-v1.json`, introduced by [SubscriptionBridge commit `28c2c99`](https://github.com/arkfile/SubscriptionBridge/commit/28c2c9965d32a44fe2ea572c89fbc4f15662f371). Consumer mirrors must remain byte-for-byte identical and pin that source commit or a later release containing unchanged fixture bytes.

## Architecture

```text
Browser -> TLS proxy -> Subscription Bridge -> Processor API
                              |
                              +-> PostgreSQL
                              |
                              +-> signed callback -> Consumer application
Consumer -> signed browser token -> /v1/start or /v1/portal
Consumer -> authenticated GET -> /v1/subscriptions/{subscription_ref}
Stripe -> authenticated webhook -> /v1/webhooks/stripe -> Stripe adapter
Adyen  -> authenticated webhook -> /v1/webhooks/adyen  -> Adyen adapter
```
