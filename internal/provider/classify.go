package provider

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Class is what an attempt's outcome means for the notification (K7, K41).
type Class string

// The four classes.
const (
	// ClassAccepted: 202; the notification is sent.
	ClassAccepted Class = "accepted"
	// ClassTemporary: retried while attempts are left.
	ClassTemporary Class = "temporary"
	// ClassPermanent: an error waiting cannot fix; failed on this attempt.
	ClassPermanent Class = "permanent"
	// ClassTerminal: a response K7 does not list, which may mean the provider
	// processed the request; failed on this attempt, never retried (K41).
	ClassTerminal Class = "terminal"
)

// Outcome is a classified attempt.
type Outcome struct {
	Class Class
	// Error is the last_error text, "<class>: <detail>"; empty when accepted.
	Error string
	// Outage marks a failure that counts towards the circuit breaker (K34):
	// a network error, a timeout, 408 and the 5xx codes K7 retries. A 429
	// and permanent errors concern the request, not the provider.
	Outage bool
	// SharedQuota marks a 429 under a profile that states that its quota
	// covers the whole URL; with a valid Retry-After it opens the breaker.
	SharedQuota bool
}

// Classify sorts an attempt's result into its class (K7, K41). err is the
// error Send returned; resp is ignored when err is set.
func Classify(resp Response, err error, profile Profile) Outcome {
	if err != nil {
		var transport *TransportError
		if errors.As(err, &transport) {
			return Outcome{Class: ClassTemporary, Error: "temporary: " + err.Error(), Outage: true}
		}
		// The request could not even be built; another attempt would fail the
		// same way.
		return Outcome{Class: ClassPermanent, Error: "permanent: " + err.Error()}
	}

	code := resp.StatusCode
	switch code {
	case http.StatusAccepted:
		return Outcome{Class: ClassAccepted}
	case http.StatusRequestTimeout, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return Outcome{Class: ClassTemporary, Error: fmt.Sprintf("temporary: status %d", code), Outage: true}
	case http.StatusTooManyRequests:
		// A free webhook.site URL answers 429 only when its request quota is
		// used up, and waiting does not reopen it (K7).
		if profile == ProfileWebhookSiteFree {
			return Outcome{Class: ClassPermanent, Error: "permanent: status 429 (webhook.site quota used up)"}
		}
		return Outcome{Class: ClassTemporary, Error: "temporary: status 429", SharedQuota: profile.SharedQuota()}
	case http.StatusBadRequest, http.StatusNotImplemented:
		return Outcome{Class: ClassPermanent, Error: fmt.Sprintf("permanent: status %d", code)}
	default:
		return Outcome{Class: ClassTerminal, Error: fmt.Sprintf("terminal: unexpected status %d", code)}
	}
}

// ParseRetryAfter reads a Retry-After value: a number of seconds or an HTTP
// date (RFC 9110). A date in the past gives 0. ok is false for an empty or
// invalid value, which is then ignored.
func ParseRetryAfter(value string, now time.Time) (wait time.Duration, ok bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if isDigits(value) {
		secs, err := strconv.ParseInt(value, 10, 64)
		if err != nil || secs > int64(maxRetryAfter/time.Second) {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	at, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	if d := at.Sub(now); d > 0 {
		return d, true
	}
	return 0, true
}

// maxRetryAfter keeps a huge number of seconds from overflowing a Duration.
const maxRetryAfter = 100 * 365 * 24 * time.Hour

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
