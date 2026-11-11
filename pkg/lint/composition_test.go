package lint

import (
	"slices"
	"strconv"
	"strings"
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

// TestComposition_OwnershipCycle pins that entities owning one another in a
// loop of required owners raise one blocking error per loop, pointed at the
// first member's owned relationship into it, and that a loop with an optional
// owner, a referenced link, or only two members does not.
func TestComposition_OwnershipCycle(t *testing.T) {
	t.Parallel()
	const threeWay = "Alpha, Beta, and Gamma own one another in a cycle and each must have an owner, so none can be the root and none can exist — make one of these relationships ownership: referenced"
	cases := []struct {
		name     string
		entities string
		want     []Finding
	}{
		{
			name: "three entities own one another",
			entities: `
  Alpha:
    definition: An alpha.
    relationships:
      - {entity: Beta, cardinality: "1:n", ownership: owned}
  Beta:
    definition: A beta.
    relationships:
      - {entity: Gamma, cardinality: "1:n", ownership: owned}
  Gamma:
    definition: A gamma.
    relationships:
      - {entity: Alpha, cardinality: "1:n", ownership: owned}
`,
			want: []Finding{{
				Severity: SeverityError,
				Category: CategorySemantic,
				Path:     "/entities/Alpha/relationships/0/ownership",
				Message:  threeWay,
			}},
		},
		{
			name: "path skips relationships outside the cycle",
			entities: `
  Alpha:
    definition: An alpha.
    relationships:
      - {entity: Delta, cardinality: "1:n", ownership: owned}
      - {entity: Beta, cardinality: "1..n:n", ownership: owned}
  Beta:
    definition: A beta.
    relationships:
      - {entity: Gamma, cardinality: "1:n", ownership: owned}
  Gamma:
    definition: A gamma.
    relationships:
      - {entity: Alpha, cardinality: "1:n", ownership: owned}
  Delta:
    definition: A delta.
`,
			want: []Finding{{
				Severity: SeverityError,
				Category: CategorySemantic,
				Path:     "/entities/Alpha/relationships/1/ownership",
				Message:  threeWay,
			}},
		},
		{
			name: "an entity owns itself",
			entities: `
  Folder:
    definition: A folder.
    relationships:
      - {entity: Folder, cardinality: "1:n", ownership: owned}
`,
			want: []Finding{{
				Severity: SeverityError,
				Category: CategorySemantic,
				Path:     "/entities/Folder/relationships/0/ownership",
				Message:  "Folder owns Folder and every Folder must have an owner, so no Folder can be the root and none can exist — use ownership: referenced",
			}},
		},
		{
			name: "two separate cycles raise two findings",
			entities: `
  Folder:
    definition: A folder.
    relationships:
      - {entity: Folder, cardinality: "1:n", ownership: owned}
  Alpha:
    definition: An alpha.
    relationships:
      - {entity: Beta, cardinality: "1:n", ownership: owned}
  Beta:
    definition: A beta.
    relationships:
      - {entity: Gamma, cardinality: "1:n", ownership: owned}
  Gamma:
    definition: A gamma.
    relationships:
      - {entity: Alpha, cardinality: "1:n", ownership: owned}
`,
			want: []Finding{
				{
					Severity: SeverityError,
					Category: CategorySemantic,
					Path:     "/entities/Alpha/relationships/0/ownership",
					Message:  threeWay,
				},
				{
					Severity: SeverityError,
					Category: CategorySemantic,
					Path:     "/entities/Folder/relationships/0/ownership",
					Message:  "Folder owns Folder and every Folder must have an owner, so no Folder can be the root and none can exist — use ownership: referenced",
				},
			},
		},
		{
			name: "a loop made only of mutual pairs is left to the reciprocity check",
			entities: `
  Alpha:
    definition: An alpha.
    relationships:
      - {entity: Beta, cardinality: "1:1", ownership: owned}
  Beta:
    definition: A beta.
    relationships:
      - {entity: Alpha, cardinality: "1:1", ownership: owned}
      - {entity: Gamma, cardinality: "1:1", ownership: owned}
  Gamma:
    definition: A gamma.
    relationships:
      - {entity: Beta, cardinality: "1:1", ownership: owned}
`,
		},
		{
			name: "path skips a mutual link into the loop",
			entities: `
  Alpha:
    definition: An alpha.
    relationships:
      - {entity: Gamma, cardinality: "1:1", ownership: owned}
      - {entity: Beta, cardinality: "1:n", ownership: owned}
  Beta:
    definition: A beta.
    relationships:
      - {entity: Gamma, cardinality: "1:n", ownership: owned}
  Gamma:
    definition: A gamma.
    relationships:
      - {entity: Alpha, cardinality: "1:1", ownership: owned}
      - {entity: Delta, cardinality: "1:n", ownership: owned}
  Delta:
    definition: A delta.
    relationships:
      - {entity: Alpha, cardinality: "1:n", ownership: owned}
`,
			want: []Finding{{
				Severity: SeverityError,
				Category: CategorySemantic,
				Path:     "/entities/Alpha/relationships/1/ownership",
				Message:  "Alpha, Beta, Delta, and Gamma own one another in a cycle and each must have an owner, so none can be the root and none can exist — make one of these relationships ownership: referenced",
			}},
		},
		{
			name: "a long loop names its first five members",
			entities: `
  Ent1:
    definition: One.
    relationships:
      - {entity: Ent2, cardinality: "1:n", ownership: owned}
  Ent2:
    definition: Two.
    relationships:
      - {entity: Ent3, cardinality: "1:n", ownership: owned}
  Ent3:
    definition: Three.
    relationships:
      - {entity: Ent4, cardinality: "1:n", ownership: owned}
  Ent4:
    definition: Four.
    relationships:
      - {entity: Ent5, cardinality: "1:n", ownership: owned}
  Ent5:
    definition: Five.
    relationships:
      - {entity: Ent6, cardinality: "1:n", ownership: owned}
  Ent6:
    definition: Six.
    relationships:
      - {entity: Ent7, cardinality: "1:n", ownership: owned}
  Ent7:
    definition: Seven.
    relationships:
      - {entity: Ent1, cardinality: "1:n", ownership: owned}
`,
			want: []Finding{{
				Severity: SeverityError,
				Category: CategorySemantic,
				Path:     "/entities/Ent1/relationships/0/ownership",
				Message:  "Ent1, Ent2, Ent3, Ent4, Ent5, and 2 others own one another in a cycle and each must have an owner, so none can be the root and none can exist — make one of these relationships ownership: referenced",
			}},
		},
		{
			name: "an optional owner leaves room for a root",
			entities: `
  Folder:
    definition: A folder.
    relationships:
      - {entity: Folder, cardinality: "0..1:n", ownership: owned}
`,
		},
		{
			name: "a referenced link breaks the cycle",
			entities: `
  Alpha:
    definition: An alpha.
    relationships:
      - {entity: Beta, cardinality: "1:n", ownership: owned}
  Beta:
    definition: A beta.
    relationships:
      - {entity: Gamma, cardinality: "1:n", ownership: owned}
  Gamma:
    definition: A gamma.
    relationships:
      - {entity: Alpha, cardinality: "1:n", ownership: referenced}
`,
		},
		{
			name: "a two-entity loop is left to the reciprocity check",
			entities: `
  Alpha:
    definition: An alpha.
    relationships:
      - {entity: Beta, cardinality: "1:1", ownership: owned}
  Beta:
    definition: A beta.
    relationships:
      - {entity: Alpha, cardinality: "1:1", ownership: owned}
`,
		},
		{
			name: "an owned chain without a loop",
			entities: `
  Alpha:
    definition: An alpha.
    relationships:
      - {entity: Beta, cardinality: "1:n", ownership: owned}
  Beta:
    definition: A beta.
    relationships:
      - {entity: Gamma, cardinality: "1:n", ownership: owned}
  Gamma:
    definition: A gamma.
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			src := "kind: DomainModel\nversion: v1\nentities:" + tc.entities
			res, err := Run(testModelPath, []byte(src), nil)
			if err != nil {
				t.Fatal(err)
			}
			var got []Finding
			for _, f := range res.Findings {
				if strings.Contains(f.Message, "none can exist") {
					got = append(got, f)
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("cycle findings mismatch\ngot:  %+v\nwant: %+v", got, tc.want)
			}
			if len(tc.want) > 0 && !(&Result{Findings: got}).HasBlocking(false) {
				t.Error("an ownership cycle must block, even without --completeness error")
			}
		})
	}
}
