package catalog

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

func upsert(sku string, version int64, name string, price int64) Record {
	return Record{SKU: sku, Type: RecordUpsert, Version: version, Name: name, PriceCents: price, Currency: "USD", Category: "Desk"}
}

func withdraw(sku string, version int64) Record {
	return Record{SKU: sku, Type: RecordWithdraw, Version: version}
}

func active(sku string, version int64, name string, price int64) *Product {
	p := Product{SKU: sku, Name: name, PriceCents: price, Currency: "USD", Category: "Desk",
		Status: StatusActive, Version: version, CreatedAt: t0, UpdatedAt: t0}
	return &p
}

func withdrawn(sku string, version int64) *Product {
	w := t0
	return &Product{SKU: sku, Name: "Old name", Status: StatusWithdrawn, Version: version,
		WithdrawnAt: &w, CreatedAt: t0, UpdatedAt: t0}
}

func TestDecide(t *testing.T) {
	later := t0.Add(time.Hour)

	tests := []struct {
		name         string
		existing     *Product
		rec          Record
		wantOutcome  Outcome
		wantVersion  int64 // version of Decision.Product when the product is written
		wantStatus   Status
		wantFields   []string
		wantConflict bool
	}{
		{
			name: "new product is created", existing: nil, rec: upsert("A", 1, "Desk", 100),
			wantOutcome: OutcomeCreated, wantVersion: 1, wantStatus: StatusActive, wantFields: allFields(),
		},
		{
			name: "newer version with changed price updates", existing: active("A", 1, "Desk", 100), rec: upsert("A", 2, "Desk", 150),
			wantOutcome: OutcomeUpdated, wantVersion: 2, wantStatus: StatusActive, wantFields: []string{"price_cents"},
		},
		{
			name: "newer version with several changes lists them all", existing: active("A", 1, "Desk", 100), rec: upsert("A", 3, "Desk XL", 150),
			wantOutcome: OutcomeUpdated, wantVersion: 3, wantStatus: StatusActive, wantFields: []string{"name", "price_cents"},
		},
		{
			name: "newer version with identical content only moves the version", existing: active("A", 1, "Desk", 100), rec: upsert("A", 2, "Desk", 100),
			wantOutcome: OutcomeUnchanged, wantVersion: 2, wantStatus: StatusActive,
		},
		{
			name: "same version same content is a duplicate", existing: active("A", 2, "Desk", 100), rec: upsert("A", 2, "Desk", 100),
			wantOutcome: OutcomeDuplicate,
		},
		{
			name: "older version is stale and does not overwrite", existing: active("A", 5, "Desk", 100), rec: upsert("A", 4, "Cheap desk", 1),
			wantOutcome: OutcomeStale,
		},
		{
			name: "same version different content is a conflict", existing: active("A", 2, "Desk", 100), rec: upsert("A", 2, "Desk", 999),
			wantOutcome: OutcomeConflict, wantConflict: true,
		},
		{
			name: "withdraw newer version withdraws", existing: active("A", 1, "Desk", 100), rec: withdraw("A", 2),
			wantOutcome: OutcomeWithdrawn, wantVersion: 2, wantStatus: StatusWithdrawn, wantFields: []string{"status"},
		},
		{
			name: "withdraw of unknown product stores a tombstone", existing: nil, rec: withdraw("A", 3),
			wantOutcome: OutcomeWithdrawn, wantVersion: 3, wantStatus: StatusWithdrawn, wantFields: []string{"status"},
		},
		{
			name: "stale withdraw does not withdraw a newer active product", existing: active("A", 5, "Desk", 100), rec: withdraw("A", 4),
			wantOutcome: OutcomeStale,
		},
		{
			name: "repeated withdraw is a duplicate", existing: withdrawn("A", 3), rec: withdraw("A", 3),
			wantOutcome: OutcomeDuplicate,
		},
		{
			name: "newer withdraw of withdrawn product only moves the version", existing: withdrawn("A", 3), rec: withdraw("A", 4),
			wantOutcome: OutcomeUnchanged, wantVersion: 4, wantStatus: StatusWithdrawn,
		},
		{
			name: "OLDER upsert must not restore a withdrawn product", existing: withdrawn("A", 3), rec: upsert("A", 2, "Desk", 100),
			wantOutcome: OutcomeStale,
		},
		{
			name: "upsert after tombstone with lower version stays withdrawn", existing: withdrawn("A", 3), rec: upsert("A", 1, "Desk", 100),
			wantOutcome: OutcomeStale,
		},
		{
			name: "same-version upsert must not restore a withdrawn product", existing: withdrawn("A", 3), rec: upsert("A", 3, "Desk", 100),
			wantOutcome: OutcomeConflict, wantConflict: true,
		},
		{
			name: "strictly newer upsert relists a withdrawn product", existing: withdrawn("A", 3), rec: upsert("A", 4, "Desk", 100),
			wantOutcome: OutcomeRelisted, wantVersion: 4, wantStatus: StatusActive,
			wantFields: []string{"status", "name", "category", "price_cents", "currency"},
		},
		{
			name: "same-version withdraw against active product is a conflict", existing: active("A", 3, "Desk", 100), rec: withdraw("A", 3),
			wantOutcome: OutcomeConflict, wantConflict: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var before Product
			if tc.existing != nil {
				before = *tc.existing
			}

			got := Decide(tc.existing, tc.rec, later)

			if got.Outcome != tc.wantOutcome {
				t.Fatalf("outcome = %q, want %q", got.Outcome, tc.wantOutcome)
			}
			if tc.existing != nil && !reflect.DeepEqual(*tc.existing, before) {
				t.Fatalf("Decide must not modify the stored product")
			}
			if tc.wantConflict && got.Reason == "" {
				t.Errorf("conflict must carry a reason for staff")
			}
			if got.Outcome.WritesProduct() {
				if got.Product.Version != tc.wantVersion {
					t.Errorf("product version = %d, want %d", got.Product.Version, tc.wantVersion)
				}
				if got.Product.Status != tc.wantStatus {
					t.Errorf("product status = %q, want %q", got.Product.Status, tc.wantStatus)
				}
				if tc.existing != nil && got.Product.Version <= tc.existing.Version {
					t.Errorf("a written product must have a strictly higher version")
				}
				if got.Product.SKU != tc.rec.SKU {
					t.Errorf("sku = %q, want %q", got.Product.SKU, tc.rec.SKU)
				}
			}
			if tc.wantFields != nil && !reflect.DeepEqual(got.ChangedFields, tc.wantFields) {
				t.Errorf("changed fields = %v, want %v", got.ChangedFields, tc.wantFields)
			}
		})
	}
}

// Whatever order the same set of records arrives in, the final state of a
// product must be the same. This is the heart of "out of order is safe".
func TestDecideIsOrderIndependent(t *testing.T) {
	records := []Record{
		upsert("A", 1, "Desk", 100),
		upsert("A", 2, "Desk", 120),
		withdraw("A", 3),
		upsert("A", 4, "Desk v4", 140),
		upsert("A", 2, "Desk", 120), // repeat
	}

	finalOf := func(order []int) Product {
		var p *Product
		for _, i := range order {
			d := Decide(p, records[i], t0)
			if d.Outcome.WritesProduct() {
				cp := d.Product
				p = &cp
			}
		}
		return *p
	}

	// Compare every permutation against the in-order result on the fields that matter.
	want := finalOf([]int{0, 1, 2, 3, 4})
	if want.Status != StatusActive || want.Version != 4 || want.Name != "Desk v4" {
		t.Fatalf("unexpected reference result: %+v", want)
	}
	permute([]int{0, 1, 2, 3, 4}, func(order []int) {
		got := finalOf(order)
		if got.Version != want.Version || got.Status != want.Status || got.Name != want.Name || got.PriceCents != want.PriceCents {
			t.Errorf("order %v gave %+v, want %+v", order, got, want)
		}
	})
}

func permute(a []int, visit func([]int)) {
	var rec func(k int)
	rec = func(k int) {
		if k == len(a) {
			visit(append([]int(nil), a...))
			return
		}
		for i := k; i < len(a); i++ {
			a[k], a[i] = a[i], a[k]
			rec(k + 1)
			a[k], a[i] = a[i], a[k]
		}
	}
	rec(0)
}

func TestParseRecordValid(t *testing.T) {
	rec, err := ParseRecord([]byte(`{"event_id":" e1 ","sku":"  hp-1011 ","type":"UPSERT","version":2,
		"name":" Wool Throw ","price_cents":4500,"currency":"usd","category":"Home"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.SKU != "HP-1011" || rec.Type != RecordUpsert || rec.Currency != "USD" || rec.Name != "Wool Throw" || rec.EventID != "e1" {
		t.Fatalf("not normalised: %+v", rec)
	}
}

func TestParseRecordWithdrawNeedsOnlyIdentityAndVersion(t *testing.T) {
	rec, err := ParseRecord([]byte(`{"sku":"A1","type":"withdraw","version":4}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Type != RecordWithdraw || rec.Version != 4 {
		t.Fatalf("got %+v", rec)
	}
}

func TestParseRecordInvalid(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string // substring that must appear in the problems
	}{
		{"not an object", `[1,2,3]`, "not a JSON object"},
		{"garbage", `{"sku":`, "not a JSON object"},
		{"missing sku", `{"type":"upsert","version":1,"name":"x","price_cents":1,"currency":"USD"}`, "sku is required"},
		{"bad sku characters", `{"sku":"a b/c","type":"upsert","version":1,"name":"x","price_cents":1,"currency":"USD"}`, "sku must be"},
		{"unknown type", `{"sku":"A","type":"delete","version":1}`, "type must be"},
		{"zero version", `{"sku":"A","type":"withdraw","version":0}`, "version must be"},
		{"missing version", `{"sku":"A","type":"withdraw"}`, "version must be"},
		{"fractional version", `{"sku":"A","type":"withdraw","version":1.5}`, "not a JSON object"},
		{"missing name", `{"sku":"A","type":"upsert","version":1,"price_cents":1,"currency":"USD"}`, "name is required"},
		{"blank name", `{"sku":"A","type":"upsert","version":1,"name":"   ","price_cents":1,"currency":"USD"}`, "name is required"},
		{"negative price", `{"sku":"A","type":"upsert","version":1,"name":"x","price_cents":-5,"currency":"USD"}`, "must not be negative"},
		{"missing price", `{"sku":"A","type":"upsert","version":1,"name":"x","currency":"USD"}`, "price_cents is required"},
		{"price as string", `{"sku":"A","type":"upsert","version":1,"name":"x","price_cents":"12.99","currency":"USD"}`, "not a JSON object"},
		{"bad currency", `{"sku":"A","type":"upsert","version":1,"name":"x","price_cents":1,"currency":"dollars"}`, "currency must be"},
		{"name too long", `{"sku":"A","type":"upsert","version":1,"name":"` + strings.Repeat("x", 201) + `","price_cents":1,"currency":"USD"}`, "name is too long"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseRecord([]byte(tc.raw))
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("want *ValidationError, got %v", err)
			}
			if !strings.Contains(ve.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", ve.Error(), tc.want)
			}
		})
	}
}

func TestParseRecordReportsAllProblemsAtOnce(t *testing.T) {
	_, err := ParseRecord([]byte(`{"type":"upsert","version":0,"price_cents":-1,"currency":"x"}`))
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want *ValidationError, got %v", err)
	}
	if len(ve.Problems) < 5 {
		t.Fatalf("expected every problem to be listed, got %v", ve.Problems)
	}
}

func TestNormalizeSKU(t *testing.T) {
	if got := NormalizeSKU("  hp-1011\t"); got != "HP-1011" {
		t.Fatalf("got %q", got)
	}
}
