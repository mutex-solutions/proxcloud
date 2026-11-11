// Package message is the message-template system: named, admin-editable email bodies
// (collection "messageTemplate") rendered with Mustache-style {{var}} placeholders. System
// templates are seeded if-absent at startup. The branded outer shell (a Jinja
// defaultTemplate.html) is applied by the mail.Service wrapper.
package message

import (
	"context"
	"fmt"
	"html"
	"regexp"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// MessageTemplate is a document in the "messageTemplate" collection.
type MessageTemplate struct {
	ID             string     `json:"id,omitempty"`
	Key            string     `json:"key,omitempty"`
	Disabled       bool       `json:"disabled"`
	SystemTemplate bool       `json:"systemTemplate"`
	Category       string     `json:"category,omitempty"`
	MessageTitle   string     `json:"messageTitle,omitempty"`
	MessageBody    string     `json:"messageBody,omitempty"`
	CreatedAt      *time.Time `json:"createdAt,omitempty"`
	UpdatedAt      *time.Time `json:"updatedAt,omitempty"`
}

// Rendered is the per-message output: a title and a body.
type Rendered struct {
	Title string
	Body  string
}

// Repo backs the messageTemplate collection.
type Repo struct{ col *pgdoc.Store }

func NewRepo(db *pgdoc.DB) *Repo { return &Repo{col: db.C("messageTemplate")} }

// ByKey loads a template by key. nil if absent.
func (r *Repo) ByKey(ctx context.Context, key string) (*MessageTemplate, error) {
	var t MessageTemplate
	found, err := r.col.FindOne(ctx, pgdoc.M{"key": key}, &t)
	if err != nil || !found {
		return nil, err
	}
	return &t, nil
}

// ExistsByKey reports whether a template with the key exists (the seed's create-if-absent guard).
func (r *Repo) ExistsByKey(ctx context.Context, key string) (bool, error) {
	return r.col.Exists(ctx, pgdoc.M{"key": key})
}

// Create inserts a template + back-fills its id.
func (r *Repo) Create(ctx context.Context, t *MessageTemplate) error {
	now := time.Now().UTC()
	t.CreatedAt, t.UpdatedAt = &now, &now
	id, err := r.col.InsertOne(ctx, t)
	if err != nil {
		return err
	}
	t.ID = id
	return nil
}

// Render Mustache-substitutes the template body with the scope vars → Rendered{title, body}.
// ok=false when the template is missing or disabled (the gated email side-effects degrade to
// a no-op).
func (r *Repo) Render(ctx context.Context, key string, vars map[string]any) (Rendered, bool, error) {
	t, err := r.ByKey(ctx, key)
	if err != nil || t == nil || t.Disabled {
		return Rendered{}, false, err
	}
	return Rendered{Title: renderMustacheText(t.MessageTitle, vars), Body: renderMustache(t.MessageBody, vars)}, true, nil
}

// mustacheRe matches {{var}} (HTML-escaped) and {{{var}}} (unescaped) placeholders.
var mustacheRe = regexp.MustCompile(`\{\{\{?\s*(\w+)\s*\}?\}\}`)

// renderMustacheText substitutes {{var}} placeholders in a plain-text context (the mail
// subject): no HTML escaping, missing vars render empty.
func renderMustacheText(tmpl string, vars map[string]any) string {
	return mustacheRe.ReplaceAllStringFunc(tmpl, func(m string) string {
		sub := mustacheRe.FindStringSubmatch(m)
		v, ok := vars[sub[1]]
		if !ok {
			return ""
		}
		return fmt.Sprintf("%v", v)
	})
}

// renderMustache does flat Mustache variable substitution (sufficient for the system templates —
// they use only {{var}} placeholders, no sections/partials). Missing vars render empty (Mustache).
func renderMustache(tmpl string, vars map[string]any) string {
	return mustacheRe.ReplaceAllStringFunc(tmpl, func(m string) string {
		sub := mustacheRe.FindStringSubmatch(m)
		v, ok := vars[sub[1]]
		if !ok {
			return ""
		}
		s := fmt.Sprintf("%v", v)
		if strings.HasPrefix(m, "{{{") { // triple-stache → unescaped
			return s
		}
		return html.EscapeString(s)
	})
}
