package pgdoc

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestTxRequiresOwnedTransaction(t *testing.T) {
	a, b := &DB{}, &DB{}
	var tx pgx.Tx = &pgxTxStub{}
	if _, err := a.Tx(context.Background()); err == nil {
		t.Fatal("missing transaction accepted")
	}
	ctx := context.WithValue(context.Background(), txKey{}, tx)
	ctx = context.WithValue(ctx, txOwnerKey{}, a)
	if got, err := a.Tx(ctx); err != nil || got != tx {
		t.Fatalf("owned transaction = %v %v", got, err)
	}
	if _, err := b.Tx(ctx); err == nil {
		t.Fatal("another database's transaction accepted")
	}
	called := false
	if err := b.WithTx(ctx, func(context.Context) error { called = true; return nil }); err == nil || called {
		t.Fatal("nested transaction silently crossed databases")
	}
}

func TestRejectInheritedTransactionWithoutContextStripping(t *testing.T) {
	if err := RejectInheritedTransaction(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []*DB{{}, {}} {
		ctx := context.WithValue(context.Background(), txKey{}, &pgxTxStub{})
		ctx = context.WithValue(ctx, txOwnerKey{}, owner)
		if err := RejectInheritedTransaction(ctx); !errors.Is(err, ErrInheritedTransaction) {
			t.Fatalf("inherited transaction accepted: %v", err)
		}
	}
}

// Embedded interface suffices: this test never executes SQL.
type pgxTxStub struct{ pgx.Tx }
