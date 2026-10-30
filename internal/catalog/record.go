package catalog

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"
)

type RecordType string

const (
	RecordUpsert RecordType = "upsert"
	RecordWithdraw RecordType = "withdraw"
)

// Limits applied to supplier text so one bad record cannot bloat the database.
const (
	maxEventIDLen     = 100
	maxNameLen        = 200
	maxDescriptionLen = 2000
	maxCategoryLen    = 100
)

var (
	skuPattern      = regexp.MustCompile(`^[A-Z0-9][A-Z0-9._-]{0,63}$`)
	currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)
)

// Record is one validated, normalised supplier record.
type Record struct {
	EventID     string
	SKU         string
	Type        RecordType
	Version     int64
	Name        string
	Description string
	Category    string
	PriceCents  int64
	Currency    string
}

type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string { return strings.Join(e.Problems, "; ") }

// wireRecord mirrors the supplier's JSON. Price is a pointer so that a missing
// price can be told apart from a price of zero.
type wireRecord struct {
	EventID     string `json:"event_id"`
	SKU         string `json:"sku"`
	Type        string `json:"type"`
	Version     int64  `json:"version"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Category    string `json:"category"`
	PriceCents  *int64 `json:"price_cents"`
	Currency    string `json:"currency"`
}


func NormalizeSKU(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

// ValidSKU reports whether an already normalised SKU is acceptable.
func ValidSKU(s string) bool { return skuPattern.MatchString(s) }


func ParseRecord(raw []byte) (Record, error) {
	var w wireRecord
	if err := json.Unmarshal(raw, &w); err != nil {
		return Record{}, &ValidationError{Problems: []string{
			"record is not a JSON object with the expected field types: " + err.Error(),
		}}
	}

	rec := Record{
		EventID:     strings.TrimSpace(w.EventID),
		SKU:         NormalizeSKU(w.SKU),
		Type:        RecordType(strings.ToLower(strings.TrimSpace(w.Type))),
		Version:     w.Version,
		Name:        strings.TrimSpace(w.Name),
		Description: strings.TrimSpace(w.Description),
		Category:    strings.TrimSpace(w.Category),
		Currency:    strings.ToUpper(strings.TrimSpace(w.Currency)),
	}

	var problems []string
	add := func(msg string) { problems = append(problems, msg) }

	switch {
	case rec.SKU == "":
		add("sku is required")
	case !ValidSKU(rec.SKU):
		add("sku must be 1-64 characters: letters, digits, '.', '_' or '-'")
	}

	if rec.Type != RecordUpsert && rec.Type != RecordWithdraw {
		add(`type must be "upsert" or "withdraw"`)
	}
	if rec.Version < 1 {
		add("version must be a positive integer")
	}
	if utf8.RuneCountInString(rec.EventID) > maxEventIDLen {
		add("event_id is too long")
	}

	if rec.Type == RecordUpsert {
		if rec.Name == "" {
			add("name is required")
		} else if utf8.RuneCountInString(rec.Name) > maxNameLen {
			add("name is too long")
		}
		if utf8.RuneCountInString(rec.Description) > maxDescriptionLen {
			add("description is too long")
		}
		if utf8.RuneCountInString(rec.Category) > maxCategoryLen {
			add("category is too long")
		}
		switch {
		case w.PriceCents == nil:
			add("price_cents is required")
		case *w.PriceCents < 0:
			add("price_cents must not be negative")
		default:
			rec.PriceCents = *w.PriceCents
		}
		if !currencyPattern.MatchString(rec.Currency) {
			add("currency must be a 3-letter code such as USD")
		}
	}

	if len(problems) > 0 {
		return Record{}, &ValidationError{Problems: problems}
	}
	return rec, nil
}
