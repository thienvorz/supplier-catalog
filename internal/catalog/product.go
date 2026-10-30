package catalog

import "time"

// Status says whether a product is currently sold.
type Status string

const (
	StatusActive    Status = "active"
	StatusWithdrawn Status = "withdrawn"
)


type Product struct {
	SKU         string     `json:"sku"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Category    string     `json:"category"`
	PriceCents  int64      `json:"price_cents"`
	Currency    string     `json:"currency"`
	Status      Status     `json:"status"`
	Version     int64      `json:"version"`
	WithdrawnAt *time.Time `json:"withdrawn_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}
