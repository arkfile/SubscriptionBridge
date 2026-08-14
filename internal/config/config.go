package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/arkfile/SubscriptionBridge/internal/hmac"
	"github.com/arkfile/SubscriptionBridge/internal/protocol"
	"go.yaml.in/yaml/v3"
)

type Config struct {
	PublicURL               string
	ConsumerWebhookURL      string
	PairingRoot             string
	DefaultProcessor        string
	StripeSecretKey         string
	StripeWebhookSecret     string
	AdyenAPIKey             string
	AdyenHMACKey            string
	AdyenClientKey          string
	AdyenLivePrefix         string
	AdyenEnvironment        string
	AdyenDataEncryptionKey  string
	Listen                  string
	DatabaseURL             string
	LogLevel                string
	SchedulerEnabled        bool
	RenewalRetryDelays      []time.Duration
	DunningTermination      time.Duration
	AdyenResolutionDeadline time.Duration
	QuarantineEnabled       bool
	QuarantineMaxRetention  time.Duration
	PlansPath               string
	Catalog                 Catalog
	DevMode                 bool
}

type Catalog struct {
	DefaultProcessor string
	Plans            map[string]Plan
}

type Plan struct {
	Processor   string
	Currency    string
	AmountMinor int64
	Interval    string
	Stripe      StripePlan
	Adyen       AdyenPlan
}

type StripePlan struct {
	PriceID string `yaml:"price_id"`
}

type AdyenPlan struct {
	MerchantAccount string `yaml:"merchant_account"`
	CountryCode     string `yaml:"country_code"`
}

type fileCatalog struct {
	DefaultProcessor string          `yaml:"default_processor"`
	Plans            map[string]Plan `yaml:"plans"`
}

// Load reads bridge configuration from environment variables.
func Load() (Config, error) {
	cfg := Config{
		PublicURL:              strings.TrimSpace(os.Getenv("BRIDGE_PUBLIC_URL")),
		ConsumerWebhookURL:     strings.TrimSpace(os.Getenv("CONSUMER_WEBHOOK_URL")),
		PairingRoot:            strings.TrimSpace(os.Getenv("BRIDGE_CONSUMER_PAIRING_ROOT")),
		DefaultProcessor:       strings.TrimSpace(os.Getenv("BRIDGE_DEFAULT_PROCESSOR")),
		StripeSecretKey:        strings.TrimSpace(os.Getenv("STRIPE_SECRET_KEY")),
		StripeWebhookSecret:    strings.TrimSpace(os.Getenv("STRIPE_WEBHOOK_SECRET")),
		AdyenAPIKey:            strings.TrimSpace(os.Getenv("ADYEN_API_KEY")),
		AdyenHMACKey:           strings.TrimSpace(os.Getenv("ADYEN_HMAC_KEY")),
		AdyenClientKey:         strings.TrimSpace(os.Getenv("ADYEN_CLIENT_KEY")),
		AdyenLivePrefix:        strings.TrimSpace(os.Getenv("ADYEN_LIVE_PREFIX")),
		AdyenEnvironment:       getenv("ADYEN_ENVIRONMENT", "test"),
		AdyenDataEncryptionKey: strings.TrimSpace(os.Getenv("ADYEN_DATA_ENCRYPTION_KEY")),
		Listen:                 getenv("BRIDGE_LISTEN", "127.0.0.1:8081"),
		DatabaseURL:            strings.TrimSpace(os.Getenv("BRIDGE_DATABASE_URL")),
		LogLevel:               getenv("BRIDGE_LOG_LEVEL", "info"),
		SchedulerEnabled:       getenv("BRIDGE_SCHEDULER_ENABLED", "true") != "false",
		QuarantineEnabled:      getenv("BRIDGE_PROVIDER_PAYLOAD_QUARANTINE_ENABLED", "false") == "true",
		PlansPath:              getenv("BRIDGE_PLANS_PATH", "config/plans.yaml"),
	}
	var err error
	cfg.RenewalRetryDelays, err = parseDurations(getenv("BRIDGE_RENEWAL_RETRY_DELAYS", "24h,72h,120h"))
	if err != nil {
		return Config{}, err
	}
	cfg.DunningTermination, err = parseBoundedDuration(getenv("BRIDGE_DUNNING_TERMINATION_DELAY", "0s"), 0, 168*time.Hour)
	if err != nil {
		return Config{}, err
	}
	cfg.AdyenResolutionDeadline, err = parseBoundedDuration(getenv("BRIDGE_ADYEN_RESOLUTION_DEADLINE", "144h"), time.Hour, 144*time.Hour)
	if err != nil {
		return Config{}, err
	}
	cfg.QuarantineMaxRetention, err = parseBoundedDuration(getenv("BRIDGE_PROVIDER_PAYLOAD_QUARANTINE_MAX_RETENTION", "168h"), time.Hour, 168*time.Hour)
	if err != nil {
		return Config{}, err
	}
	if err := cfg.ValidateCore(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// LoadCatalog reads and validates the local plan YAML catalog.
func (c *Config) LoadCatalog() error {
	raw, err := os.ReadFile(c.PlansPath)
	if err != nil {
		return fmt.Errorf("plans: %w", err)
	}
	catalog, err := ParseCatalog(raw)
	if err != nil {
		return err
	}
	if c.DefaultProcessor == "" {
		c.DefaultProcessor = catalog.DefaultProcessor
	}
	c.Catalog = catalog
	return c.ValidatePlans()
}

// ParseCatalog unmarshals the plan catalog YAML.
func ParseCatalog(raw []byte) (Catalog, error) {
	var file fileCatalog
	if err := yaml.Unmarshal(raw, &file); err != nil {
		return Catalog{}, fmt.Errorf("plans yaml: %w", err)
	}
	if file.Plans == nil {
		file.Plans = map[string]Plan{}
	}
	return Catalog{DefaultProcessor: file.DefaultProcessor, Plans: file.Plans}, nil
}

// ValidateCore checks pairing root, URLs, listen address, and database URL.
func (c Config) ValidateCore() error {
	if _, err := hmac.ParsePairingRoot(c.PairingRoot); err != nil {
		return fmt.Errorf("BRIDGE_CONSUMER_PAIRING_ROOT: %w", err)
	}
	if err := protocol.IsHTTPSPublicURL(c.PublicURL); err != nil {
		return fmt.Errorf("BRIDGE_PUBLIC_URL: %w", err)
	}
	if err := protocol.IsHTTPSPublicURL(c.ConsumerWebhookURL); err != nil {
		return fmt.Errorf("CONSUMER_WEBHOOK_URL: %w", err)
	}
	if c.DatabaseURL == "" {
		return fmt.Errorf("BRIDGE_DATABASE_URL is required")
	}
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("BRIDGE_LISTEN: %w", err)
	}
	if ip := net.ParseIP(host); ip != nil && !ip.IsLoopback() && !ip.IsUnspecified() {
		// Allowed; TLS is expected at the reverse proxy.
	}
	return nil
}

// ValidatePlans checks catalog processor mappings and required credentials.
func (c Config) ValidatePlans() error {
	if c.Catalog.DefaultProcessor == "" && c.DefaultProcessor == "" {
		return fmt.Errorf("default processor is required")
	}
	def := c.DefaultProcessor
	if def == "" {
		def = c.Catalog.DefaultProcessor
	}
	if def != protocol.ProcessorStripe && def != protocol.ProcessorAdyen {
		return fmt.Errorf("unknown default processor")
	}
	selected := map[string]bool{}
	for id, plan := range c.Catalog.Plans {
		if err := protocol.ValidatePlanID(id); err != nil {
			return fmt.Errorf("plan %q: %w", id, err)
		}
		proc := plan.Processor
		if proc == "" {
			proc = def
		}
		if proc != protocol.ProcessorStripe && proc != protocol.ProcessorAdyen {
			return fmt.Errorf("plan %q: unknown processor", id)
		}
		if plan.AmountMinor <= 0 || plan.Currency == "" || plan.Interval == "" {
			return fmt.Errorf("plan %q: amount, currency, and interval are required", id)
		}
		if proc == protocol.ProcessorStripe {
			if plan.Stripe.PriceID == "" {
				return fmt.Errorf("plan %q: stripe.price_id is required", id)
			}
			selected[protocol.ProcessorStripe] = true
		}
		if proc == protocol.ProcessorAdyen {
			if plan.Adyen.MerchantAccount == "" || plan.Adyen.CountryCode == "" {
				return fmt.Errorf("plan %q: adyen merchant_account and country_code are required", id)
			}
			selected[protocol.ProcessorAdyen] = true
		}
	}
	if selected[protocol.ProcessorStripe] {
		if c.StripeSecretKey == "" || c.StripeWebhookSecret == "" {
			return fmt.Errorf("stripe credentials are required for configured stripe plans")
		}
	}
	if selected[protocol.ProcessorAdyen] {
		if c.AdyenAPIKey == "" || c.AdyenHMACKey == "" || c.AdyenClientKey == "" || c.AdyenDataEncryptionKey == "" {
			return fmt.Errorf("adyen credentials, client key, and data-encryption key are required for configured adyen plans")
		}
		if c.AdyenEnvironment != "test" && c.AdyenEnvironment != "live" {
			return fmt.Errorf("ADYEN_ENVIRONMENT must be test or live")
		}
		if c.AdyenEnvironment == "live" && c.AdyenLivePrefix == "" {
			return fmt.Errorf("ADYEN_LIVE_PREFIX is required in live")
		}
	}
	return nil
}

// ResolvePlan returns a plan and its processor family from trusted configuration.
func (c Config) ResolvePlan(planID string) (Plan, string, error) {
	plan, ok := c.Catalog.Plans[planID]
	if !ok {
		return Plan{}, "", protocol.ErrUnknownPlan
	}
	proc := plan.Processor
	if proc == "" {
		proc = c.DefaultProcessor
		if proc == "" {
			proc = c.Catalog.DefaultProcessor
		}
	}
	plan.Processor = proc
	return plan, proc, nil
}

// StripePriceMap maps Stripe price IDs to local plan IDs.
func (c Config) StripePriceMap() map[string]string {
	out := map[string]string{}
	for id, plan := range c.Catalog.Plans {
		if plan.Stripe.PriceID != "" {
			out[plan.Stripe.PriceID] = id
		}
	}
	return out
}

// getenv returns a trimmed environment value or fallback.
func getenv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// parseDurations parses a comma-separated positive duration list.
func parseDurations(raw string) ([]time.Duration, error) {
	parts := strings.Split(raw, ",")
	out := make([]time.Duration, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		d, err := time.ParseDuration(p)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("invalid duration %q", p)
		}
		out = append(out, d)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("renewal retry delays required")
	}
	return out, nil
}

// parseBoundedDuration parses a duration and enforces inclusive bounds.
func parseBoundedDuration(raw string, min, max time.Duration) (time.Duration, error) {
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, err
	}
	if d < min || d > max {
		return 0, fmt.Errorf("duration %s out of bounds", d)
	}
	return d, nil
}

// ConsumerWebhookPath extracts the path used in reconciliation HMAC bases.
func ConsumerWebhookPath(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Path == "" {
		return "/", nil
	}
	return u.Path, nil
}
