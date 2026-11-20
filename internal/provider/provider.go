// Package provider is the HTTP client for the notification provider (K14,
// K15). Every channel goes to the one configured URL; the channel travels in
// the body (A2).
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
)

// Profile decides how the provider's 429 is handled (K7, K14). Slice 4 uses it.
type Profile string

// The two profiles.
const (
	ProfileStandard        Profile = "standard"
	ProfileWebhookSiteFree Profile = "webhook_site_free"
)

// Valid reports whether p is a known profile.
func (p Profile) Valid() bool {
	return p == ProfileStandard || p == ProfileWebhookSiteFree
}

// CorrelationIDHeader carries the notification's correlation ID to the
// provider (K21).
const CorrelationIDHeader = "X-Correlation-ID"

// maxBody bounds how much of a response body is read.
const maxBody = 64 << 10

// Request is one delivery.
type Request struct {
	To            string
	Channel       string
	Content       string
	CorrelationID string
}

// requestBody is the case's request shape: POST {to, channel, content} (R13).
type requestBody struct {
	To      string `json:"to"`
	Channel string `json:"channel"`
	Content string `json:"content"`
}

// Response is what the provider answered.
type Response struct {
	StatusCode int
	// Acceptance is the decoded 202 body; empty for any other status.
	Acceptance domain.Acceptance
	// BodyProblem says why a 202 body could not be used in full, or "". The
	// notification is still sent: the provider accepted it (K15).
	BodyProblem string
}

// Client sends deliveries to one provider URL.
type Client struct {
	url     string
	timeout time.Duration
	http    *http.Client
}

// NewClient returns a client for providerURL. Each call is bounded by
// timeout; maxIdleConns keeps that many connections open for reuse, normally
// the number of calls in flight.
func NewClient(providerURL string, timeout time.Duration, maxIdleConns int) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = maxIdleConns
	transport.MaxIdleConnsPerHost = maxIdleConns
	return &Client{
		url:     providerURL,
		timeout: timeout,
		http: &http.Client{
			Transport: transport,
			// Redirects are not followed: the first response comes back as it
			// is, so a POST never reaches a second address (K14).
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// Send posts req. The call ends at the earlier of ctx's deadline and the
// client's timeout. An error means no status line arrived. Once a 202 status
// line has arrived, Send never returns an error: a body that is broken, or
// that stops arriving before the deadline, only sets BodyProblem.
func (c *Client) Send(ctx context.Context, req Request) (Response, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	body, err := json.Marshal(requestBody{To: req.To, Channel: req.Channel, Content: req.Content})
	if err != nil {
		return Response{}, fmt.Errorf("encode request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set(CorrelationIDHeader, req.CorrelationID)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		// The url.Error repeats the URL, and a webhook.site URL holds its
		// token; keep only the cause.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return Response{}, fmt.Errorf("provider call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusAccepted {
		// Read a little so the connection can be reused; the body is not used.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return Response{StatusCode: resp.StatusCode}, nil
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	switch {
	case err != nil:
		return Response{StatusCode: resp.StatusCode, BodyProblem: "body could not be read: " + err.Error()}, nil
	case len(raw) > maxBody:
		return Response{StatusCode: resp.StatusCode, BodyProblem: fmt.Sprintf("body is over %d bytes", maxBody)}, nil
	}
	acc, problem := ParseAcceptance(raw)
	return Response{StatusCode: resp.StatusCode, Acceptance: acc, BodyProblem: problem}, nil
}

// ParseAcceptance decodes a 202 body {messageId, status, timestamp}. Each
// field that is missing or invalid stays empty and is named in problem (K15).
// status is not stored, so it is not checked.
func ParseAcceptance(body []byte) (acc domain.Acceptance, problem string) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return domain.Acceptance{}, "body is not a JSON object"
	}
	var problems []string

	var id string
	if raw, ok := fields["messageId"]; !ok {
		problems = append(problems, "messageId is missing")
	} else if err := json.Unmarshal(raw, &id); err != nil || id == "" {
		problems = append(problems, "messageId is not a non-empty string")
	} else {
		acc.MessageID = id
	}

	var ts string
	if raw, ok := fields["timestamp"]; !ok {
		problems = append(problems, "timestamp is missing")
	} else if err := json.Unmarshal(raw, &ts); err != nil {
		problems = append(problems, "timestamp is not a string")
	} else if parsed, err := time.Parse(time.RFC3339Nano, ts); err != nil {
		problems = append(problems, "timestamp is not RFC 3339")
	} else {
		acc.Timestamp = parsed
	}

	return acc, strings.Join(problems, "; ")
}
