# Subscription Bridge

Subscription Bridge is a small, privacy-preserving service that translates payment-processor subscription lifecycles into a stable, provider-neutral protocol for consumer applications.
It sits between a consumer application and payment processors such as Stripe or Adyen. The consumer retains usernames, product rules, feature gates, and its local plan catalog. Subscription Bridge retains processor credentials, processor-native identifiers, recurring payment state, and callback delivery state.
The bridge and its processors must never receive consumer usernames or other consumer account identifiers. The systems are joined only through opaque `checkout_id` and `subscription_ref` values.

## Project Status

Subscription Bridge is a greenfield project under active development. There are no production deployments and no backward-compatibility requirements unless explicitly documented.
The normative implementation contract is `SPEC.md`. Where this README and `SPEC.md` differ, `SPEC.md` takes precedence.

## Core Responsibilities

Subscription Bridge:

- Accepts short-lived, HMAC-signed checkout and portal tokens from a consumer application.
- Creates processor-hosted checkout sessions.
- Maintains the canonical processor-facing subscription state.
- Normalizes Stripe and Adyen lifecycle events.
- Delivers immutable, HMAC-signed callbacks to the consumer.
- Exposes an authenticated subscription snapshot endpoint for reconciliation.
- Retries callback delivery safely and idempotently.
- Schedules Adyen recurring charges without permitting duplicate charges.
- Keeps processor credentials and processor-native identifiers off the consumer host.

Subscription Bridge is not a general-purpose billing platform. Version 1 does not provide one-off payments, public merchant signup, tax calculation, coupons, proration, or currency conversion.

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

The payment processor may know the financial identity and payment instrument required to process payment.
Subscription Bridge and processor metadata must not contain consumer usernames, email addresses used as consumer identifiers, or other consumer account data. An operator with access to both the consumer and bridge databases may correlate opaque identifiers; the architecture reduces unnecessary disclosure but does not claim that such correlation is impossible.

## Protocol

Subscription Bridge Protocol v1 uses:

- `checkout_id` for one checkout attempt
- `subscription_ref` for one ongoing subscription
- `event_id` for one immutable callback event
- `state_version` for strictly ordered state changes
- HMAC-SHA256 for tokens, callbacks, and reconciliation authentication
- HKDF-SHA256 to derive purpose-specific keys from one pairing root

The pairing root derives separate keys for:

- Consumer-to-bridge browser tokens
- Bridge-to-consumer callbacks
- Consumer-to-bridge reconciliation requests

The pairing root is never used directly as an HMAC key.

See `SPEC.md` for exact payloads, validation rules, key derivation labels, golden vectors, state transitions, and retry behavior.

## Processor Support

Version 1 requires complete adapters for:

### Stripe

Stripe owns the subscription schedule. The adapter uses Stripe Checkout in subscription mode, Stripe Billing Portal, authenticated Stripe webhooks, and authoritative subscription retrieval to prevent late events from regressing state.

### Adyen

Subscription Bridge owns the recurring schedule. The adapter uses Adyen Checkout for initial payment and tokenization, stores the resulting payment-method reference in encrypted form, and performs recurring charges through Adyen using stable idempotency keys.

The Adyen implementation includes renewal scheduling, leasing, retry and dunning policy, payment-method replacement, cancellation, and a bridge-hosted portal.

Both adapters must pass the same provider conformance suite.

## Architecture

```text
Browser -> TLS proxy -> Subscription Bridge -> Processor API
                              |
                              +-> PostgreSQL
                              |
                              +-> signed callback -> Consumer application
Consumer -> signed browser token -> /v1/start or /v1/portal
Consumer -> authenticated GET -> /v1/subscriptions/{subscription_ref}
Processor -> authenticated webhook -> provider adapter
```
