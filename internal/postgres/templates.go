package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
)

// Templates are only inserted and read: no UPDATE or DELETE exists for them
// (K33).
const (
	templateColumns = `id::text, name, channel, body, created_at`

	insertTemplateSQL = `
		INSERT INTO templates (name, channel, body)
		VALUES ($1, $2, $3)
		RETURNING ` + templateColumns

	getTemplateSQL = `SELECT ` + templateColumns + ` FROM templates WHERE id = $1::text::uuid`

	// loadTemplatesSQL reads the templates a create or batch request names,
	// with one query inside its transaction (K33).
	loadTemplatesSQL = `SELECT ` + templateColumns + ` FROM templates WHERE id = ANY($1::text[]::uuid[])`

	listTemplatesSQL = `SELECT ` + templateColumns + ` FROM templates
		WHERE $1::timestamptz IS NULL OR (created_at, id) < ($1::timestamptz, $2::text::uuid)
		ORDER BY created_at DESC, id DESC
		LIMIT $3`
)

func scanTemplate(row pgx.Row) (domain.Template, error) {
	var (
		t       domain.Template
		channel string
	)
	if err := row.Scan(&t.ID, &t.Name, &channel, &t.Body, &t.CreatedAt); err != nil {
		return domain.Template{}, err
	}
	t.Channel = domain.Channel(channel)
	return t, nil
}

// CreateTemplate saves a template that passed the K47 rules.
func (s *Store) CreateTemplate(ctx context.Context, t domain.NewTemplate) (domain.Template, error) {
	out, err := scanTemplate(s.pool.QueryRow(ctx, insertTemplateSQL, t.Name, string(t.Channel), t.Body))
	if err != nil {
		return domain.Template{}, fmt.Errorf("insert template: %w", err)
	}
	return out, nil
}

// GetTemplate reads one template; domain.ErrTemplateNotFound when it does not
// exist.
func (s *Store) GetTemplate(ctx context.Context, id string) (domain.Template, error) {
	t, err := scanTemplate(s.pool.QueryRow(ctx, getTemplateSQL, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Template{}, domain.ErrTemplateNotFound
	}
	if err != nil {
		return domain.Template{}, fmt.Errorf("get template: %w", err)
	}
	return t, nil
}

// ListTemplates returns at most f.Limit templates newest first by
// (created_at, id); with f.After, only those below it.
func (s *Store) ListTemplates(ctx context.Context, f domain.ListFilter) ([]domain.Template, error) {
	var (
		after   any
		afterID any
	)
	if f.After != nil {
		after, afterID = f.After.CreatedAt, f.After.ID
	}
	rows, err := s.pool.Query(ctx, listTemplatesSQL, after, afterID, f.Limit)
	if err != nil {
		return nil, fmt.Errorf("list templates: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Template, error) {
		return scanTemplate(row)
	})
	if err != nil {
		return nil, fmt.Errorf("list templates: %w", err)
	}
	return out, nil
}

// loadTemplates reads the templates the items name, once each.
func loadTemplates(ctx context.Context, tx pgx.Tx, items []domain.NewNotification) (domain.Templates, error) {
	seen := map[string]bool{}
	var ids []string
	for _, n := range items {
		if n.TemplateID != "" && !seen[n.TemplateID] {
			seen[n.TemplateID] = true
			ids = append(ids, n.TemplateID)
		}
	}
	out := domain.Templates{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx, loadTemplatesSQL, ids)
	if err != nil {
		return nil, fmt.Errorf("load templates: %w", err)
	}
	templates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Template, error) {
		return scanTemplate(row)
	})
	if err != nil {
		return nil, fmt.Errorf("load templates: %w", err)
	}
	for _, t := range templates {
		out[t.ID] = t
	}
	return out, nil
}

// templateArg is the template_id parameter: null for plain content.
func templateArg(n domain.NewNotification) *string {
	if n.TemplateID == "" {
		return nil
	}
	return &n.TemplateID
}
