package catalog

import (
	"fmt"
	"strings"
	"time"
)

type Outcome string

const (

	OutcomeCreated   Outcome = "created"   // new product
	OutcomeUpdated   Outcome = "updated"   // details of an active product changed
	OutcomeRelisted  Outcome = "relisted"  // withdrawn product returned by a NEWER version
	OutcomeWithdrawn Outcome = "withdrawn" // product withdrawn from sale

	OutcomeUnchanged Outcome = "unchanged"

	OutcomeDuplicate Outcome = "duplicate" // same version, same content: a repeat
	OutcomeStale     Outcome = "stale"     // older than what we have: ignored
	OutcomeConflict  Outcome = "conflict"  // same version, different content: needs a human
)

func (o Outcome) WritesProduct() bool {
	switch o {
	case OutcomeCreated, OutcomeUpdated, OutcomeRelisted, OutcomeWithdrawn, OutcomeUnchanged:
		return true
	}
	return false
}

// IsChange reports whether the outcome is recorded in the change history.
func (o Outcome) IsChange() bool {
	switch o {
	case OutcomeCreated, OutcomeUpdated, OutcomeRelisted, OutcomeWithdrawn:
		return true
	}
	return false
}

// Decision is the result of Decide.
type Decision struct {
	Outcome Outcome
	Product Product
	ChangedFields []string
	Reason string
}


func Decide(existing *Product, rec Record, now time.Time) Decision {
	if existing == nil {
		if rec.Type == RecordWithdraw {
			return Decision{
				Outcome:       OutcomeWithdrawn,
				Product:       tombstone(rec, now),
				ChangedFields: []string{"status"},
			}
		}
		p := Product{SKU: rec.SKU, CreatedAt: now}
		applyDetails(&p, rec, now)
		return Decision{Outcome: OutcomeCreated, Product: p, ChangedFields: allFields()}
	}

	switch {
	case rec.Version < existing.Version:
		return Decision{Outcome: OutcomeStale}

	case rec.Version == existing.Version:
		if sameState(*existing, rec) {
			return Decision{Outcome: OutcomeDuplicate}
		}
		return Decision{Outcome: OutcomeConflict, Reason: conflictReason(*existing, rec)}
	}

	// rec.Version > existing.Version from here on.
	next := *existing
	next.Version = rec.Version

	if rec.Type == RecordWithdraw {
		if existing.Status == StatusWithdrawn {
			return Decision{Outcome: OutcomeUnchanged, Product: next}
		}
		next.Status = StatusWithdrawn
		next.WithdrawnAt = &now
		next.UpdatedAt = now
		return Decision{Outcome: OutcomeWithdrawn, Product: next, ChangedFields: []string{"status"}}
	}

	changed := changedFields(*existing, rec)
	if existing.Status == StatusWithdrawn {
		applyDetails(&next, rec, now)
		return Decision{Outcome: OutcomeRelisted, Product: next, ChangedFields: append([]string{"status"}, changed...)}
	}
	if len(changed) == 0 {
		return Decision{Outcome: OutcomeUnchanged, Product: next}
	}
	applyDetails(&next, rec, now)
	return Decision{Outcome: OutcomeUpdated, Product: next, ChangedFields: changed}
}

func tombstone(rec Record, now time.Time) Product {
	return Product{
		SKU:         rec.SKU,
		Status:      StatusWithdrawn,
		Version:     rec.Version,
		WithdrawnAt: &now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}

// applyDetails copies the supplier's details onto p and marks it active.
func applyDetails(p *Product, rec Record, now time.Time) {
	p.Name = rec.Name
	p.Description = rec.Description
	p.Category = rec.Category
	p.PriceCents = rec.PriceCents
	p.Currency = rec.Currency
	p.Status = StatusActive
	p.WithdrawnAt = nil
	p.Version = rec.Version
	p.UpdatedAt = now
}

func allFields() []string {
	return []string{"name", "description", "category", "price_cents", "currency"}
}

// changedFields lists which details of an upsert differ from the stored product.
func changedFields(p Product, rec Record) []string {
	var out []string
	if p.Name != rec.Name {
		out = append(out, "name")
	}
	if p.Description != rec.Description {
		out = append(out, "description")
	}
	if p.Category != rec.Category {
		out = append(out, "category")
	}
	if p.PriceCents != rec.PriceCents {
		out = append(out, "price_cents")
	}
	if p.Currency != rec.Currency {
		out = append(out, "currency")
	}
	return out
}

// sameState reports whether a record at the SAME version as the stored product
// says exactly what the product already says.
func sameState(p Product, rec Record) bool {
	if rec.Type == RecordWithdraw {
		return p.Status == StatusWithdrawn
	}
	return p.Status == StatusActive && len(changedFields(p, rec)) == 0
}

func conflictReason(p Product, rec Record) string {
	switch {
	case rec.Type == RecordWithdraw:
		return fmt.Sprintf("supplier sent a withdrawal at version %d, but we already applied an upsert at that version", rec.Version)
	case p.Status == StatusWithdrawn:
		return fmt.Sprintf("supplier sent an upsert at version %d, but we already applied a withdrawal at that version", rec.Version)
	default:
		return fmt.Sprintf("version %d was already applied with different content (differs in: %s)",
			rec.Version, strings.Join(changedFields(p, rec), ", "))
	}
}
