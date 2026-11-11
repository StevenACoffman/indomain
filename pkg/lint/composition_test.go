package lint

import (
	"strconv"
	"testing"
)

// TestComposition_OwnerSideMustBeOne pins that an owned relationship warns
// unless its owner side is exactly one, and that the warning never blocks —
// not even under strict completeness — so models that lint clean today still
// pass.
func TestComposition_OwnerSideMustBeOne(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		cardinality string
		ownership   string
		want        bool
	}{
		{name: "one owner, many parts", cardinality: "1:n", ownership: "owned", want: false},
		{name: "one owner, one part", cardinality: "1:1", ownership: "owned", want: false},
		{name: "one owner, bounded parts", cardinality: "1:0..5", ownership: "owned", want: false},
		{name: "many owners share a part", cardinality: "n:1", ownership: "owned", want: true},
		{name: "many to many", cardinality: "n:n", ownership: "owned", want: true},
		{name: "optional owner lets a part stand alone", cardinality: "0..1:n", ownership: "owned", want: true},
		{name: "exact count above one", cardinality: "2:n", ownership: "owned", want: true},
		{name: "referenced is unconstrained", cardinality: "n:1", ownership: "referenced", want: false},
		{name: "omitted ownership means referenced", cardinality: "n:1", ownership: "", want: false},
		{name: "inverted range is reported by its own check", cardinality: "5..2:n", ownership: "owned", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ownership := ""
			if tc.ownership != "" {
				ownership = "\n        ownership: " + tc.ownership
			}
			src := `
kind: DomainModel
version: v1
entities:
  Order:
    definition: A purchase.
    relationships:
      - entity: LineItem
        cardinality: ` + strconv.Quote(tc.cardinality) + ownership + `
  LineItem:
    definition: One line of an order.
`
			res, err := Run(testModelPath, []byte(src), nil)
			if err != nil {
				t.Fatal(err)
			}
			var got []Finding
			for _, f := range res.Findings {
				if f.Path == "/entities/Order/relationships/0/ownership" {
					got = append(got, f)
				}
			}
			if !tc.want {
				if len(got) != 0 {
					t.Fatalf("cardinality %q: expected no ownership finding, got %+v", tc.cardinality, got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("cardinality %q: expected exactly one ownership finding, got %+v", tc.cardinality, got)
			}
			want := Finding{
				Severity: SeverityWarning,
				Category: CategorySemantic,
				Path:     "/entities/Order/relationships/0/ownership",
				Message: `Order→LineItem is owned, so each LineItem has exactly one Order, but cardinality ` +
					strconv.Quote(tc.cardinality) +
					` does not say so — write the owner (left) side as "1", or use ownership: referenced if a LineItem can be shared or stand alone`,
			}
			if got[0] != want {
				t.Fatalf("finding mismatch\ngot:  %+v\nwant: %+v", got[0], want)
			}
			if (&Result{Findings: got}).HasBlocking(true) {
				t.Error("the owner-side warning must not block, even under --completeness error")
			}
		})
	}
}

