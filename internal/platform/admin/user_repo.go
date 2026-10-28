package admin

import (
	"context"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
)

// user_repo.go holds the domain finders the user admin surface needs that the generic crud.go helpers
// don't cover: typed User decoding (so the User domain MarshalJSON — consent/customInfo always
// present + computed `language` — applies on the wire) and the project-owner "in use" check.

// userByID loads a user by id: a typed User, or (nil,nil) when absent. The
// caller maps nil → the 404 "User with id %s not found ".
func (r *Repo) userByID(ctx context.Context, id string) (*user.User, error) {
	var u user.User
	found, err := r.c("users").Get(ctx, id, &u)
	if err != nil || !found {
		return nil, err
	}
	return &u, nil
}

// userBySub loads the newest user by sub: the newest User with
// that sub, or (nil,nil) when none (the caller returns a missing user as an empty {}).
func (r *Repo) userBySub(ctx context.Context, sub string) (*user.User, error) {
	if sub == "" {
		return nil, nil
	}
	var u user.User
	found, err := r.c("users").FindOne(ctx, pgdoc.M{"sub": sub}, &u,
		pgdoc.Sort(pgdoc.DescK("createdAt", pgdoc.KTime)))
	if err != nil || !found {
		return nil, err
	}
	return &u, nil
}

// userByEmail loads the first user by email: the first User with that email, or
// (nil,nil) when none (the caller maps non-nil → 400 "User with this email already exists").
func (r *Repo) userByEmail(ctx context.Context, email string) (*user.User, error) {
	if email == "" {
		return nil, nil
	}
	var u user.User
	found, err := r.c("users").FindOne(ctx, pgdoc.M{"email": email}, &u)
	if err != nil || !found {
		return nil, err
	}
	return &u, nil
}

// insertUserDoc writes the user document shape (with the
// EXPLICIT empty arrays/maps — consent [], customInfo {}, identities [], services [], metadata {})
// keyed by a hex-string _id (the users-collection convention the adminapi create established).
func (r *Repo) insertUserDoc(ctx context.Context, doc pgdoc.M) (pgdoc.M, error) {
	doc["_id"] = pgdoc.NewID()
	if _, err := r.c("users").InsertOne(ctx, doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// deleteUserByID removes the user doc.
func (r *Repo) deleteUserByID(ctx context.Context, id string) error {
	_, err := r.c("users").DeleteByID(ctx, id)
	return err
}

// projectByID loads a project by id: the raw project
// doc, or (nil,nil) when absent (the caller maps nil → the
// 404 "The project with id %s was not found. ").
func (r *Repo) projectByID(ctx context.Context, id string) (pgdoc.M, error) {
	return r.FindDoc(ctx, "project", id)
}

// userInUse reports whether
// any project has the (deprecated) `owner` field equal to the sub.
func (r *Repo) userInUse(ctx context.Context, sub string) (bool, error) {
	if sub == "" {
		return false, nil
	}
	return r.c("project").Exists(ctx, pgdoc.M{"owner": sub})
}
