// Package crm is the CRM-sync integration (contacts / segments / user sync + the HubSpot/Segment-style
// integrations). No CRM platform is connected yet, so the default Provider is a StubProvider
// (no-op): structure + sync job + tests exist and pass; a real CRM plugs in later by
// implementing Provider + wiring the crons. "Implement + test, integrate later".
package crm

import "context"

// Record is one entity pushed to the CRM (a user or a contact); the concrete shape is the vendor's
// concern — the sync passes the domain object through opaquely.
type Record struct {
	ID    string
	Email string
	Attrs map[string]any
}

// Provider pushes records to an external CRM.
type Provider interface {
	SyncContacts(ctx context.Context, records []Record) error
	SyncUsers(ctx context.Context, records []Record) error
}

// StubProvider is the default until a CRM is integrated: it drops everything.
type StubProvider struct{}

func (StubProvider) SyncContacts(context.Context, []Record) error { return nil }
func (StubProvider) SyncUsers(context.Context, []Record) error    { return nil }

// Source supplies the records to sync (a user/contact repo adapter binds this when a CRM lands).
type Source interface {
	Contacts(ctx context.Context) ([]Record, error)
	Users(ctx context.Context) ([]Record, error)
}

// SyncJob pushes the platform's contacts + users to the CRM. With the StubProvider it is a
// no-op. Returns the count pushed.
type SyncJob struct {
	source   Source
	provider Provider
}

func NewSyncJob(source Source, provider Provider) *SyncJob {
	if provider == nil {
		provider = StubProvider{}
	}
	return &SyncJob{source: source, provider: provider}
}

func (j *SyncJob) Run(ctx context.Context) (int, error) {
	contacts, err := j.source.Contacts(ctx)
	if err != nil {
		return 0, err
	}
	if err := j.provider.SyncContacts(ctx, contacts); err != nil {
		return 0, err
	}
	users, err := j.source.Users(ctx)
	if err != nil {
		return len(contacts), err
	}
	if err := j.provider.SyncUsers(ctx, users); err != nil {
		return len(contacts), err
	}
	return len(contacts) + len(users), nil
}
