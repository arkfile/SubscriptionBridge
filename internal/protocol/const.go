package protocol

import "time"

const (
	Name    = "subscription-bridge"
	Version = 1

	HKDFSalt      = "subscription-bridge/v1"
	TokenInfo     = "consumer-to-bridge/token"
	CallbackInfo  = "bridge-to-consumer/callback"
	ReconcileInfo = "consumer-to-bridge/reconcile"

	KeySize = 32

	CheckoutPrefix      = "subchk_"
	SubscriptionPrefix  = "sub_"
	EventPrefix         = "evt_"
	ShopperPrefix       = "sbr_"
	AttemptPrefix       = "sba_"
	ActionKeyPrefix     = "act_"
	MaxIdentifierLength = 160
	MaxPlanIDBytes      = 128
	MaxTokenBytes       = 8192

	ClockSkew       = 300 * time.Second
	MaxTokenTTL     = 15 * time.Minute
	ReplayWindow    = 300 * time.Second
	SignatureHexLen = 64

	HMACHeaderName   = "Subscription-Bridge-Signature"
	ReconcileScheme  = "Subscription-Bridge-HMAC"
	ProcessorStripe  = "stripe"
	ProcessorAdyen   = "adyen"
	ActionKeyInfo    = "subscription-bridge/v1/action"
	CanonicalJSONSep = ""
)

const (
	StatusActive   = "active"
	StatusTrialing = "trialing"
	StatusPastDue  = "past_due"
	StatusCanceled = "canceled"
	StatusExpired  = "expired"
)

const (
	EventActivated   = "subscription.activated"
	EventRenewed     = "subscription.renewed"
	EventPastDue     = "subscription.past_due"
	EventCanceled    = "subscription.canceled"
	EventExpired     = "subscription.expired"
	EventPlanChanged = "subscription.plan_changed"
	EventSync        = "subscription.sync"
)

const (
	CheckoutCreating  = "creating"
	CheckoutPending   = "pending"
	CheckoutCompleted = "completed"
	CheckoutExpired   = "expired"
	CheckoutCanceled  = "canceled"
)

const (
	DeliveryPending      = "pending"
	DeliveryDelivered    = "delivered"
	DeliveryDeadLettered = "dead_lettered"
	DeliveryAbandoned    = "abandoned"
)

const (
	ActionRenew  = "renew"
	ActionExpire = "expire"
)
