package domain

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Template limits (K47), in Unicode code points as in K11.
const (
	MaxTemplateNameLength = 100
	MaxTemplateBodyLength = 10000
)

// ErrTemplateNotFound means no template has the requested ID.
var ErrTemplateNotFound = errors.New("template not found")

// NewTemplate is a template as a client sends it (K33).
type NewTemplate struct {
	Name    string
	Channel Channel
	Body    string
}

// Template is a stored template. Templates never change: a changed message
// is a new template (K33).
type Template struct {
	ID        string
	Name      string
	Channel   Channel
	Body      string
	CreatedAt time.Time
}

// Templates holds the stored templates a request names, by ID.
type Templates map[string]Template

// placeholderName is the name inside {{ }} (K33).
var placeholderName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// templatePart is literal text or one placeholder.
type templatePart struct {
	text string
	name string // set for a placeholder
}

// ParsedTemplate is a template body split into literal text and placeholders.
type ParsedTemplate struct {
	parts []templatePart
	names []string // each placeholder once, in order of first use
}

// TemplateSyntaxError says why a template body does not parse.
type TemplateSyntaxError struct {
	Message string
}

func (e *TemplateSyntaxError) Error() string { return e.Message }

// ParseTemplate splits body into literal text and {{name}} placeholders.
// Single braces are plain text; "{{" opens a placeholder that must be closed
// by "}}" around a name matching [A-Za-z_][A-Za-z0-9_]*, without spaces.
func ParseTemplate(body string) (*ParsedTemplate, error) {
	p := &ParsedTemplate{}
	seen := map[string]bool{}
	rest := body
	for {
		open := strings.Index(rest, "{{")
		if open < 0 {
			if rest != "" {
				p.parts = append(p.parts, templatePart{text: rest})
			}
			return p, nil
		}
		if open > 0 {
			p.parts = append(p.parts, templatePart{text: rest[:open]})
		}
		after := rest[open+2:]
		end := strings.Index(after, "}}")
		if end < 0 {
			return nil, &TemplateSyntaxError{Message: fmt.Sprintf("placeholder at byte %d is not closed with }}", len(body)-len(rest)+open)}
		}
		name := after[:end]
		switch {
		case name == "":
			return nil, &TemplateSyntaxError{Message: "a placeholder has an empty name: {{}}"}
		case !placeholderName.MatchString(name):
			return nil, &TemplateSyntaxError{Message: fmt.Sprintf("placeholder name %q must match [A-Za-z_][A-Za-z0-9_]* with no spaces", name)}
		}
		p.parts = append(p.parts, templatePart{name: name})
		if !seen[name] {
			seen[name] = true
			p.names = append(p.names, name)
		}
		rest = after[end+2:]
	}
}

// Names returns each placeholder once, in order of first use.
func (p *ParsedTemplate) Names() []string { return append([]string(nil), p.names...) }

// ValidateTemplate checks a new template against the K47 rules. Code points
// are counted as in K11.
func ValidateTemplate(t NewTemplate) []FieldError {
	var errs []FieldError
	add := func(field string, code ErrorCode, msg string) {
		errs = append(errs, FieldError{Field: field, Code: code, Message: msg})
	}
	switch n := utf8.RuneCountInString(t.Name); {
	case t.Name == "":
		add("name", CodeRequired, "is required")
	case strings.ContainsRune(t.Name, 0):
		add("name", CodeNUL, "must not contain the NUL character (U+0000)")
	case n > MaxTemplateNameLength:
		add("name", CodeTooLong, fmt.Sprintf("must be at most %d characters, got %d", MaxTemplateNameLength, n))
	}
	switch t.Channel {
	case "":
		add("channel", CodeRequired, "is required")
	case ChannelSMS, ChannelEmail, ChannelPush:
	default:
		add("channel", CodeUnknownValue, "must be sms, email or push")
	}
	switch n := utf8.RuneCountInString(t.Body); {
	case t.Body == "":
		add("body", CodeRequired, "is required")
	case strings.ContainsRune(t.Body, 0):
		add("body", CodeNUL, "must not contain the NUL character (U+0000)")
	case n > MaxTemplateBodyLength:
		add("body", CodeTooLong, fmt.Sprintf("must be at most %d characters, got %d", MaxTemplateBodyLength, n))
	default:
		if _, err := ParseTemplate(t.Body); err != nil {
			add("body", CodeFormat, err.Error())
		}
	}
	return errs
}

// Render replaces every placeholder with its value in one pass and checks
// the result with the K11 content rules (K33, K47):
//
//   - vars must name exactly the template's placeholders: a missing or an
//     extra variable is an error on "variables";
//   - values are inserted as they are; a {{x}} inside a value is not rendered
//     again;
//   - the output is counted in code points as it is written, and rendering
//     stops as soon as the next part would pass limit, so a placeholder
//     repeated many times cannot build a large string;
//   - empty output and output with NUL are errors on "content".
//
// limit <= 0 means no limit.
func (p *ParsedTemplate) Render(vars map[string]string, limit int) (string, []FieldError) {
	var b strings.Builder
	out, errs := p.render(vars, limit, &b)
	return out, errs
}

func (p *ParsedTemplate) render(vars map[string]string, limit int, b *strings.Builder) (string, []FieldError) {
	var errs []FieldError
	for _, name := range p.names {
		if _, ok := vars[name]; !ok {
			errs = append(errs, FieldError{Field: "variables", Code: CodeRequired, Message: fmt.Sprintf("needs a value for {{%s}}", name)})
		}
	}
	placeholders := make(map[string]bool, len(p.names))
	for _, name := range p.names {
		placeholders[name] = true
	}
	var extra []string
	for name := range vars {
		if !placeholders[name] {
			extra = append(extra, name)
		}
	}
	if len(extra) > 0 {
		// Sorted, so the same request always gets the same answer.
		slices.Sort(extra)
		for _, name := range extra {
			errs = append(errs, FieldError{Field: "variables", Code: CodeUnknownValue, Message: fmt.Sprintf("%q is not a placeholder of the template", name)})
		}
	}
	if len(errs) > 0 {
		return "", errs
	}

	count := 0
	for _, part := range p.parts {
		s := part.text
		if part.name != "" {
			s = vars[part.name]
		}
		n := utf8.RuneCountInString(s)
		if limit > 0 && count+n > limit {
			return "", []FieldError{{
				Field: "content", Code: CodeTooLong,
				Message: fmt.Sprintf("the rendered content must be at most %d characters; it passed the limit while rendering", limit),
			}}
		}
		b.WriteString(s)
		count += n
	}
	out := b.String()
	switch {
	case out == "":
		return "", []FieldError{{Field: "content", Code: CodeRequired, Message: "the rendered content is empty"}}
	case strings.ContainsRune(out, 0):
		return "", []FieldError{{Field: "content", Code: CodeNUL, Message: "the rendered content must not contain the NUL character (U+0000)"}}
	}
	return out, nil
}

// For returns the content limit of channel.
func (l Limits) For(channel Channel) int {
	switch channel {
	case ChannelSMS:
		return l.SMS
	case ChannelEmail:
		return l.Email
	case ChannelPush:
		return l.Push
	}
	return 0
}

// Prepare returns n ready to save: a template request rendered from its
// stored template, a plain one checked against the content limit. Errors name
// the request's fields (K33).
func (l Limits) Prepare(n NewNotification, templates Templates) (NewNotification, []FieldError) {
	if n.TemplateID == "" {
		return n, l.Check(n)
	}
	t, ok := templates[n.TemplateID]
	if !ok {
		return n, []FieldError{{Field: "template_id", Code: CodeUnknownValue, Message: "no template has this ID"}}
	}
	if t.Channel != n.Channel {
		return n, []FieldError{{Field: "channel", Code: CodeUnknownValue,
			Message: fmt.Sprintf("must equal the template's channel, %s", t.Channel)}}
	}
	parsed, err := ParseTemplate(t.Body)
	if err != nil {
		// A stored template was checked when it was created.
		return n, []FieldError{{Field: "template_id", Code: CodeFormat, Message: "the stored template does not parse"}}
	}
	content, errs := parsed.Render(n.Variables, l.For(n.Channel))
	if len(errs) > 0 {
		return n, errs
	}
	n.Content = content
	return n, nil
}
