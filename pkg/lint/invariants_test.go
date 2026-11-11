package lint

import (
	"slices"
	"strings"
	"testing"
)

// TestInvariantCoverage_Unreferenced pins that an invariant no scenario
// touches and no action preserves raises one completeness warning at the
// invariant, at either scope, and that it blocks only under strict
// completeness.
func TestInvariantCoverage_Unreferenced(t *testing.T) {
	t.Parallel()
	const entities = `
entities:
  Project:
    definition: A container.
    actions:
      - name: archive
        preserves: [preserved-rule]
    invariants:
      - id: preserved-rule
        statement: "Stays consistent when archived"
      - id: orphan-rule
        statement: "Nothing refers to this"
`
	message := func(id string) string {
		return `no scenario touches and no action preserves invariant "` + id +
			`" — list it in a scenario's invariants_touched or an action's preserves, or drop it if it only restates a cardinality`
	}
	cases := []struct {
		name string
		rest string
		want []Finding
	}{
		{
			name: "only the invariant nothing references warns",
			rest: "",
			want: []Finding{{
				Severity: SeverityWarning,
				Category: CategoryCompleteness,
				Path:     "/entities/Project/invariants/1",
				Message:  message("orphan-rule"),
			}},
		},
		{
			name: "a scenario's invariants_touched covers its invariant",
			rest: `
scenarios:
  - name: Touch the orphan
    steps: ["A project changes"]
    invariants_touched: [" orphan-rule "]
`,
		},
		{
			name: "an unreferenced model-level invariant",
			rest: `
scenarios:
  - name: Touch the orphan
    steps: ["A project changes"]
    invariants_touched: [orphan-rule]
invariants:
  - id: cross-entity-rule
    statement: "Holds across every project"
`,
			want: []Finding{{
				Severity: SeverityWarning,
				Category: CategoryCompleteness,
				Path:     "/invariants/0",
				Message:  message("cross-entity-rule"),
			}},
		},
		{
			name: "a referenced model-level invariant",
			rest: `
scenarios:
  - name: Touch both
    steps: ["A project changes"]
    invariants_touched: [orphan-rule, cross-entity-rule]
invariants:
  - id: cross-entity-rule
    statement: "Holds across every project"
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			src := "kind: DomainModel\nversion: v1\n" + entities + tc.rest
			res, err := Run(testModelPath, []byte(src), nil)
			if err != nil {
				t.Fatal(err)
			}
			var got []Finding
			for _, f := range res.Findings {
				if strings.HasPrefix(f.Message, "no scenario touches and no action preserves") {
					got = append(got, f)
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("coverage findings mismatch\ngot:  %+v\nwant: %+v", got, tc.want)
			}
			if len(got) == 0 {
				return
			}
			only := &Result{Findings: got}
			if only.HasBlocking(false) {
				t.Error("an unreferenced invariant must not block under --completeness warn")
			}
			if !only.HasBlocking(true) {
				t.Error("an unreferenced invariant must block under --completeness error")
			}
		})
	}
}
