package protocol

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// TruncateUTC converts t to UTC and drops sub-second precision.
func TruncateUTC(t time.Time) time.Time {
	return t.UTC().Truncate(time.Second)
}

// FormatUTC renders a second-precision RFC3339 timestamp with Z.
func FormatUTC(t time.Time) string {
	return TruncateUTC(t).Format(time.RFC3339)
}

// ParseUTC accepts only canonical RFC3339 UTC second-precision timestamps.
func ParseUTC(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, ErrInvalidTimestamp
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %v", ErrInvalidTimestamp, err)
	}
	if !strings.HasSuffix(value, "Z") {
		return time.Time{}, fmt.Errorf("%w: timestamps must use Z", ErrInvalidTimestamp)
	}
	if parsed.Nanosecond() != 0 {
		return time.Time{}, fmt.Errorf("%w: second precision required", ErrInvalidTimestamp)
	}
	if value != parsed.UTC().Format(time.RFC3339) {
		return time.Time{}, fmt.Errorf("%w: non-canonical rfc3339", ErrInvalidTimestamp)
	}
	return parsed.UTC(), nil
}

// ParseUnixSeconds parses a non-negative Unix timestamp with no leading zeros.
func ParseUnixSeconds(raw string) (int64, error) {
	if raw == "" {
		return 0, ErrInvalidTimestamp
	}
	if strings.ContainsAny(raw, ".eE+") {
		return 0, ErrInvalidTimestamp
	}
	if len(raw) > 1 && raw[0] == '0' {
		return 0, ErrInvalidTimestamp
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, ErrInvalidTimestamp
	}
	if n < 0 {
		return 0, ErrInvalidTimestamp
	}
	return n, nil
}

// CanonicalUnix formats a Unix timestamp without leading zeros.
func CanonicalUnix(n int64) (string, error) {
	if n < 0 {
		return "", ErrInvalidTimestamp
	}
	return strconv.FormatInt(n, 10), nil
}

// ValidateTokenLifetime enforces exp>iat, max TTL, and clock-skew bounds.
func ValidateTokenLifetime(iat, exp int64, now time.Time) error {
	if iat < 0 || exp < 0 {
		return ErrInvalidTimestamp
	}
	if exp <= iat {
		return ErrInvalidTimestamp
	}
	if time.Duration(exp-iat)*time.Second > MaxTokenTTL {
		return ErrInvalidTimestamp
	}
	nowUnix := now.UTC().Unix()
	skew := int64(ClockSkew / time.Second)
	if iat > nowUnix+skew {
		return ErrInvalidTimestamp
	}
	if nowUnix > exp+skew {
		return ErrExpiredToken
	}
	return nil
}

// ValidateReplayWindow rejects HMAC timestamps outside the replay window.
func ValidateReplayWindow(ts int64, now time.Time) error {
	if ts < 0 {
		return ErrInvalidTimestamp
	}
	delta := now.UTC().Unix() - ts
	if delta < 0 {
		delta = -delta
	}
	if delta > int64(ReplayWindow/time.Second) {
		return ErrReplayWindow
	}
	return nil
}

// AddCalendarMonths advances a UTC anniversary, clamping to the last day of shorter months.
func AddCalendarMonths(start time.Time, months int) time.Time {
	start = TruncateUTC(start)
	year, month, day := start.Date()
	month = time.Month(int(month) + months)
	for month > 12 {
		year++
		month -= 12
	}
	for month < 1 {
		year--
		month += 12
	}
	last := daysIn(year, month)
	if day > last {
		day = last
	}
	return time.Date(year, month, day, start.Hour(), start.Minute(), start.Second(), 0, time.UTC)
}

// daysIn returns the number of days in a UTC month.
func daysIn(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}
