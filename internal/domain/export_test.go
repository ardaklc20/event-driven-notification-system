package domain

import (
	"strings"
	"unicode/utf8"
)

// RenderBuffered renders like Render and also returns how many code points
// the output buffer held when rendering ended, for the test of K47's
// rendering limit.
func (p *ParsedTemplate) RenderBuffered(vars map[string]string, limit int) (string, int, []FieldError) {
	var b strings.Builder
	out, errs := p.render(vars, limit, &b)
	return out, utf8.RuneCountInString(b.String()), errs
}
