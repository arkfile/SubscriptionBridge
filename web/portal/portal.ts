// Build: bun web/portal/build.ts

interface PortalConfig {
  available: boolean;
  clientKey: string;
  environment: string;
  sessionId: string;
  sessionData: string;
}

interface AdyenCheckoutInstance {
  readonly _brand: "AdyenCheckout";
}

interface DropinInstance {
  mount(selector: string): DropinInstance;
}

interface AdyenWebGlobal {
  AdyenCheckout: (config: Record<string, unknown>) => Promise<AdyenCheckoutInstance>;
  Dropin: new (checkout: AdyenCheckoutInstance, options?: Record<string, unknown>) => DropinInstance;
}

const CARD_STYLES = {
  base: {
    color: "#cce7ff", // --foam-1
    fontFamily:
      "system-ui, -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, Oxygen, Ubuntu, Cantarell, sans-serif",
    fontSize: "16px",
    fontSmoothing: "antialiased",
  },
  error: { color: "#e05c5c" }, // --coral
  placeholder: { color: "#8ab8d8" }, // --foam-2
  validated: { color: "#00d4aa" }, // --biolum
};

// readConfig loads the server-provided Drop-in session from the page.
function readConfig(): PortalConfig | null {
  const el = document.getElementById("portal-config");
  if (!el || !el.textContent) {
    return null;
  }
  return JSON.parse(el.textContent) as PortalConfig;
}

// setStatus shows a success or error message next to Drop-in.
function setStatus(kind: "ok" | "err", message: string): void {
  const el = document.getElementById("dropin-status");
  if (!el) {
    return;
  }
  el.hidden = false;
  el.className = kind === "ok" ? "status-ok" : "status-err";
  el.textContent = message;
}

// mountDropin initializes Adyen Drop-in for payment-method replacement.
async function mountDropin(): Promise<void> {
  const cfg = readConfig();
  if (!cfg || !cfg.available) {
    setStatus("err", "Payment method update is unavailable.");
    return;
  }
  const web = (window as unknown as { AdyenWeb?: AdyenWebGlobal }).AdyenWeb;
  if (!web) {
    setStatus("err", "Payment method update is unavailable.");
    return;
  }
  const checkout = await web.AdyenCheckout({
    clientKey: cfg.clientKey,
    environment: cfg.environment,
    session: { id: cfg.sessionId, sessionData: cfg.sessionData },
    onPaymentCompleted: (result: { resultCode?: string }) => {
      if (result && result.resultCode === "Authorised") {
        setStatus("ok", "Payment method updated.");
        return;
      }
      setStatus("err", "Payment method update did not complete.");
    },
    onPaymentFailed: () => {
      setStatus("err", "Payment method update failed.");
    },
    onError: () => {
      setStatus("err", "Payment method update failed.");
    },
  });
  new web.Dropin(checkout, {
    paymentMethodsConfiguration: {
      card: { styles: CARD_STYLES, hasHolderName: false },
    },
  }).mount("#dropin");
}

void mountDropin();
