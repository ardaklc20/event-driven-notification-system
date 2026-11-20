package service

import (
	"context"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
)

// TemplateStore is what the template rules need from the database (K33).
type TemplateStore interface {
	CreateTemplate(ctx context.Context, t domain.NewTemplate) (domain.Template, error)
	GetTemplate(ctx context.Context, id string) (domain.Template, error)
	// ListTemplates returns up to f.Limit templates newest first, after
	// f.After when it is set. Only After and Limit are read.
	ListTemplates(ctx context.Context, f domain.ListFilter) ([]domain.Template, error)
}

// Templates creates, reads and lists templates. Templates never change, so
// there is no update or delete (K33).
type Templates struct {
	store TemplateStore
}

// NewTemplates returns the service.
func NewTemplates(store TemplateStore) *Templates { return &Templates{store: store} }

// Create checks the K47 rules and saves the template.
func (s *Templates) Create(ctx context.Context, t domain.NewTemplate) (domain.Template, error) {
	if errs := domain.ValidateTemplate(t); len(errs) > 0 {
		return domain.Template{}, &domain.ValidationError{Errors: errs}
	}
	return s.store.CreateTemplate(ctx, t)
}

// Get reads one template; domain.ErrTemplateNotFound when it does not exist.
func (s *Templates) Get(ctx context.Context, id string) (domain.Template, error) {
	return s.store.GetTemplate(ctx, id)
}

// TemplatePage is one page of templates.
type TemplatePage struct {
	Items []domain.Template
	// NextCursor reads the next page; "" on the last page.
	NextCursor string
}

// List returns one page, newest first, with the K13 cursor and the K44 limit.
func (s *Templates) List(ctx context.Context, cursor string, limit int) (TemplatePage, error) {
	f, err := listFilter(cursor, limit)
	if err != nil {
		return TemplatePage{}, err
	}
	items, err := s.store.ListTemplates(ctx, f)
	if err != nil {
		return TemplatePage{}, err
	}
	if len(items) <= limit {
		return TemplatePage{Items: items}, nil
	}
	items = items[:limit]
	last := items[len(items)-1]
	return TemplatePage{Items: items, NextCursor: EncodeCursor(domain.Cursor{CreatedAt: last.CreatedAt, ID: last.ID})}, nil
}
