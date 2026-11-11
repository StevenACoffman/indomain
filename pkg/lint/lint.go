// Package lint validates a domain model in three layers:
//
//   - Structural: the file conforms to the JSON Schema (types, required
//     fields, enums). These are hard errors.
//   - Semantic: cross-references hold — relationship targets and backticked
//     entity names resolve to defined entities, and scenario invariants match
//     declared ones. Broken references are errors; unresolved freeform terms
//     are advisory warnings.
//   - Completeness: advisory gaps — entities with no invariants, entities no
//     scenario exercises. These surface gaps without demanding perfection.
package lint

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
	"sigs.k8s.io/yaml"

	"github.com/StevenACoffman/indomain/pkg/model"
	"github.com/StevenACoffman/indomain/pkg/schema"
)

// Severity levels a finding carries.
const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Categories name the layer that produced a finding.
const (
	CategoryStructural   Category = "structural"
	CategorySemantic     Category = "semantic"
	CategoryCompleteness Category = "completeness"
)

// roleLabelMax is the longest a role may be before it crowds the diagram line
// it labels, whatever its shape. Measured in runes, so a non-ASCII role is not
// judged by its byte count.
const roleLabelMax = 40

var (
	backtickRE   = regexp.MustCompile("`([^`]+)`")
	pascalCaseRE = regexp.MustCompile(`^[A-Z][A-Za-z0-9]*$`)
)

// reciprocalDecl is one relationship declaration, with the paths a finding
// about it points at.
type reciprocalDecl struct {
	from, to  string
	card      string
	owned     bool
	path      string
	ownerPath string
}

// Severity classifies how serious a finding is.
type Severity string

// Category groups findings by the layer that produced them.
type Category string

// Finding is a single lint result.
type Finding struct {
	Severity Severity `json:"severity"`
	Category Category `json:"category"`
	Path     string   `json:"path"`
	Message  string   `json:"message"`
}

// Result is the collected output of a lint run.
type Result struct {
	Findings []Finding `json:"findings"`
}

// HasBlocking reports whether the result should fail a build. Errors always
// block; completeness findings block only when completenessAsError is set.
func (r *Result) HasBlocking(completenessAsError bool) bool {
	for _, f := range r.Findings {
		if f.Severity == SeverityError {
			return true
		}
		if completenessAsError && f.Category == CategoryCompleteness {
			return true
		}
	}
	return false
}

func (r *Result) add(sev Severity, cat Category, path, msg string) {
	r.Findings = append(r.Findings, Finding{Severity: sev, Category: cat, Path: path, Message: msg})
}

// Run validates the model at path and returns all findings. src is the model's
// own bytes; files reads the ones its `imports:` name, resolved relative to
// path's directory, and answers where that resolution is confined. A nil files
// uses the local filesystem.
func Run(path string, src []byte, files Files) (*Result, error) {
	res := &Result{}
	if files == nil {
		files = OSFiles{}
	}

	// Whether this file is somebody else's copy is settled first: it decides
	// which of the layers below are findings about a model this repository
	// controls, and it holds even when the file does not parse.
	vendored := runProvenance(path, src, res)

	// Layer 1: structural validation against the JSON Schema.
	structuralOK := runStructural(src, res)

	// If it does not even parse into our typed model, stop — semantic and
	// completeness checks need a model to work with. The structural layer has
	// already reported why.
	m, err := model.Parse(src)
	if err != nil {
		if structuralOK {
			// Schema passed but typed parsing failed; surface it so we never
			// silently skip the later layers.
			res.Findings = append(res.Findings, Finding{
				Severity: SeverityError,
				Category: CategoryStructural,
				Path:     "",
				Message:  err.Error(),
			})
		}
		if vendored {
			dropOwnedDiagnostics(res)
		}
		sortFindings(res)
		return res, nil
	}

	runSemantic(m, res)
	// Imports resolve only against a document the schema accepted. A scope the
	// schema already rejected would otherwise bind anyway, and the advice that
	// follows would tell the author to write syntax that cannot work — the same
	// reason the version check gates schema validation.
	if structuralOK {
		runImports(path, m, files, res, vendored)
	}
	runRelationshipShape(m, res)
	runCompositionOwner(m, res)
	runOwnershipCycles(m, res)
	runTermCollisions(m, res)
	runSubtypes(m, res)
	runReciprocity(m, res)
	runPairing(m, res)
	runCompleteness(m, res)
	runInvariantCoverage(m, res)

	if vendored {
		dropOwnedDiagnostics(res)
	}
	sortFindings(res)
	return res, nil
}

// Structural runs only the JSON Schema (structural) layer and returns its
// findings — empty if the document is structurally valid. `indomain render` uses it
// to fail with a friendly schema error instead of the raw strict-YAML parse
// error, without running the semantic/completeness layers (which shouldn't
// block rendering).
func Structural(data []byte) []Finding {
	res := &Result{}
	runStructural(data, res)
	sortFindings(res)
	return res.Findings
}

// runStructural validates against the JSON Schema and returns whether it accepted
// the document.
func runStructural(data []byte, res *Result) bool {
	jsonBytes, err := yaml.YAMLToJSON(data)
	if err != nil {
		res.add(SeverityError, CategoryStructural, "", fmt.Sprintf("not valid YAML: %v", err))
		return false
	}

	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(jsonBytes))
	if err != nil {
		res.add(
			SeverityError,
			CategoryStructural,
			"",
			fmt.Sprintf("could not decode document: %v", err),
		)
		return false
	}

	version, ok := declaredVersion(inst, res)
	if !ok {
		return false
	}

	sch, err := schema.CompileVersion(version)
	if err != nil {
		res.add(SeverityError, CategoryStructural, "", fmt.Sprintf("pkg: %v", err))
		return false
	}
	return validateInstance(sch, inst, res)
}

// declaredVersion dispatches on the declared format version. indomain — not the
// schema — is the source of truth for which versions this build understands: if
// the file targets a version we don't have, say so plainly (so a newer file
// gets "upgrade indomain", not a cryptic schema mismatch) and validate against
// that version's schema. A missing/empty version falls through to the schema,
// which requires it and reports the absence in the usual way.
func declaredVersion(inst any, res *Result) (string, bool) {
	obj, ok := inst.(map[string]any)
	if !ok {
		return schema.Current, true
	}
	v, ok := obj["version"].(string)
	if !ok || v == "" {
		return schema.Current, true
	}
	if !schema.Supported(v) {
		res.add(SeverityError, CategoryStructural, "/version", fmt.Sprintf(
			"unsupported schema version %q; this indomain supports: %s "+
				"(upgrade indomain, or set a supported version)",
			v,
			strings.Join(schema.SupportedVersions(), ", "),
		))
		return "", false
	}
	return v, true
}

// validateInstance reports the schema's findings and whether it accepted inst.
//
// It says what a cross-model entity reference actually is before the schema
// reports it as a pattern violation, and suppresses its opaque message so one
// mistake reads as one finding. It is reported on its own terms and is not
// counted against the document's structural validity: a broken import in the
// same file is an unrelated mistake, and holding the imports layer back
// until this one is fixed would hide it.
func validateInstance(sch *jsonschema.Schema, inst any, res *Result) bool {
	before := len(res.Findings)
	err := sch.Validate(inst)
	if err == nil {
		return true
	}
	var ve *jsonschema.ValidationError
	if errors.As(err, &ve) {
		collectLeaves(ve, res, message.NewPrinter(language.English))
		return len(res.Findings) == before
	}
	res.add(SeverityError, CategoryStructural, "", err.Error())
	return false
}

func collectLeaves(e *jsonschema.ValidationError, res *Result, printer *message.Printer) {
	if len(e.Causes) == 0 {
		ptr := "/" + strings.Join(e.InstanceLocation, "/")
		if ptr == "/" {
			ptr = ""
		}
		msg := e.Error()
		if e.ErrorKind != nil {
			msg = e.ErrorKind.LocalizedString(printer)
		}
		res.add(SeverityError, CategoryStructural, ptr, msg)
		return
	}
	for _, c := range e.Causes {
		collectLeaves(c, res, printer)
	}
}

// runRelationshipShape checks each relationship's cardinality and symmetric
// marker beyond what the schema pattern can express. The schema pattern accepts
// an inverted range like "5..2"; here it becomes a semantic error. A symmetric
// marker is only meaningful when the two ends are interchangeable, so it is
// restricted to a self-referential relationship or one whose target side is
// more than one.
func runRelationshipShape(m *model.Model, res *Result) {
	for _, name := range m.EntityNames() {
		for i, rel := range m.Entities[name].Relationships {
			checkRelationshipShape(
				res,
				name,
				fmt.Sprintf("/entities/%s/relationships/%d", name, i),
				&rel,
			)
		}
	}
}

func checkRelationshipShape(res *Result, name, path string, rel *model.Relationship) {
	// The schema pattern accepts an inverted range like "5..2"
	// syntactically; flag only that semantic case here. Other parse
	// failures (a malformed string, an absurd overflow) are the
	// schema's to report, so this check doesn't double up on them.
	if hasInvertedRange(rel.Cardinality) {
		res.add(SeverityError, CategorySemantic, path+"/cardinality", fmt.Sprintf(
			"invalid cardinality %q: a range's minimum must not exceed its maximum",
			rel.Cardinality,
		))
	}

	// A symmetric marker is only meaningful when the two ends are
	// interchangeable. Skip the check when the cardinality doesn't parse
	// — the invalid cardinality is the finding to fix first, and the
	// target side can't be judged.
	_, right, ok := model.ParseCardinality(rel.Cardinality)
	if !rel.Symmetric || !ok {
		return
	}
	selfReferential := rel.Entity == name
	targetIsMany := right.Max < 0 || right.Max > 1
	if selfReferential || targetIsMany {
		return
	}
	res.add(SeverityError, CategorySemantic, path+"/symmetric", fmt.Sprintf(
		"symmetric relationship %s→%s must be self-referential or have a target side greater than one",
		name,
		rel.Entity,
	))
}

// hasInvertedRange reports whether either side of a cardinality is a range whose
// minimum exceeds its maximum (e.g. "5..2"). This is the one semantic error the
// schema's syntactic pattern cannot catch.
func hasInvertedRange(card string) bool {
	a, b, ok := strings.Cut(card, ":")
	if !ok {
		return false
	}
	return sideInverted(a) || sideInverted(b)
}

func sideInverted(s string) bool {
	lo, hi, isRange := strings.Cut(s, "..")
	if !isRange || hi == "n" {
		return false
	}
	loN, err1 := strconv.Atoi(lo)
	hiN, err2 := strconv.Atoi(hi)
	return err1 == nil && err2 == nil && loN > hiN
}

// runSubtypes validates the is-a links: a subtypeOf must name a defined entity,
// and the chain of subtypeOf links must not cycle (an entity cannot be a kind
// of itself, directly or transitively).
func runSubtypes(m *model.Model, res *Result) {
	for _, name := range m.EntityNames() {
		parent := m.Entities[name].SubtypeOf
		if parent == "" {
			continue
		}
		if qualifiedRefRE.MatchString(parent) {
			continue // resolved against a direct import; imported ancestry stops here
		}
		if _, ok := m.Entities[parent]; !ok {
			res.Findings = append(res.Findings, Finding{
				Severity: SeverityError,
				Category: CategorySemantic,
				Path:     fmt.Sprintf("/entities/%s/subtypeOf", name),
				Message: fmt.Sprintf(
					"entity %q is a subtype of undefined entity %q",
					name,
					parent,
				),
			})
			continue
		}
		if subtypeChainCycles(m, name) {
			res.Findings = append(res.Findings, Finding{
				Severity: SeverityError,
				Category: CategorySemantic,
				Path:     fmt.Sprintf("/entities/%s/subtypeOf", name),
				Message: fmt.Sprintf(
					"entity %q has a cyclic subtypeOf chain: an entity cannot be a kind of itself, directly or transitively",
					name,
				),
			})
		}
	}
}

// subtypeChainCycles reports whether following name's subtypeOf links revisits
// an entity, which means name is (transitively) a subtype of itself. It stops
// at an undefined parent, which is reported separately.
func subtypeChainCycles(m *model.Model, name string) bool {
	seen := map[string]bool{}
	for cur := name; cur != ""; {
		e, ok := m.Entities[cur]
		if !ok {
			return false
		}
		if seen[cur] {
			return true
		}
		seen[cur] = true
		cur = e.SubtypeOf
	}
	return false
}

// inheritsInvariants reports whether any ancestor of name (via subtypeOf) has an
// invariant, so a subtype's own empty invariant list is covered by its parent.
func inheritsInvariants(m *model.Model, name string) bool {
	return slices.ContainsFunc(ancestors(m, name), func(a string) bool {
		return len(m.Entities[a].Invariants) > 0
	})
}

// ancestors returns name's local supertypes, nearest first, following
// subtypeOf. The walk is cycle-safe and stops at an undefined or imported
// parent, so every name returned is a defined local entity.
func ancestors(m *model.Model, name string) []string {
	var out []string
	seen := map[string]bool{name: true}
	for cur := m.Entities[name].SubtypeOf; cur != "" && !seen[cur]; {
		e, ok := m.Entities[cur]
		if !ok {
			break
		}
		out = append(out, cur)
		seen[cur] = true
		cur = e.SubtypeOf
	}
	return out
}

// runReciprocity checks that a relationship declared from both sides agrees, on
// cardinality and on ownership.
//
// B→A must declare the inverse of A→B's cardinality. A contradiction (e.g. A
// says "1:n" B while B says "1:1" A) is an error — the model can't be both, and
// the renderer would otherwise draw two conflicting edges.
//
// Ownership belongs to the relationship, not to the end that declared it, so at
// most one end may claim `owned`; both claiming it is a contradiction. One end
// claiming it is the ordinary composition pattern — the renderer folds the two
// declarations into a single identifying line (ADR-0008), whatever roles the
// two ends give it.
//
// Only pairs with exactly one declaration in each direction are checked.
// Multiple edges between the same pair (a legitimate pattern — e.g. a User is
// both `Owner` and `Member` of a Project) can't be paired up unambiguously, so
// they're left alone rather than guessed at.
func runReciprocity(m *model.Model, res *Result) {
	byPair := reciprocalDecls(m)
	for _, k := range sortedMapKeys(byPair) {
		fwd, rev := splitByDirection(byPair[k])
		if len(fwd) != 1 || len(rev) != 1 {
			continue
		}
		checkReciprocalPair(res, &fwd[0], &rev[0])
	}
}

// reciprocalDecls groups the local relationship declarations by unordered
// entity pair.
func reciprocalDecls(m *model.Model) map[string][]reciprocalDecl {
	byPair := map[string][]reciprocalDecl{}
	for _, name := range m.EntityNames() {
		for i, rel := range m.Entities[name].Relationships {
			if qualifiedRefRE.MatchString(rel.Entity) {
				continue
			}
			pair := []string{name, rel.Entity}
			sort.Strings(pair)
			k := pair[0] + "\x00" + pair[1]
			byPair[k] = append(byPair[k], reciprocalDecl{
				from:      name,
				to:        rel.Entity,
				card:      rel.Cardinality,
				owned:     rel.Ownership == "owned",
				path:      fmt.Sprintf("/entities/%s/relationships/%d/cardinality", name, i),
				ownerPath: fmt.Sprintf("/entities/%s/relationships/%d/ownership", name, i),
			})
		}
	}
	return byPair
}

// splitByDirection separates a pair's declarations by the end that made them:
// fwd from the alphabetically earlier entity, rev from the later one.
func splitByDirection(decls []reciprocalDecl) (fwd, rev []reciprocalDecl) {
	for _, d := range decls {
		switch {
		case d.from == d.to:
			// Self-relationship; no reciprocal to reconcile.
		case d.from < d.to:
			fwd = append(fwd, d)
		default:
			rev = append(rev, d)
		}
	}
	return fwd, rev
}

func checkReciprocalPair(res *Result, f, r *reciprocalDecl) {
	// Mutual ownership is a contradiction whatever the cardinalities say,
	// so it is checked before they are parsed.
	if f.owned && r.owned {
		res.add(SeverityError, CategorySemantic, f.ownerPath, fmt.Sprintf(
			"mutual ownership: %s→%s and %s→%s both declare ownership \"owned\" — a relationship is owned by at most one end, so make the other end \"referenced\" (or omit it)",
			f.from,
			f.to,
			r.from,
			r.to,
		))
	}

	// Reciprocity is checked only for structurally valid cardinalities; an
	// invalid one is already reported (by the schema, and by
	// runRelationshipShape). Compare the parsed multiplicities, not the raw
	// strings, so semantically equal declarations written differently
	// ("1:n" one way, "0..n:1" the other) don't read as a conflict.
	fL, fR, fok := model.ParseCardinality(f.card)
	rL, rR, rok := model.ParseCardinality(r.card)
	if !fok || !rok {
		return
	}
	if fL != rR || fR != rL {
		res.add(SeverityError, CategorySemantic, f.path, fmt.Sprintf(
			"reciprocal cardinality conflict: %s→%s declares %q but %s→%s declares %q (expected %q, the inverse)",
			f.from,
			f.to,
			f.card,
			r.from,
			r.to,
			r.card,
			model.InvertCardinality(f.card),
		))
	}
}

// runPairing warns where a relationship declared from both ends cannot be
// paired up: one end declares the same line more than once, so which
// declaration is the reciprocal of which is undeterminable — the format has no
// way to say. The renderer refuses to guess and draws every declaration as its
// own line (ADR-0008), which is lossless but shows more lines than the author
// probably means, so it is worth naming.
//
// A warning, never an error: the model may be exactly right, and there is no
// wording of it the linter could demand instead. Declaring each relationship
// from one end only resolves it.
func runPairing(m *model.Model, res *Result) {
	for _, g := range model.EdgeGroups(m) {
		if qualifiedRefRE.MatchString(g.FirstN) || qualifiedRefRE.MatchString(g.SecondN) {
			continue
		}
		if !g.AmbiguousPairing() {
			continue
		}
		// Point at the first declaration on the crowded end — the one an author
		// would edit — preferring the alphabetically earlier entity when both
		// ends are crowded, so the finding is stable.
		at := g.First
		if len(at) < 2 && len(g.Second) > 1 {
			at = g.Second
		}
		d := at[0]
		res.Findings = append(res.Findings, Finding{
			Severity: SeverityWarning,
			Category: CategorySemantic,
			Path:     fmt.Sprintf("/entities/%s/relationships/%d", d.From, d.Index),
			Message: fmt.Sprintf(
				"ambiguous reciprocal pairing between %q and %q at cardinality %q: %s declares it %d time(s) and %s %d time(s), so which declaration is the reciprocal of which cannot be determined — the diagram draws every declaration as its own line; declare each relationship from one end only to pair them up",
				g.FirstN,
				g.SecondN,
				g.First[0].Rel.Cardinality,
				g.FirstN,
				len(g.First),
				g.SecondN,
				len(g.Second),
			),
		})
	}
}

// sortedMapKeys returns a map's string keys in stable order, so iteration that
// emits findings is deterministic.
func sortedMapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// entityRefs extracts backticked, entity-looking terms from freeform text. A
// term qualifies if, after stripping a trailing ".field" accessor, it is
// PascalCase. Lowercase terms (field names, types) are ignored.
func entityRefs(text string) []string {
	var out []string
	for _, m := range backtickRE.FindAllStringSubmatch(text, -1) {
		tok := m[1]
		if i := strings.IndexByte(tok, '.'); i >= 0 {
			tok = tok[:i]
		}
		tok = strings.TrimSpace(tok)
		if pascalCaseRE.MatchString(tok) {
			out = append(out, tok)
		}
	}
	return out
}

// readsAsProse reports whether a relationship role is written as a sentence
// rather than a role name. It judges the role as the diagram will draw it, so
// backticks — markup, not label text — come off first.
//
// Three signals, each of which alone wrecks a diagram label: too long, too many
// words, or a sentence terminator. Sentence punctuation is deliberately *not*
// tested character-by-character: a comma separates a short list of role names
// ("`Owner`, `Member`"), and a full stop is as likely to be an accessor
// ("`Project`.owner") or a version ("v1.0 owner") as the end of a sentence.
// "`Owner` or `Member`" passes; "the record this one supersedes" does not.
func readsAsProse(role string) bool {
	role = model.NormalizeRole(role)
	if role == "" {
		return false
	}
	if len([]rune(role)) > roleLabelMax {
		return true
	}
	if strings.Contains(role, ";") || strings.HasSuffix(role, ".") {
		return true
	}
	return len(strings.Fields(role)) > 4
}

// plural is a naive English pluralizer good enough to match entity names like
// Policy -> Policies and Project -> Projects.
func plural(s string) string {
	switch {
	case strings.HasSuffix(s, "y"):
		return s[:len(s)-1] + "ies"
	case strings.HasSuffix(s, "s"),
		strings.HasSuffix(s, "x"),
		strings.HasSuffix(s, "ch"),
		strings.HasSuffix(s, "sh"):
		return s + "es"
	default:
		return s + "s"
	}
}

// sortFindings orders findings so the most actionable surface first: errors
// before warnings, then by layer (structural → semantic → completeness), then
// by path and message for determinism.
func sortFindings(res *Result) {
	severityRank := map[Severity]int{SeverityError: 0, SeverityWarning: 1}
	categoryRank := map[Category]int{
		CategoryStructural:   0,
		CategorySemantic:     1,
		CategoryCompleteness: 2,
	}
	sort.SliceStable(res.Findings, func(i, j int) bool {
		a, b := res.Findings[i], res.Findings[j]
		if severityRank[a.Severity] != severityRank[b.Severity] {
			return severityRank[a.Severity] < severityRank[b.Severity]
		}
		if categoryRank[a.Category] != categoryRank[b.Category] {
			return categoryRank[a.Category] < categoryRank[b.Category]
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Message < b.Message
	})
}
