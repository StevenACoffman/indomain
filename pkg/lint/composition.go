package lint

import (
	"fmt"
	"slices"
	"strings"

	"github.com/StevenACoffman/indomain/pkg/model"
)

// maxCycleNames bounds how many members a cycle message names, so a loop
// through a large generated model stays one readable line.
const maxCycleNames = 5

// ownershipCycle is one loop of entities that own one another.
type ownershipCycle struct {
	members   []string // sorted
	firstLink int      // index of members[0]'s owned relationship into the loop
}

// ownedLink is an owned relationship that requires an owner, by its position
// in the declaring entity's relationships.
type ownedLink struct {
	target string
	index  int
}

// tarjan holds the state of one run of Tarjan's strongly-connected-components
// algorithm.
type tarjan struct {
	edges      map[string][]string
	index      map[string]int
	low        map[string]int
	onStack    map[string]bool
	stack      []string
	components [][]string
}

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
			res.add(SeverityWarning, CategorySemantic,
				fmt.Sprintf("/entities/%s/relationships/%d/ownership", name, i),
				fmt.Sprintf(
					"%s→%s is owned, so each %s has exactly one %s, but cardinality %q does not say so — write the owner (left) side as \"1\", or use ownership: referenced if a %s can be shared or stand alone",
					name,
					rel.Entity,
					rel.Entity,
					name,
					rel.Cardinality,
					rel.Entity,
				))
		}
	}
}

func exactlyOne(m model.Multiplicity) bool {
	return m.Min == 1 && m.Max == 1
}

// runOwnershipCycles errors on entities that own one another in a loop where
// every link requires an owner. Each instance then needs an owner of the type
// before it, all the way around, so no instance can be the root and the
// entities in the loop can never exist.
func runOwnershipCycles(m *model.Model, res *Result) {
	for _, c := range ownershipCycles(m) {
		res.Findings = append(res.Findings, Finding{
			Severity: SeverityError,
			Category: CategorySemantic,
			Path: fmt.Sprintf(
				"/entities/%s/relationships/%d/ownership",
				c.members[0],
				c.firstLink,
			),
			Message: cycleMessage(c.members),
		})
	}
}

// ownershipCycles returns each loop of local entities that own one another
// through relationships that require an owner, ordered by first member.
//
// A link counts only when its owner side's minimum is at least one: an optional
// owner leaves room for a root, which runCompositionOwner already questions.
// Two entities that each own the other are left out: that is one relationship
// declared owned from both ends, which runReciprocity or runPairing already
// reports, and fixing it first may be what breaks the larger loop.
func ownershipCycles(m *model.Model) []ownershipCycle {
	links, edges := ownerLinks(m)

	var cycles []ownershipCycle
	for _, component := range stronglyConnected(m.EntityNames(), edges) {
		ownsItself := len(component) == 1 && slices.Contains(edges[component[0]], component[0])
		if len(component) == 1 && !ownsItself {
			continue
		}
		slices.Sort(component)
		// Every member of a loop has a link into it, so first is never -1.
		first := slices.IndexFunc(links[component[0]], func(l ownedLink) bool {
			return slices.Contains(component, l.target)
		})
		cycles = append(cycles, ownershipCycle{
			members:   component,
			firstLink: links[component[0]][first].index,
		})
	}
	slices.SortFunc(
		cycles,
		func(a, b ownershipCycle) int { return strings.Compare(a.members[0], b.members[0]) },
	)
	return cycles
}

// ownerLinks returns, per entity, the owned relationships that require an owner
// (with their positions) and the entities they point at, leaving out one that
// the target owns right back.
func ownerLinks(m *model.Model) (map[string][]ownedLink, map[string][]string) {
	owns := map[[2]string]bool{}
	for _, name := range m.EntityNames() {
		for _, rel := range m.Entities[name].Relationships {
			if _, local := m.Entities[rel.Entity]; local && requiresOwner(&rel) {
				owns[[2]string{name, rel.Entity}] = true
			}
		}
	}
	links := map[string][]ownedLink{}
	edges := map[string][]string{}
	for _, name := range m.EntityNames() {
		for i, rel := range m.Entities[name].Relationships {
			mutual := name != rel.Entity && owns[[2]string{rel.Entity, name}]
			if owns[[2]string{name, rel.Entity}] && !mutual {
				links[name] = append(links[name], ownedLink{target: rel.Entity, index: i})
				edges[name] = append(edges[name], rel.Entity)
			}
		}
	}
	return links, edges
}

// requiresOwner reports whether rel is owned and its owner side needs at least
// one owner.
func requiresOwner(rel *model.Relationship) bool {
	if rel.Ownership != "owned" {
		return false
	}
	owner, _, ok := model.ParseCardinality(rel.Cardinality)
	return ok && owner.Min >= 1
}

// stronglyConnected partitions nodes into strongly connected components using
// Tarjan's algorithm.
func stronglyConnected(nodes []string, edges map[string][]string) [][]string {
	t := &tarjan{
		edges:   edges,
		index:   map[string]int{},
		low:     map[string]int{},
		onStack: map[string]bool{},
	}
	for _, v := range nodes {
		if _, seen := t.index[v]; !seen {
			t.visit(v)
		}
	}
	return t.components
}

func (t *tarjan) visit(v string) {
	t.index[v] = len(t.index)
	t.low[v] = t.index[v]
	t.stack = append(t.stack, v)
	t.onStack[v] = true
	for _, w := range t.edges[v] {
		if _, seen := t.index[w]; !seen {
			t.visit(w)
			t.low[v] = min(t.low[v], t.low[w])
		} else if t.onStack[w] {
			t.low[v] = min(t.low[v], t.index[w])
		}
	}
	if t.low[v] == t.index[v] {
		t.components = append(t.components, t.popComponent(v))
	}
}

// popComponent pops the stack down to and including root, which is the
// component root leads.
func (t *tarjan) popComponent(root string) []string {
	var component []string
	for {
		w := t.stack[len(t.stack)-1]
		t.stack = t.stack[:len(t.stack)-1]
		t.onStack[w] = false
		component = append(component, w)
		if w == root {
			return component
		}
	}
}

func cycleMessage(cycle []string) string {
	if len(cycle) == 1 {
		return fmt.Sprintf(
			"%[1]s owns %[1]s and every %[1]s must have an owner, so no %[1]s can be the root and none can exist — use ownership: referenced",
			cycle[0],
		)
	}
	var names string
	if len(cycle) > maxCycleNames {
		names = fmt.Sprintf(
			"%s, and %d others",
			strings.Join(cycle[:maxCycleNames], ", "),
			len(cycle)-maxCycleNames,
		)
	} else {
		names = strings.Join(cycle[:len(cycle)-1], ", ") + ", and " + cycle[len(cycle)-1]
	}
	return fmt.Sprintf(
		"%s own one another in a cycle and each must have an owner, so none can be the root and none can exist — make one of these relationships ownership: referenced",
		names,
	)
}
