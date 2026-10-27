package crm

import (
	"context"
	"testing"
)

type fakeSource struct{ contacts, users []Record }

func (f fakeSource) Contacts(context.Context) ([]Record, error) { return f.contacts, nil }
func (f fakeSource) Users(context.Context) ([]Record, error)    { return f.users, nil }

type captureProvider struct{ contacts, users int }

func (c *captureProvider) SyncContacts(_ context.Context, r []Record) error {
	c.contacts += len(r)
	return nil
}
func (c *captureProvider) SyncUsers(_ context.Context, r []Record) error {
	c.users += len(r)
	return nil
}

func TestSyncPushesContactsAndUsers(t *testing.T) {
	src := fakeSource{
		contacts: []Record{{ID: "c1"}, {ID: "c2"}},
		users:    []Record{{ID: "u1"}},
	}
	cp := &captureProvider{}
	n, err := NewSyncJob(src, cp).Run(context.Background())
	if err != nil || n != 3 {
		t.Fatalf("synced=%d err=%v, want 3", n, err)
	}
	if cp.contacts != 2 || cp.users != 1 {
		t.Fatalf("provider got contacts=%d users=%d", cp.contacts, cp.users)
	}
}

func TestStubProviderIsNoop(t *testing.T) {
	src := fakeSource{contacts: []Record{{ID: "c1"}}, users: []Record{{ID: "u1"}}}
	n, err := NewSyncJob(src, nil).Run(context.Background()) // nil → StubProvider (no-op, but counts)
	if err != nil || n != 2 {
		t.Fatalf("stub run: n=%d err=%v, want 2", n, err)
	}
}
