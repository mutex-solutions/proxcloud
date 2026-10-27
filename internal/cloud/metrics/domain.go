package metrics

import (
	"time"

	"github.com/shopspring/decimal"
)

// BillBillingCycle is the billing-cycle window (also embedded in bills).
type BillBillingCycle struct {
	StartDate *time.Time `json:"startDate,omitempty"`
	EndDate   *time.Time `json:"endDate,omitempty"`
}

// GnocchiMetricsDetails holds network traffic in MB,
// public/private incoming/outgoing + totals (decimal.Decimal,
// stored as decimal strings in jsonb).
type GnocchiMetricsDetails struct {
	IncomingPublicTrafficMb  decimal.Decimal `json:"incomingPublicTrafficMb"`
	IncomingPrivateTrafficMb decimal.Decimal `json:"incomingPrivateTrafficMb"`
	OutgoingPublicTrafficMb  decimal.Decimal `json:"outgoingPublicTrafficMb"`
	OutgoingPrivateTrafficMb decimal.Decimal `json:"outgoingPrivateTrafficMb"`
	TotalPublicTrafficMb     decimal.Decimal `json:"totalPublicTrafficMb"`
	TotalPrivateTrafficMb    decimal.Decimal `json:"totalPrivateTrafficMb"`
	TotalTrafficMb           decimal.Decimal `json:"totalTrafficMb"`
}

// OstorMetrics holds S3/Ostor object-store counters.
type OstorMetrics struct {
	DownloadedBytes int64 `json:"downloadedBytes"`
	UploadedBytes   int64 `json:"uploadedBytes"`
	Puts            int64 `json:"puts"`
	Gets            int64 `json:"gets"`
	Lists           int64 `json:"lists"`
	Others          int64 `json:"others"`
}

// GnocchiMetrics is the gnocchiMetrics collection document — one per (resource, billing
// cycle), holding the month's accumulated usage that the rating cron charges from.
type GnocchiMetrics struct {
	ID           string                 `json:"id,omitempty"`
	ResourceID   string                 `json:"resourceId,omitempty"`
	ResourceType string                 `json:"resourceType,omitempty"`
	BillingCycle *BillBillingCycle      `json:"billingCycle,omitempty"`
	Details      *GnocchiMetricsDetails `json:"details,omitempty"`
	OstorMetrics *OstorMetrics          `json:"ostorMetrics,omitempty"`
	CreatedAt    *time.Time             `json:"createdAt,omitempty"`
	UpdatedAt    *time.Time             `json:"updatedAt,omitempty"`
}

// zeroDetails is the zero-initialised details for a freshly-created month doc.
func zeroDetails() *GnocchiMetricsDetails {
	return &GnocchiMetricsDetails{
		IncomingPublicTrafficMb:  decimal.Zero,
		IncomingPrivateTrafficMb: decimal.Zero,
		OutgoingPublicTrafficMb:  decimal.Zero,
		OutgoingPrivateTrafficMb: decimal.Zero,
		TotalPublicTrafficMb:     decimal.Zero,
		TotalPrivateTrafficMb:    decimal.Zero,
		TotalTrafficMb:           decimal.Zero,
	}
}
