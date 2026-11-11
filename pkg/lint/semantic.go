package lint

import (
	"fmt"
	"strings"

	"github.com/StevenACoffman/indomain/pkg/model"
)

// semanticChecker holds the vocabularies the semantic layer resolves a
// reference against.
type semanticChecker struct {
	m   *model.Model
	res *Result

	entitySet map[string]bool
	// allowed maps a backtick token (entity name or its naive plural) to the
	// canonical entity it refers to.
	allowed map[string]string
	// glossary terms are defined non-entity vocabulary (roles, states, nouns).
	glossary map[string]bool
	// enums are referenceable types (an attribute names one in its `type`).
	enums map[string]bool
	// invariantID collects ids across both scopes — per-entity and model-level.
	// Ids share one namespace and must be unique so a reference
	// (invariants_touched, action.preserves) resolves unambiguously, regardless
	// of which scope declared the invariant.
	invariantID map[string]bool
	// knownNonEntity is the non-entity vocabulary backtick resolution accepts:
	// glossary terms, declared scenario actors (which may be ad-hoc
	// participants like `TargetUser` and are intentionally not required to be
	// glossary terms), and role terms (so they don't double-warn in freeform
	// text).
	knownNonEntity map[string]bool
}

func newSemanticChecker(m *model.Model, res *Result) *semanticChecker {
	c := &semanticChecker{
		m:              m,
		res:            res,
		entitySet:      map[string]bool{},
		allowed:        map[string]string{},
		glossary:       map[string]bool{},
		enums:          map[string]bool{},
		invariantID:    map[string]bool{},
		knownNonEntity: map[string]bool{},
	}
	for name := range m.Entities {
		c.entitySet[name] = true
		c.allowed[name] = name
		c.allowed[plural(name)] = name
	}
	for term := range m.Glossary {
		c.glossary[term] = true
	}
	for name := range m.Enums {
		c.enums[name] = true
	}
	return c
}

// runSemantic checks that the cross-references in a model hold: relationship
// targets, glossary and enum references, invariant ids, and backticked terms.
func runSemantic(m *model.Model, res *Result) {
	c := newSemanticChecker(m, res)
	c.collectInvariantIDs()
	c.checkRelationships()
	c.collectKnownNonEntity()
	c.checkBacktickRefs()
	c.checkScenarioInvariants()
}

func (c *semanticChecker) collectInvariantIDs() {
	for _, name := range c.m.EntityNames() {
		for i, inv := range c.m.Entities[name].Invariants {
			c.registerInvariant(inv.ID, fmt.Sprintf("/entities/%s/invariants/%d/id", name, i))
		}
	}
	for i, inv := range c.m.Invariants {
		c.registerInvariant(inv.ID, fmt.Sprintf("/invariants/%d/id", i))
	}
}

func (c *semanticChecker) registerInvariant(id, path string) {
	if id == "" {
		return // schema requires it; the structural layer reports absence
	}
	if c.invariantID[id] {
		c.res.add(SeverityError, CategorySemantic, path, fmt.Sprintf(
			"duplicate invariant id %q — ids must be unique so references resolve unambiguously",
			id,
		))
		return
	}
	c.invariantID[id] = true
}

// checkRelationships requires every relationship target to be a defined entity
// and every role to read as a role.
func (c *semanticChecker) checkRelationships() {
	for _, name := range c.m.EntityNames() {
		for i, rel := range c.m.Entities[name].Relationships {
			base := fmt.Sprintf("/entities/%s/relationships/%d", name, i)
			c.checkRelationshipTarget(name, base, &rel)
			c.checkRelationshipRole(base+"/role", &rel)
		}
	}
}

func (c *semanticChecker) checkRelationshipTarget(name, base string, rel *model.Relationship) {
	switch {
	case qualifiedRefRE.MatchString(rel.Entity):
		// Qualified targets are resolved against direct imports by runImports.
	case !c.entitySet[rel.Entity]:
		c.res.add(SeverityError, CategorySemantic, base+"/entity",
			fmt.Sprintf("relationship targets undefined entity %q", rel.Entity))
	case rel.Ownership == "owned" && c.m.Entities[rel.Entity].Derived:
		c.res.add(SeverityWarning, CategorySemantic, base+"/ownership", fmt.Sprintf(
			"entity %q owns %q, which is derived — composing an ephemeral, never-persisted entity is usually a modeling error",
			name,
			rel.Entity,
		))
	}
}

// checkRelationshipRole holds a prose role to one finding. A prose role wrecks
// the diagram: it is the only label on the rendered relationship line
// (ADR-0008), so a sentence there collides with its neighbours. `note` is the
// field for prose.
//
// Rewriting a prose role is the fix that comes first, and the terms buried in
// the sentence are likely to change with it, so the undefined-term check waits
// until the role is a role name.
func (c *semanticChecker) checkRelationshipRole(path string, rel *model.Relationship) {
	if readsAsProse(rel.Role) {
		c.res.add(SeverityWarning, CategorySemantic, path, fmt.Sprintf(
			"role %q reads as prose — keep the role to a short role name (ideally a glossary term) and move the explanation to the relationship's note",
			rel.Role,
		))
		return
	}
	// A role names a non-entity vocabulary term; it should resolve to an entity
	// or a glossary term (the DDD-1 payoff — undefined roles).
	for _, base := range entityRefs(rel.Role) {
		if c.allowed[base] != "" || c.glossary[base] {
			continue
		}
		c.res.add(SeverityWarning, CategorySemantic, path, fmt.Sprintf(
			"role term %q is not a defined entity or glossary term — define it in the glossary",
			base,
		))
	}
}

func (c *semanticChecker) collectKnownNonEntity() {
	for term := range c.glossary {
		c.knownNonEntity[term] = true
	}
	for _, sc := range c.m.Scenarios {
		for _, actor := range sc.Actors {
			c.knownNonEntity[strings.TrimSpace(actor)] = true
		}
	}
	for name := range c.m.Entities {
		for _, rel := range c.m.Entities[name].Relationships {
			for _, base := range entityRefs(rel.Role) {
				c.knownNonEntity[base] = true
			}
		}
	}
}

// checkBacktickRefs requires a backticked entity-looking term in freeform text
// to resolve to a defined entity, glossary term, declared role, or actor.
func (c *semanticChecker) checkBacktickRefs() {
	m := c.m
	for _, term := range sortedMapKeys(m.Glossary) {
		c.checkRefs(fmt.Sprintf("/glossary/%s", term), m.Glossary[term])
	}
	for _, ename := range sortedMapKeys(m.Enums) {
		en := m.Enums[ename]
		c.checkRefs(fmt.Sprintf("/enums/%s/description", ename), en.Description)
		for i, v := range en.Values {
			c.checkRefs(fmt.Sprintf("/enums/%s/values/%d/definition", ename, i), v.Definition)
		}
	}
	for _, name := range m.EntityNames() {
		c.checkEntityRefs(name)
	}
	for i, inv := range m.Invariants {
		c.checkRefs(fmt.Sprintf("/invariants/%d/statement", i), inv.Statement)
	}
	for i, sc := range m.Scenarios {
		for j, step := range sc.Steps {
			c.checkRefs(fmt.Sprintf("/scenarios/%d/steps/%d", i, j), step)
		}
	}
}

func (c *semanticChecker) checkRefs(path, text string) {
	for _, base := range entityRefs(text) {
		if c.allowed[base] != "" || c.knownNonEntity[base] {
			continue
		}
		c.res.add(SeverityWarning, CategorySemantic, path, fmt.Sprintf(
			"backticked term %q is not a defined entity, glossary term, role, or actor",
			base,
		))
	}
}

func (c *semanticChecker) checkEntityRefs(name string) {
	ent := c.m.Entities[name]
	base := fmt.Sprintf("/entities/%s", name)
	c.checkRefs(base+"/definition", ent.Definition)
	c.checkRefs(base+"/derivation", ent.Derivation)
	for i, rel := range ent.Relationships {
		c.checkRefs(fmt.Sprintf("%s/relationships/%d/role", base, i), rel.Role)
		c.checkRefs(fmt.Sprintf("%s/relationships/%d/note", base, i), rel.Note)
	}
	for i, attr := range ent.Attributes {
		c.checkAttribute(fmt.Sprintf("%s/attributes/%d", base, i), &attr)
	}
	for i, act := range ent.Actions {
		c.checkAction(fmt.Sprintf("%s/actions/%d", base, i), &act)
	}
	for i, inv := range ent.Invariants {
		c.checkRefs(fmt.Sprintf("%s/invariants/%d/statement", base, i), inv.Statement)
	}
}

func (c *semanticChecker) checkAttribute(base string, attr *model.Attribute) {
	// A PascalCase type is an enum reference; it must resolve.
	if pascalCaseRE.MatchString(attr.Type) && !c.enums[attr.Type] {
		c.res.add(SeverityWarning, CategorySemantic, base+"/type", fmt.Sprintf(
			"attribute type %q looks like an enum reference but no enum %q is defined (primitives are lowercase)",
			attr.Type,
			attr.Type,
		))
	}
	c.checkRefs(base+"/derivation", attr.Derivation)
}

func (c *semanticChecker) checkAction(base string, act *model.Action) {
	if act.Actor != "" && !c.entitySet[act.Actor] && !c.glossary[act.Actor] {
		c.res.add(SeverityWarning, CategorySemantic, base+"/actor", fmt.Sprintf(
			"action actor %q is not a defined entity or glossary term",
			act.Actor,
		))
	}
	for j, id := range act.Preserves {
		if !c.invariantID[id] {
			c.res.add(SeverityError, CategorySemantic, fmt.Sprintf("%s/preserves/%d", base, j),
				fmt.Sprintf("action preserves unknown invariant id %q", id))
		}
	}
	c.checkRefs(base+"/description", act.Description)
}

// checkScenarioInvariants requires a scenario's invariants_touched to reference
// a declared invariant id. With stable ids (DDD-9) this is a real reference, so
// a miss is a broken reference (error), not the soft "may be a gap" signal it
// was as free text.
func (c *semanticChecker) checkScenarioInvariants() {
	for i, sc := range c.m.Scenarios {
		for j, id := range sc.InvariantsTouched {
			if c.invariantID[strings.TrimSpace(id)] {
				continue
			}
			c.res.add(SeverityError, CategorySemantic,
				fmt.Sprintf("/scenarios/%d/invariants_touched/%d", i, j),
				fmt.Sprintf("scenario %q touches unknown invariant id %q", sc.Name, id))
		}
	}
}
