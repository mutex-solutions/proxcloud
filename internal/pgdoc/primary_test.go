package pgdoc

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

type primaryRow struct{ recovery, readonly bool }

func (r primaryRow) Scan(dest ...any) error {
	*dest[0].(*bool) = r.recovery
	*dest[1].(*bool) = r.readonly
	return nil
}

type primaryProbe struct{ row primaryRow }

func (q primaryProbe) QueryRow(context.Context, string, ...any) pgx.Row { return q.row }
func TestRequirePrimaryRejectsWrongPool(t *testing.T) {
	for _, tc := range []primaryRow{{true, false}, {false, true}, {true, true}} {
		if RequirePrimary(context.Background(), primaryProbe{tc}) == nil {
			t.Fatalf("replica/read-only accepted: %+v", tc)
		}
	}
	if err := RequirePrimary(context.Background(), primaryProbe{}); err != nil {
		t.Fatal(err)
	}
}
