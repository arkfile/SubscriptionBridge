package config_test

import (
	"os"
	"testing"

	"github.com/arkfile/SubscriptionBridge/internal/config"
	"github.com/arkfile/SubscriptionBridge/internal/protocol"
	"github.com/arkfile/SubscriptionBridge/internal/testdata"
)

// TestParseExamplePlans checks that config/plans.example.yaml parses.
func TestParseExamplePlans(t *testing.T) {
	raw, err := os.ReadFile(testdata.ModuleRoot(t) + "/config/plans.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cat, err := config.ParseCatalog(raw)
	if err != nil {
		t.Fatal(err)
	}
	if cat.DefaultProcessor != protocol.ProcessorStripe {
		t.Fatalf("default %s", cat.DefaultProcessor)
	}
	if _, ok := cat.Plans["plan_500gb"]; !ok {
		t.Fatal("missing plan_500gb")
	}
}
