package admin

import (
	"context"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// messagetemplate_repo.go holds the MessageTemplate-specific admin.Repo methods that the generic
// crud.go helpers don't cover (the key-existence guard used on create).

// MessageTemplateExistsByKey reports whether a message-template
// document has the given `key`. An empty key short-circuits to false (the body's key is required, but
// an empty value can never collide with a stored non-empty key — save then fails
// downstream; the existence check itself just returns false).
func (r *Repo) MessageTemplateExistsByKey(ctx context.Context, key string) (bool, error) {
	return r.c(messageTemplateCollection).Exists(ctx, pgdoc.M{"key": key})
}
