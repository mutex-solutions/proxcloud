package admin

import (
	"context"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// adminpermission_repo.go holds the admin-permission repo methods. The
// adminPermission collection keys `_id` by the user sub (a non-hex string), not a generated
// id, so the generic InsertDoc (which strips id + generates one) does not fit —
// grant/update must save with a caller-chosen `_id`.

// SaveAdminPermission saves a granted permission and is also reused by
// the role-update path: build an AdminPermission with `_id` = sub-when-known else email, the email, the role,
// and pending = (sub == "") (a pending grant has no resolved User sub yet). Upserts by `_id`
// and returns the stored value. Optional blank email/role are omitted
// from the stored doc so the JSON drops them (a null field is dropped, not stored as "").
func (r *Repo) SaveAdminPermission(ctx context.Context, sub, email, role string) (*AdminPermission, error) {
	key := sub
	if key == "" {
		key = email
	}
	pending := sub == ""

	// The doc is fully determined by (email, role, pending) — a full-replace upsert is the
	// faithful equivalent of the previous set-or-clear upsert (blank optionals are absent).
	doc := pgdoc.M{"pending": pending}
	if email != "" {
		doc["email"] = email
	}
	if role != "" {
		doc["role"] = role
	}
	if err := r.col.Upsert(ctx, key, doc); err != nil {
		return nil, err
	}
	return &AdminPermission{Sub: key, Email: email, Role: role, Pending: pending}, nil
}

// UpdateAdminPermissionRole overwrites ONLY the role
// of an existing adminPermission (sub/email/pending untouched). A blank role becomes null (removed)
// when empty. The caller has already confirmed the doc exists (looked up by id → 404 when absent).
func (r *Repo) UpdateAdminPermissionRole(ctx context.Context, sub, role string) error {
	var err error
	if role == "" {
		_, err = r.col.SetByID(ctx, sub, nil, []string{"role"})
	} else {
		_, err = r.col.SetByID(ctx, sub, pgdoc.M{"role": role}, nil)
	}
	return err
}
