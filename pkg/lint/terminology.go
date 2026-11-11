package lint

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/StevenACoffman/indomain/pkg/model"
)

// runTermCollisions warns when one entity or glossary name ends with another,
// as TreatmentSession ends with Session. English compounds put the head noun
// last and get shortened by dropping the modifier, so people say "session" for
// a TreatmentSession and the two concepts blur. A suffix is the collision that
// shortening produces; a shared prefix is not.
//
// Two cases mean the author already settled the ambiguity, and do not warn: the
// longer name is a subtype of the shorter one (a CreditCard is a Card), or its
// definition backticks the shorter name, recording how the two differ.
func runTermCollisions(m *model.Model, res *Result) {
	names := vocabulary(m)
	for _, long := range names {
		for _, short := range names {
			if long == short || !strings.HasSuffix(long, short) {
				continue
			}
			if slices.Contains(ancestors(m, long), short) || mentions(termDefinition(m, long), short) {
				continue
			}
			res.Findings = append(res.Findings, Finding{
				Severity: SeverityWarning,
				Category: CategorySemantic,
				Path:     termPath(m, long),
				Message: fmt.Sprintf(
					"%q ends with %q, which names a different concept, so shortening one to the other confuses them — rename one, or say how they differ by backticking `%s` in %s's definition",
					long, short, short, long,
				),
			})
		}
	}
}

// vocabulary returns the defined entity and glossary names, sorted, each once.
func vocabulary(m *model.Model) []string {
	names := append(m.EntityNames(), sortedMapKeys(m.Glossary)...)
	sort.Strings(names)
	return slices.Compact(names)
}

// termDefinition returns name's definition, preferring the entity when a name
// is both an entity and a glossary term.
func termDefinition(m *model.Model, name string) string {
	if e, ok := m.Entities[name]; ok {
		return e.Definition
	}
	return m.Glossary[name]
}

func termPath(m *model.Model, name string) string {
	if _, ok := m.Entities[name]; ok {
		return "/entities/" + name
	}
	return "/glossary/" + name
}

// mentions reports whether text backticks term, singular or plural.
func mentions(text, term string) bool {
	for _, ref := range entityRefs(text) {
		if ref == term || ref == plural(term) {
			return true
		}
	}
	return false
}
