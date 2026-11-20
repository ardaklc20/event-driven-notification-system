package domain_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
)

func valid(channel domain.Channel) domain.NewNotification {
	recipients := map[domain.Channel]string{
		domain.ChannelSMS:   "+905551234567",
		domain.ChannelEmail: "user@example.com",
		domain.ChannelPush:  "device-token-123",
	}
	return domain.NewNotification{Recipient: recipients[channel], Channel: channel, Content: "hello", Priority: domain.PriorityHigh}
}

func TestValidate(t *testing.T) {
	t.Run("structure", func(t *testing.T) {
		tests := []struct {
			name  string
			edit  func(*domain.NewNotification)
			field string
			code  domain.ErrorCode
		}{
			{"valid sms", func(*domain.NewNotification) {}, "", ""},
			{"valid email", func(n *domain.NewNotification) { *n = valid(domain.ChannelEmail) }, "", ""},
			{"valid push", func(n *domain.NewNotification) { *n = valid(domain.ChannelPush) }, "", ""},
			{"E.164 without plus", func(n *domain.NewNotification) { n.Recipient = "905551234567" }, "recipient", domain.CodeFormat},
			{"E.164 with 16 digits", func(n *domain.NewNotification) { n.Recipient = "+9055512345678901" }, "recipient", domain.CodeFormat},
			{"E.164 starting with 0", func(n *domain.NewNotification) { n.Recipient = "+05551234567" }, "recipient", domain.CodeFormat},
			{"E.164 with letters", func(n *domain.NewNotification) { n.Recipient = "+90555abc4567" }, "recipient", domain.CodeFormat},
			{"email with display name", func(n *domain.NewNotification) {
				*n = valid(domain.ChannelEmail)
				n.Recipient = "User <user@example.com>"
			}, "recipient", domain.CodeFormat},
			{"email without domain", func(n *domain.NewNotification) {
				*n = valid(domain.ChannelEmail)
				n.Recipient = "user"
			}, "recipient", domain.CodeFormat},
			{"push with whitespace", func(n *domain.NewNotification) {
				*n = valid(domain.ChannelPush)
				n.Recipient = "token with space"
			}, "recipient", domain.CodeFormat},
			{"push over 512 characters", func(n *domain.NewNotification) {
				*n = valid(domain.ChannelPush)
				n.Recipient = strings.Repeat("ğ", 513)
			}, "recipient", domain.CodeFormat},
			{"empty recipient", func(n *domain.NewNotification) { n.Recipient = "" }, "recipient", domain.CodeRequired},
			{"empty content", func(n *domain.NewNotification) { n.Content = "" }, "content", domain.CodeRequired},
			{"missing channel", func(n *domain.NewNotification) { n.Channel = "" }, "channel", domain.CodeRequired},
			{"unknown channel", func(n *domain.NewNotification) { n.Channel = "fax" }, "channel", domain.CodeUnknownValue},
			{"unknown priority", func(n *domain.NewNotification) { n.Priority = "urgent" }, "priority", domain.CodeUnknownValue},
			{"NUL in content", func(n *domain.NewNotification) { n.Content = "a\x00b" }, "content", domain.CodeNUL},
			{"NUL in recipient", func(n *domain.NewNotification) {
				*n = valid(domain.ChannelPush)
				n.Recipient = "tok\x00en"
			}, "recipient", domain.CodeNUL},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				n := valid(domain.ChannelSMS)
				tt.edit(&n)
				_, errs := domain.Normalize(n)
				if tt.field == "" {
					if len(errs) != 0 {
						t.Fatalf("want no errors, got %+v", errs)
					}
					return
				}
				if len(errs) != 1 || errs[0].Field != tt.field || errs[0].Code != tt.code {
					t.Fatalf("want one %s error on %s, got %+v", tt.code, tt.field, errs)
				}
			})
		}
	})

	t.Run("several errors at once", func(t *testing.T) {
		_, errs := domain.Normalize(domain.NewNotification{Channel: "fax", Priority: "urgent"})
		if len(errs) != 4 { // channel, priority, recipient, content
			t.Fatalf("want 4 errors, got %+v", errs)
		}
	})

	t.Run("missing priority becomes normal", func(t *testing.T) {
		n := valid(domain.ChannelSMS)
		n.Priority = ""
		got, errs := domain.Normalize(n)
		if len(errs) != 0 || got.Priority != domain.PriorityNormal {
			t.Fatalf("priority = %q, errors %+v; want normal and none", got.Priority, errs)
		}
	})

	t.Run("limits", func(t *testing.T) {
		limits := domain.DefaultLimits()
		sms := valid(domain.ChannelSMS)

		sms.Content = strings.Repeat("a", 160)
		if errs := limits.Check(sms); len(errs) != 0 {
			t.Errorf("sms 160: %+v", errs)
		}
		sms.Content = strings.Repeat("a", 161)
		if errs := limits.Check(sms); len(errs) != 1 || errs[0].Code != domain.CodeTooLong {
			t.Errorf("sms 161: want too_long, got %+v", errs)
		}

		// 160 Turkish characters are 320 bytes but 160 code points.
		sms.Content = strings.Repeat("ş", 160)
		if errs := limits.Check(sms); len(errs) != 0 {
			t.Errorf("sms 160 Turkish characters: %+v", errs)
		}
		sms.Content = strings.Repeat("ş", 161)
		if errs := limits.Check(sms); len(errs) != 1 {
			t.Errorf("sms 161 Turkish characters: want one error, got %+v", errs)
		}

		push := valid(domain.ChannelPush)
		push.Content = strings.Repeat("a", 1001)
		email := valid(domain.ChannelEmail)
		email.Content = strings.Repeat("a", 10001)
		if len(limits.Check(push)) != 1 || len(limits.Check(email)) != 1 {
			t.Error("push 1001 or email 10001 accepted")
		}

		lowered := limits
		lowered.SMS = 100
		sms.Content = strings.Repeat("a", 120)
		if errs := lowered.Check(sms); len(errs) != 1 {
			t.Errorf("lowered limit 100 with 120 characters: want one error, got %+v", errs)
		}
	})
}

func TestRequestHash(t *testing.T) {
	withoutPriority := valid(domain.ChannelSMS)
	withoutPriority.Priority = ""
	withoutPriority, _ = domain.Normalize(withoutPriority)
	withNormal := valid(domain.ChannelSMS)
	withNormal.Priority = domain.PriorityNormal

	a := domain.RequestHash(domain.OperationCreate, []domain.NewNotification{withoutPriority})
	b := domain.RequestHash(domain.OperationCreate, []domain.NewNotification{withNormal})
	if !bytes.Equal(a, b) {
		t.Error("missing priority and explicit normal hash differently")
	}

	batch := domain.RequestHash(domain.OperationBatch, []domain.NewNotification{withNormal})
	if bytes.Equal(a, batch) {
		t.Error("the same body hashes the same under create and batch")
	}

	other := withNormal
	other.Content = "different"
	if bytes.Equal(b, domain.RequestHash(domain.OperationCreate, []domain.NewNotification{other})) {
		t.Error("different content hashes the same")
	}
	if len(a) != 32 {
		t.Errorf("hash length %d, want 32", len(a))
	}
}
