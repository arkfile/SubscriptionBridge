package hmac

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/arkfile/SubscriptionBridge/internal/protocol"
)

// CallbackHeader builds the canonical t=,v1= callback signature header.
func CallbackHeader(key []byte, ts int64, body []byte) (string, error) {
	unix, err := protocol.CanonicalUnix(ts)
	if err != nil {
		return "", err
	}
	base := unix + "." + string(body)
	sig := Sign(key, []byte(base))
	return "t=" + unix + ",v1=" + sig, nil
}

// VerifyCallbackHeader checks replay window and HMAC over the stored callback bytes.
func VerifyCallbackHeader(key []byte, header string, body []byte, now time.Time) error {
	ts, sig, err := parseComponents(header)
	if err != nil {
		return err
	}
	if err := protocol.ValidateReplayWindow(ts, now); err != nil {
		return err
	}
	unix, err := protocol.CanonicalUnix(ts)
	if err != nil {
		return err
	}
	base := unix + "." + string(body)
	return Verify(key, []byte(base), sig)
}

// ReconcileHeader builds the SubscriptionBridge-HMAC1 Authorization value.
func ReconcileHeader(key []byte, ts int64, path string) (string, error) {
	unix, err := protocol.CanonicalUnix(ts)
	if err != nil {
		return "", err
	}
	base := "GET\n" + path + "\n" + unix
	sig := Sign(key, []byte(base))
	return protocol.ReconcileScheme + " t=" + unix + ",v1=" + sig, nil
}

// VerifyReconcileHeader checks the reconciliation Authorization grammar and HMAC.
func VerifyReconcileHeader(key []byte, authorization, path string, now time.Time) error {
	scheme, rest, ok := strings.Cut(authorization, " ")
	if !ok || scheme != protocol.ReconcileScheme || rest == "" {
		return protocol.ErrInvalidSignature
	}
	if strings.TrimSpace(rest) != rest || strings.ContainsAny(rest, " \t") {
		return protocol.ErrInvalidSignature
	}
	ts, sig, err := parseComponents(rest)
	if err != nil {
		return err
	}
	if err := protocol.ValidateReplayWindow(ts, now); err != nil {
		return err
	}
	unix, err := protocol.CanonicalUnix(ts)
	if err != nil {
		return err
	}
	base := "GET\n" + path + "\n" + unix
	return Verify(key, []byte(base), sig)
}

// parseComponents parses canonical t=,v1= header components with no extras.
func parseComponents(header string) (int64, string, error) {
	if header == "" || strings.ContainsAny(header, " \t\r\n") {
		return 0, "", protocol.ErrInvalidSignature
	}
	parts := strings.Split(header, ",")
	if len(parts) != 2 {
		return 0, "", protocol.ErrInvalidSignature
	}
	tPart, vPart := parts[0], parts[1]
	if !strings.HasPrefix(tPart, "t=") || !strings.HasPrefix(vPart, "v1=") {
		return 0, "", protocol.ErrInvalidSignature
	}
	if strings.Count(header, "t=") != 1 || strings.Count(header, "v1=") != 1 {
		return 0, "", protocol.ErrInvalidSignature
	}
	tRaw := strings.TrimPrefix(tPart, "t=")
	vRaw := strings.TrimPrefix(vPart, "v1=")
	if tRaw == "" || vRaw == "" {
		return 0, "", protocol.ErrInvalidSignature
	}
	if len(tRaw) > 1 && tRaw[0] == '0' {
		return 0, "", protocol.ErrInvalidSignature
	}
	if strings.HasPrefix(tRaw, "-") || strings.ContainsAny(tRaw, "+.eE") {
		return 0, "", protocol.ErrInvalidSignature
	}
	ts, err := strconv.ParseInt(tRaw, 10, 64)
	if err != nil || ts < 0 {
		return 0, "", protocol.ErrInvalidSignature
	}
	if strconv.FormatInt(ts, 10) != tRaw {
		return 0, "", protocol.ErrInvalidSignature
	}
	if len(vRaw) != protocol.SignatureHexLen || strings.ToLower(vRaw) != vRaw {
		return 0, "", protocol.ErrInvalidSignature
	}
	return ts, vRaw, nil
}

// SignCallback signs a callback body using the current Unix timestamp.
func SignCallback(key []byte, now time.Time, body []byte) (string, error) {
	return CallbackHeader(key, now.UTC().Unix(), body)
}

// HeaderError wraps an invalid-signature failure with a component name.
func HeaderError(component string) error {
	return fmt.Errorf("%w: %s", protocol.ErrInvalidSignature, component)
}
