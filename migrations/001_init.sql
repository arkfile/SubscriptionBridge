-- Subscription Bridge schema v1

CREATE DOMAIN sb_utc_second AS TIMESTAMPTZ
    CHECK (VALUE = date_trunc('second', VALUE));

CREATE FUNCTION sb_valid_id(val TEXT, prefix TEXT) RETURNS BOOLEAN
    LANGUAGE sql IMMUTABLE AS $$
        SELECT val LIKE prefix || '%'
           AND char_length(val) BETWEEN char_length(prefix) + 1 AND 160
           AND substring(val FROM char_length(prefix) + 1) ~ '^[A-Za-z0-9_-]+$'
    $$;

CREATE TABLE sb_checkouts (
    checkout_id TEXT PRIMARY KEY CHECK (sb_valid_id(checkout_id, 'subchk_')),
    plan_id TEXT NOT NULL CHECK (octet_length(plan_id) BETWEEN 1 AND 128),
    normalized_return_url TEXT NOT NULL,
    processor_family TEXT NOT NULL CHECK (processor_family IN ('stripe', 'adyen')),
    request_fingerprint BYTEA NOT NULL CHECK (octet_length(request_fingerprint) = 32),
    provider_idempotency_key TEXT NOT NULL UNIQUE,
    processor_shopper_reference TEXT,
    status TEXT NOT NULL CHECK (status IN ('creating', 'pending', 'completed', 'expired', 'canceled')),
    subscription_ref TEXT UNIQUE CHECK (subscription_ref IS NULL OR sb_valid_id(subscription_ref, 'sub_')),
    processor_checkout_id TEXT,
    expires_at sb_utc_second NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK ((processor_family = 'adyen') = (processor_shopper_reference IS NOT NULL))
);

CREATE TABLE sb_subscriptions (
    subscription_ref TEXT PRIMARY KEY CHECK (sb_valid_id(subscription_ref, 'sub_')),
    checkout_id TEXT NOT NULL UNIQUE REFERENCES sb_checkouts(checkout_id),
    plan_id TEXT NOT NULL CHECK (octet_length(plan_id) BETWEEN 1 AND 128),
    status TEXT NOT NULL CHECK (status IN ('active', 'trialing', 'past_due', 'canceled', 'expired')),
    state_version BIGINT NOT NULL CHECK (state_version >= 1),
    processor_family TEXT NOT NULL CHECK (processor_family IN ('stripe', 'adyen')),
    processor_customer_id TEXT,
    processor_subscription_id TEXT,
    processor_initial_payment_id TEXT,
    processor_shopper_reference TEXT,
    payment_method_ciphertext BYTEA,
    payment_method_nonce BYTEA,
    payment_method_key_version TEXT,
    current_period_start sb_utc_second NOT NULL,
    current_period_end sb_utc_second NOT NULL,
    cancel_at_period_end BOOLEAN NOT NULL DEFAULT FALSE,
    state_changed_at sb_utc_second NOT NULL,
    past_due_since TIMESTAMPTZ,
    canceled_at sb_utc_second,
    automatic_charging_blocked BOOLEAN NOT NULL DEFAULT FALSE,
    charging_block_reason TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (current_period_end > current_period_start),
    CHECK ((status = 'canceled') = cancel_at_period_end),
    CHECK ((processor_family = 'adyen') = (processor_shopper_reference IS NOT NULL)),
    CHECK (NOT automatic_charging_blocked OR status = 'past_due'),
    CHECK ((automatic_charging_blocked AND charging_block_reason IS NOT NULL)
        OR (NOT automatic_charging_blocked AND charging_block_reason IS NULL)),
    CHECK ((payment_method_ciphertext IS NULL AND payment_method_nonce IS NULL
            AND payment_method_key_version IS NULL)
        OR (payment_method_ciphertext IS NOT NULL AND payment_method_nonce IS NOT NULL
            AND payment_method_key_version IS NOT NULL))
);
CREATE UNIQUE INDEX idx_sb_subscriptions_processor_sub
    ON sb_subscriptions(processor_family, processor_subscription_id)
    WHERE processor_subscription_id IS NOT NULL;
CREATE UNIQUE INDEX idx_sb_subscriptions_shopper_ref
    ON sb_subscriptions(processor_shopper_reference)
    WHERE processor_shopper_reference IS NOT NULL;
CREATE UNIQUE INDEX idx_sb_subscriptions_initial_payment
    ON sb_subscriptions(processor_family, processor_initial_payment_id)
    WHERE processor_initial_payment_id IS NOT NULL;

CREATE TABLE sb_outbound_events (
    event_id TEXT PRIMARY KEY CHECK (sb_valid_id(event_id, 'evt_')),
    event_type TEXT NOT NULL,
    subscription_ref TEXT NOT NULL REFERENCES sb_subscriptions(subscription_ref),
    checkout_id TEXT NOT NULL REFERENCES sb_checkouts(checkout_id),
    state_version BIGINT NOT NULL,
    payload_body BYTEA NOT NULL,
    payload_json JSONB,
    delivery_state TEXT NOT NULL DEFAULT 'pending'
        CHECK (delivery_state IN ('pending', 'delivered', 'dead_lettered', 'abandoned')),
    attempt_count INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ DEFAULT NOW(),
    delivered_at TIMESTAMPTZ,
    dead_lettered_at TIMESTAMPTZ,
    abandoned_at TIMESTAMPTZ,
    last_error_class TEXT,
    claim_token UUID,
    fencing_token BIGINT NOT NULL DEFAULT 0,
    lease_until TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (
      (delivery_state = 'pending' AND next_attempt_at IS NOT NULL
       AND delivered_at IS NULL AND dead_lettered_at IS NULL AND abandoned_at IS NULL)
      OR (delivery_state = 'delivered' AND next_attempt_at IS NULL
          AND delivered_at IS NOT NULL
          AND dead_lettered_at IS NULL AND abandoned_at IS NULL)
      OR (delivery_state = 'dead_lettered' AND next_attempt_at IS NULL
          AND delivered_at IS NULL AND dead_lettered_at IS NOT NULL
          AND abandoned_at IS NULL)
      OR (delivery_state = 'abandoned' AND next_attempt_at IS NULL
          AND delivered_at IS NULL AND dead_lettered_at IS NULL
          AND abandoned_at IS NOT NULL)
    ),
    CHECK ((claim_token IS NULL) = (lease_until IS NULL)),
    CHECK (delivery_state = 'pending' OR (claim_token IS NULL AND lease_until IS NULL))
);
CREATE UNIQUE INDEX idx_sb_outbound_version
    ON sb_outbound_events(subscription_ref, state_version);
CREATE INDEX idx_sb_outbound_due
    ON sb_outbound_events(next_attempt_at)
    WHERE delivery_state = 'pending';

CREATE TABLE sb_processor_events (
    processor_family TEXT NOT NULL,
    processor_event_id TEXT NOT NULL,
    processing_action_id UUID NOT NULL UNIQUE,
    provider_event_type TEXT NOT NULL,
    payload_hash BYTEA NOT NULL CHECK (octet_length(payload_hash) = 32),
    normalized_fields JSONB NOT NULL,
    sensitive_ciphertext BYTEA,
    sensitive_nonce BYTEA,
    sensitive_key_version TEXT,
    processing_state TEXT NOT NULL DEFAULT 'pending'
        CHECK (processing_state IN ('pending', 'running', 'processed', 'quarantined', 'manual_review')),
    subscription_ref TEXT REFERENCES sb_subscriptions(subscription_ref),
    received_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    processed_at TIMESTAMPTZ,
    claim_token UUID,
    fencing_token BIGINT NOT NULL DEFAULT 0,
    lease_until TIMESTAMPTZ,
    last_error_class TEXT,
    CHECK ((sensitive_ciphertext IS NULL AND sensitive_nonce IS NULL
            AND sensitive_key_version IS NULL)
        OR (sensitive_ciphertext IS NOT NULL AND sensitive_nonce IS NOT NULL
            AND sensitive_key_version IS NOT NULL)),
    CHECK ((processing_state = 'running' AND claim_token IS NOT NULL AND lease_until IS NOT NULL)
        OR (processing_state <> 'running' AND claim_token IS NULL AND lease_until IS NULL)),
    PRIMARY KEY (processor_family, processor_event_id)
);

CREATE TABLE sb_processing_leases (
    processing_key TEXT PRIMARY KEY,
    status TEXT NOT NULL CHECK (status IN ('idle', 'running')),
    active_action_id UUID UNIQUE REFERENCES sb_processor_events(processing_action_id),
    claim_token UUID,
    fencing_token BIGINT NOT NULL DEFAULT 0,
    lease_until TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (
      (status = 'running' AND active_action_id IS NOT NULL
       AND claim_token IS NOT NULL AND lease_until IS NOT NULL)
      OR
      (status = 'idle' AND active_action_id IS NULL
       AND claim_token IS NULL AND lease_until IS NULL)
    )
);

CREATE TABLE sb_provider_event_quarantine (
    processor_family TEXT NOT NULL,
    processor_event_id TEXT NOT NULL,
    ciphertext BYTEA NOT NULL,
    nonce BYTEA NOT NULL,
    key_version TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (expires_at <= created_at + INTERVAL '7 days'),
    PRIMARY KEY (processor_family, processor_event_id),
    FOREIGN KEY (processor_family, processor_event_id)
        REFERENCES sb_processor_events(processor_family, processor_event_id)
        ON DELETE CASCADE
);

CREATE TABLE sb_operator_audit (
    audit_id UUID PRIMARY KEY,
    action TEXT NOT NULL,
    target_type TEXT NOT NULL,
    target_id TEXT NOT NULL,
    actor TEXT NOT NULL,
    reason TEXT NOT NULL,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE sb_scheduled_actions (
    action_id UUID PRIMARY KEY,
    action_key TEXT NOT NULL UNIQUE,
    subscription_ref TEXT NOT NULL REFERENCES sb_subscriptions(subscription_ref),
    action_type TEXT NOT NULL CHECK (action_type IN ('renew', 'expire')),
    target_at sb_utc_second NOT NULL,
    due_at sb_utc_second NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'running', 'uncertain', 'completed',
                          'canceled', 'manual_review')),
    claim_token UUID,
    fencing_token BIGINT NOT NULL DEFAULT 0,
    lease_until TIMESTAMPTZ,
    last_error_class TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK ((status = 'running' AND claim_token IS NOT NULL AND lease_until IS NOT NULL)
        OR (status <> 'running' AND claim_token IS NULL AND lease_until IS NULL))
);
CREATE INDEX idx_sb_scheduled_actions_due
    ON sb_scheduled_actions(due_at)
    WHERE status = 'pending';
CREATE INDEX idx_sb_scheduled_actions_uncertain
    ON sb_scheduled_actions(due_at)
    WHERE status = 'uncertain';

CREATE TABLE sb_charge_attempts (
    attempt_id UUID PRIMARY KEY,
    action_id UUID NOT NULL REFERENCES sb_scheduled_actions(action_id),
    subscription_ref TEXT NOT NULL REFERENCES sb_subscriptions(subscription_ref),
    period_start sb_utc_second NOT NULL,
    period_end sb_utc_second NOT NULL,
    attempt_number INTEGER NOT NULL,
    provider_endpoint TEXT NOT NULL,
    provider_api_version TEXT NOT NULL,
    merchant_account TEXT NOT NULL,
    amount_minor BIGINT NOT NULL CHECK (amount_minor > 0),
    currency TEXT NOT NULL,
    attempt_reference TEXT NOT NULL UNIQUE,
    shopper_reference TEXT NOT NULL,
    shopper_interaction TEXT NOT NULL CHECK (shopper_interaction = 'ContAuth'),
    recurring_processing_model TEXT NOT NULL
        CHECK (recurring_processing_model = 'Subscription'),
    idempotency_key TEXT NOT NULL UNIQUE,
    request_fingerprint BYTEA NOT NULL CHECK (octet_length(request_fingerprint) = 32),
    request_ciphertext BYTEA NOT NULL,
    request_nonce BYTEA NOT NULL,
    request_key_version TEXT NOT NULL,
    processor_payment_id TEXT,
    status TEXT NOT NULL
        CHECK (status IN ('prepared', 'running', 'uncertain', 'authorized',
                          'refused', 'manual_review', 'canceled')),
    claim_token UUID,
    fencing_token BIGINT NOT NULL DEFAULT 0,
    lease_until TIMESTAMPTZ,
    first_submitted_at sb_utc_second,
    resolution_deadline sb_utc_second,
    refusal_reason_code TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at sb_utc_second,
    CHECK (period_end > period_start),
    CHECK (resolution_deadline IS NULL OR first_submitted_at IS NULL
           OR resolution_deadline <= first_submitted_at + INTERVAL '6 days'),
    CHECK ((status IN ('prepared', 'canceled')
            AND first_submitted_at IS NULL AND resolution_deadline IS NULL)
        OR (status NOT IN ('prepared', 'canceled')
            AND first_submitted_at IS NOT NULL AND resolution_deadline IS NOT NULL)),
    CHECK ((status = 'running' AND claim_token IS NOT NULL AND lease_until IS NOT NULL)
        OR (status <> 'running' AND claim_token IS NULL AND lease_until IS NULL)),
    UNIQUE (subscription_ref, period_start, attempt_number)
);
CREATE UNIQUE INDEX idx_sb_charge_attempts_processor_payment
    ON sb_charge_attempts(processor_payment_id)
    WHERE processor_payment_id IS NOT NULL;
