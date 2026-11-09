package externalservice

import (
	"context"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// Repo backs the `externalService` table. It returns the RAW (still-encrypted)
// documents; the Service layer decrypts.
type Repo struct {
	col *pgdoc.Store
}

func NewRepo(db *pgdoc.DB) *Repo {
	return &Repo{col: db.C("externalService")}
}

// FindByID returns the service by _id (nil when absent).
func (r *Repo) FindByID(ctx context.Context, id string) (*ExternalService, error) {
	var es ExternalService
	found, err := r.col.Get(ctx, id, &es)
	if err != nil || !found {
		return nil, err
	}
	return &es, nil
}

// FindAll returns every external service.
func (r *Repo) FindAll(ctx context.Context) ([]ExternalService, error) {
	return r.find(ctx, pgdoc.M{})
}

// FindByType returns services of one type.
func (r *Repo) FindByType(ctx context.Context, t string) ([]ExternalService, error) {
	return r.find(ctx, pgdoc.M{"type": t})
}

// SetTemplateImage moves a sandbox template to another image only while it still holds from (an operator's edit in
// the meantime wins). false: nothing written.
func (r *Repo) SetTemplateImage(ctx context.Context, id, tmpl, from, to string) (bool, error) {
	f := "config.sandbox.templates." + tmpl + ".image"
	return r.col.SetFieldsOne(ctx, pgdoc.M{"_id": id, f: from}, pgdoc.M{f: to}, nil)
}

func (r *Repo) find(ctx context.Context, filter pgdoc.M) ([]ExternalService, error) {
	out := []ExternalService{}
	if err := r.col.Find(ctx, filter, &out); err != nil {
		return nil, err
	}
	return out, nil
}
