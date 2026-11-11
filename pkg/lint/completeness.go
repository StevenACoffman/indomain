package lint

import (
	"fmt"
	"strings"

	"github.com/StevenACoffman/indomain/pkg/model"
)

// vocabUsage records which vocabulary a model's text actually references.
type vocabUsage struct {
	terms map[string]bool
	enums map[string]bool
}

// runCompleteness reports advisory gaps: entities with no invariants, entities
// no scenario exercises, and vocabulary nothing references.
func runCompleteness(m *model.Model, res *Result) {
	checkEntityInvariants(m, res)
	checkEntitiesExercised(m, res)

	// A shared model is imported by others, and an incoming reference is
	// invisible from here: the vocabulary such a model exists to publish would
	// otherwise read as vocabulary nothing uses, and a pure vocabulary model
	// could never pass --completeness error. Only the unused-vocabulary checks
	// are relaxed; a missing invariant or an unexercised entity is a gap in the
	// model itself, which being imported does not fill.
	if m.Shared {
		return
	}
	checkUnusedVocabulary(m, res)
}

// checkEntityInvariants flags entities with no invariants — unless a
// supertype's invariants cover them.
func checkEntityInvariants(m *model.Model, res *Result) {
	for _, name := range m.EntityNames() {
		if len(m.Entities[name].Invariants) == 0 && !inheritsInvariants(m, name) {
			res.add(SeverityWarning, CategoryCompleteness, fmt.Sprintf("/entities/%s", name),
				fmt.Sprintf(
					"entity %q has no invariants — fine if no rule must always hold for it, otherwise the rules that govern it are worth capturing",
					name,
				))
		}
	}
}

// checkEntitiesExercised flags entities no scenario exercises.
func checkEntitiesExercised(m *model.Model, res *Result) {
	referenced := scenarioEntities(m)
	for _, name := range m.EntityNames() {
		if !referenced[name] {
			res.add(SeverityWarning, CategoryCompleteness, fmt.Sprintf("/entities/%s", name),
				fmt.Sprintf("no scenario exercises entity %q", name))
		}
	}
}

// scenarioEntities returns the entities named, as actors or in a step, by some
// scenario.
func scenarioEntities(m *model.Model) map[string]bool {
	referenced := map[string]bool{}
	canonical := map[string]string{}
	for name := range m.Entities {
		canonical[name] = name
		canonical[plural(name)] = name
	}
	mark := func(token string) {
		if c, ok := canonical[token]; ok {
			referenced[c] = true
		}
	}
	for _, sc := range m.Scenarios {
		for _, actor := range sc.Actors {
			mark(strings.TrimSpace(actor))
		}
		for _, step := range sc.Steps {
			for _, base := range entityRefs(step) {
				mark(base)
			}
		}
	}
	return referenced
}

// checkUnusedVocabulary flags defined-but-unused glossary terms and enums —
// vocabulary nothing references.
func checkUnusedVocabulary(m *model.Model, res *Result) {
	use := collectVocabUsage(m)
	for _, term := range sortedMapKeys(m.Glossary) {
		if !use.terms[term] {
			res.add(SeverityWarning, CategoryCompleteness, fmt.Sprintf("/glossary/%s", term),
				fmt.Sprintf("glossary term %q is defined but never referenced", term))
		}
	}
	for _, name := range sortedMapKeys(m.Enums) {
		if !use.enums[name] {
			res.add(SeverityWarning, CategoryCompleteness, fmt.Sprintf("/enums/%s", name),
				fmt.Sprintf("enum %q is defined but no attribute uses it", name))
		}
	}
}

func collectVocabUsage(m *model.Model) *vocabUsage {
	u := &vocabUsage{terms: map[string]bool{}, enums: map[string]bool{}}
	for _, def := range m.Glossary {
		u.scan(def)
	}
	for _, en := range m.Enums {
		u.scan(en.Description)
		for _, v := range en.Values {
			u.scan(v.Definition)
		}
	}
	for name := range m.Entities {
		ent := m.Entities[name]
		u.scanEntity(&ent)
	}
	for _, inv := range m.Invariants {
		u.scan(inv.Statement)
	}
	for _, sc := range m.Scenarios {
		for _, actor := range sc.Actors {
			u.terms[strings.TrimSpace(actor)] = true
		}
		for _, step := range sc.Steps {
			u.scan(step)
		}
	}
	return u
}

func (u *vocabUsage) scan(text string) {
	for _, b := range entityRefs(text) {
		u.terms[b] = true
	}
}

func (u *vocabUsage) scanEntity(ent *model.Entity) {
	u.scan(ent.Definition)
	u.scan(ent.Derivation)
	for _, rel := range ent.Relationships {
		u.scan(rel.Role)
		u.scan(rel.Note)
	}
	for _, attr := range ent.Attributes {
		u.scan(attr.Derivation)
		if attr.Type != "" {
			u.enums[attr.Type] = true
		}
	}
	for _, act := range ent.Actions {
		if actor := strings.TrimSpace(act.Actor); actor != "" {
			u.terms[actor] = true
		}
		u.scan(act.Description)
	}
	for _, inv := range ent.Invariants {
		u.scan(inv.Statement)
	}
}
