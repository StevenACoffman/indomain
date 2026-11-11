package lint

import (
	"fmt"
	"strings"

	"github.com/StevenACoffman/indomain/pkg/model"
)

// runInvariantCoverage flags an invariant that no scenario touches and no
// action preserves. The linter can check the references to a rule but never
// its meaning, so a rule nothing points at is one no part of the model claims
// to exercise or keep — the coverage gap, not a proof that the rule is wrong.
func runInvariantCoverage(m *model.Model, res *Result) {
	referenced := map[string]bool{}
	for _, sc := range m.Scenarios {
		for _, id := range sc.InvariantsTouched {
			referenced[strings.TrimSpace(id)] = true
		}
	}
	for _, ent := range m.Entities {
		for _, act := range ent.Actions {
			for _, id := range act.Preserves {
				referenced[id] = true
			}
		}
	}

	warnUnreferenced := func(path, id string) {
		if id == "" || referenced[id] {
			return
		}
		res.Findings = append(res.Findings, Finding{
			Severity: SeverityWarning,
			Category: CategoryCompleteness,
			Path:     path,
			Message: fmt.Sprintf(
				"no scenario touches and no action preserves invariant %q — list it in a scenario's invariants_touched or an action's preserves, or drop it if it only restates a cardinality",
				id,
			),
		})
	}
	for _, name := range m.EntityNames() {
		for i, inv := range m.Entities[name].Invariants {
			warnUnreferenced(fmt.Sprintf("/entities/%s/invariants/%d", name, i), inv.ID)
		}
	}
	for i, inv := range m.Invariants {
		warnUnreferenced(fmt.Sprintf("/invariants/%d", i), inv.ID)
	}
}
