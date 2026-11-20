package domain_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
)

// TestParseTemplate: every row of the slice note's syntax table (K33).
func TestParseTemplate(t *testing.T) {
	for _, tc := range []struct {
		body  string
		names []string // nil: a syntax error
	}{
		{"Hello {{first_name}}", []string{"first_name"}},
		{"{{a}}{{a}} {{_b2}}", []string{"a", "_b2"}},
		{"Code { 42 }", []string{}},
		{"Hello {{first_name", nil},
		{"Hello {{}}", nil},
		{"Hello {{1st}}", nil},
		{"Hello {{ first_name }}", nil},
	} {
		t.Run(tc.body, func(t *testing.T) {
			p, err := domain.ParseTemplate(tc.body)
			if tc.names == nil {
				if err == nil {
					t.Fatalf("parsed with placeholders %v, want a syntax error", p.Names())
				}
				errs := domain.ValidateTemplate(domain.NewTemplate{Name: "n", Channel: domain.ChannelSMS, Body: tc.body})
				if len(errs) != 1 || errs[0].Field != "body" || errs[0].Code != domain.CodeFormat {
					t.Fatalf("template rules: %+v, want one format error on body", errs)
				}
				return
			}
			if err != nil {
				t.Fatalf("syntax error %v, want placeholders %v", err, tc.names)
			}
			if got := p.Names(); !slices.Equal(got, tc.names) {
				t.Fatalf("placeholders %v, want %v", got, tc.names)
			}
		})
	}
}

// TestRenderTemplate: values replaced in one pass, the K11 content rules on
// the result, and the limit enforced while rendering (K33, K47).
func TestRenderTemplate(t *testing.T) {
	const smsLimit = 160
	type want struct {
		content string
		field   string
		code    domain.ErrorCode
	}
	for name, tc := range map[string]struct {
		body string
		vars map[string]string
		want want
	}{
		"plain values": {
			body: "Hello {{first_name}}, your code is {{code}}",
			vars: map[string]string{"first_name": "Deniz", "code": "4821"},
			want: want{content: "Hello Deniz, your code is 4821"},
		},
		"a placeholder inside a value is kept": {
			body: "Hello {{first_name}}, your code is {{code}}",
			vars: map[string]string{"first_name": "Deniz", "code": "{{code}}"},
			want: want{content: "Hello Deniz, your code is {{code}}"},
		},
		"missing variable": {
			body: "Hello {{first_name}} {{last_name}}",
			vars: map[string]string{"first_name": "Deniz"},
			want: want{field: "variables", code: domain.CodeRequired},
		},
		"extra variable": {
			body: "Hello {{first_name}}",
			vars: map[string]string{"first_name": "Deniz", "nickname": "D"},
			want: want{field: "variables", code: domain.CodeUnknownValue},
		},
		"renders empty": {
			body: "{{a}}{{b}}",
			vars: map[string]string{"a": "", "b": ""},
			want: want{field: "content", code: domain.CodeRequired},
		},
		"one code point over the sms limit": {
			body: "Code: {{code}}",
			vars: map[string]string{"code": strings.Repeat("ş", smsLimit-len("Code: ")+1)},
			want: want{field: "content", code: domain.CodeTooLong},
		},
		"exactly the sms limit": {
			body: "Code: {{code}}",
			vars: map[string]string{"code": strings.Repeat("ş", smsLimit-len("Code: "))},
			want: want{content: "Code: " + strings.Repeat("ş", smsLimit-len("Code: "))},
		},
		"a value with NUL": {
			body: "Hello {{first_name}}",
			vars: map[string]string{"first_name": "a\x00b"},
			want: want{field: "content", code: domain.CodeNUL},
		},
	} {
		t.Run(name, func(t *testing.T) {
			p, err := domain.ParseTemplate(tc.body)
			if err != nil {
				t.Fatal(err)
			}
			got, errs := p.Render(tc.vars, smsLimit)
			if tc.want.field == "" {
				if len(errs) > 0 || got != tc.want.content {
					t.Fatalf("got %q %+v, want %q", got, errs, tc.want.content)
				}
				return
			}
			if len(errs) != 1 || errs[0].Field != tc.want.field || errs[0].Code != tc.want.code || got != "" {
				t.Fatalf("got %q %+v, want one %s error on %s", got, errs, tc.want.code, tc.want.field)
			}
		})
	}

	t.Run("a placeholder repeated 1,000 times stops at the limit", func(t *testing.T) {
		const valueLen = 100
		p, err := domain.ParseTemplate(strings.Repeat("{{v}}", 1000))
		if err != nil {
			t.Fatal(err)
		}
		_, held, errs := p.RenderBuffered(map[string]string{"v": strings.Repeat("x", valueLen)}, smsLimit)
		if len(errs) != 1 || errs[0].Code != domain.CodeTooLong {
			t.Fatalf("errors %+v, want the over-limit error", errs)
		}
		if held > smsLimit+valueLen {
			t.Fatalf("the output buffer held %d code points, want at most %d (the limit plus one value)", held, smsLimit+valueLen)
		}
		t.Logf("the output buffer held %d code points of a possible 100,000", held)
	})

	t.Run("prepare: unknown template and channel mismatch", func(t *testing.T) {
		limits := domain.DefaultLimits()
		tpl := domain.Template{ID: "t1", Channel: domain.ChannelEmail, Body: "Hi {{n}}"}
		n := domain.NewNotification{Channel: domain.ChannelSMS, TemplateID: "t2", Variables: map[string]string{"n": "x"}}
		if _, errs := limits.Prepare(n, domain.Templates{"t1": tpl}); len(errs) != 1 || errs[0].Field != "template_id" {
			t.Fatalf("unknown template: %+v, want an error on template_id", errs)
		}
		n.TemplateID = "t1"
		if _, errs := limits.Prepare(n, domain.Templates{"t1": tpl}); len(errs) != 1 || errs[0].Field != "channel" {
			t.Fatalf("channel mismatch: %+v, want an error on channel", errs)
		}
	})
}
