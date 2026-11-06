package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/admin"
)

// adminKeyCmd is the non-interactive operator credential (plan AUTO-G5): `proxcloud-api admin-key
// create` mints an Admin-API SigV4 key directly in PostgreSQL and prints {id, secretKey} once. It
// needs the pod's database URL, so it is exactly as privileged as the shell that runs it, which is
// why it can exist with no debug trigger and no first admin login. The installer runs it through
// `kubectl exec`.
func adminKeyCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("admin-key create", flag.ContinueOnError)
	description := fs.String("description", "", "what this key is for")
	if len(args) == 0 || args[0] != "create" {
		return errors.New("usage: proxcloud-api admin-key create [-description text]")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	dsn := os.Getenv("PROXCLOUD_DB_URL")
	if dsn == "" {
		return errors.New("PROXCLOUD_DB_URL is not set")
	}
	ctx := context.Background()
	pg, err := pgdoc.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer func() { _ = pg.Close(ctx) }()
	doc := admin.NewAdminAPIKey(*description)
	if _, err := pg.C("hmac_keys").InsertOne(ctx, doc); err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(map[string]any{"id": doc["_id"], "secretKey": doc["secretKey"]})
}
