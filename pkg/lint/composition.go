package lint

import (
	"fmt"

	"github.com/StevenACoffman/indomain/pkg/model"
)

// runCompositionOwner warns on an owned relationship whose owner side is not
// exactly one. `ownership: owned` says the target cannot exist without the
// declaring entity, so each target instance has exactly one owner: "n:1 owned"
// would have many owners share one part, and "0..1:n owned" would let a part
// exist with no owner at all. A warning, not an error, so a model that lints
// clean today keeps passing.
func runCompositionOwner(m *model.Model, res *Result) {
	for _, name := range m.EntityNames() {
		for i, rel := range m.Entities[name].Relationships {
			if rel.Ownership != "owned" {
				continue
			}
			// An unparseable cardinality has its own finding to fix first, and
			// its owner side can't be judged.
			owner, _, ok := model.ParseCardinality(rel.Cardinality)
			if !ok || exactlyOne(owner) {
				continue
			}
			res.Findings = append(res.Findings, Finding{
				Severity: SeverityWarning,
				Category: CategorySemantic,
				Path:     fmt.Sprintf("/entities/%s/relationships/%d/ownership", name, i),
				Message: fmt.Sprintf(
					"%s→%s is owned, so each %s has exactly one %s, but cardinality %q does not say so — write the owner (left) side as \"1\", or use ownership: referenced if a %s can be shared or stand alone",
					name, rel.Entity, rel.Entity, name, rel.Cardinality, rel.Entity,
				),
			})
		}
	}
}

func exactlyOne(m model.Multiplicity) bool {
	return m.Min == 1 && m.Max == 1
}

