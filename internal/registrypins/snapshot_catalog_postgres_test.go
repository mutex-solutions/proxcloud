package registrypins

import "testing"

// Sanitized catalog only: no data, connection strings or raw marker rows. The
// shared opt-in fixture creates a fresh owned database and never starts a server.
func TestSnapshotPostgresSanitizedCatalog(t *testing.T) {
	db, _, _ := importPGFixture(t)
	var raw string
	err := db.Pool.QueryRow(t.Context(), `SELECT jsonb_build_object(
 'constraints',(SELECT jsonb_agg(jsonb_build_object('name',conname,'type',contype,'validated',convalidated,'definition',pg_get_constraintdef(oid)) ORDER BY conname) FROM pg_constraint WHERE conrelid='registry_snapshot_dispatch'::regclass),
 'columns',(SELECT jsonb_agg(jsonb_build_object('name',a.attname,'type',format_type(a.atttypid,a.atttypmod),'notNull',a.attnotnull,'default',pg_get_expr(d.adbin,d.adrelid)) ORDER BY a.attnum) FROM pg_attribute a LEFT JOIN pg_attrdef d ON d.adrelid=a.attrelid AND d.adnum=a.attnum WHERE a.attrelid='registry_snapshot_dispatch'::regclass AND a.attnum>0 AND NOT a.attisdropped),
 'indexes',(SELECT jsonb_agg(jsonb_build_object('name',c.relname,'definition',pg_get_indexdef(i.indexrelid)) ORDER BY c.relname) FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid WHERE i.indrelid='registry_snapshot_dispatch'::regclass))::text`).Scan(&raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("SNAPSHOT_CATALOG=" + raw)
}
