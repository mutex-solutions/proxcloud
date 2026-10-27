package admin

import (
	"context"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// builtininvoice_repo.go: the one BuiltInInvoice domain query crud.go does not cover — a filtered +
// createdAt-DESC list by invoiceGatewayId.

// BuiltInInvoicesByGateway lists `builtInInvoice` docs for an invoiceGatewayId, createdAt DESC
// (never nil). raw document — the caller shapeDoc()s each before writing.
func (r *Repo) BuiltInInvoicesByGateway(ctx context.Context, invoiceGatewayId string) ([]pgdoc.M, error) {
	out := []pgdoc.M{}
	if err := r.c("builtInInvoice").Find(ctx,
		pgdoc.M{"invoiceGatewayId": invoiceGatewayId}, &out,
		pgdoc.Sort(pgdoc.DescK("createdAt", pgdoc.KTime))); err != nil {
		return nil, err
	}
	return out, nil
}
