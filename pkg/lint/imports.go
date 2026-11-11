package lint

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/StevenACoffman/indomain/pkg/model"
	"github.com/StevenACoffman/indomain/pkg/schema"
)

const (
	importOutsideRepository importLoadFailureKind = iota + 1
	importOutsideModelDirectory
	importUnreadable
	importNotDomainModel
	importUnsupportedSchema
)

var (
	// qualifiedRefRE matches a well-formed cross-model reference, "scope.Name":
	// exactly one dot, a scope slug before it, a PascalCase item name after.
	// Anything else containing a dot is a malformed reference, not a primitive
	// type — malformedRefReason says which way it is malformed.
	qualifiedRefRE = regexp.MustCompile(`^(` + model.ScopeSlug + `)\.([A-Z][A-Za-z0-9]*)$`)
	// scopeRE is the slug a bare import's filename has to yield. An explicitly
	// written scope is held to the same pattern by the schema.
	scopeRE = regexp.MustCompile(`^` + model.ScopeSlug + `$`)
)

// Files is the filesystem seam import resolution runs on: reading an imported
// model, and the two path questions that decide whether it may be read at all.
// Reading and containment sit behind one interface deliberately — judging the
// boundary against the local disk while reading from somewhere else would
// confine imports on a filesystem the reads never touch.
//
// It is not an fs.FS: fs.ValidPath rejects "..", and peer models commonly sit
// in sibling directories, so "../payments/payments.modelith.yaml" has to work.
type Files interface {
	// ReadFile reads the named file.
	ReadFile(path string) ([]byte, error)
	// ResolutionRoot returns the directory an import of the model at modelPath
	// may not resolve outside of, and whether an enclosing repository defined
	// it (ADR-0013).
	ResolutionRoot(modelPath string) (root string, inRepo bool)
	// Resolve returns path as this filesystem sees it — absolute, cleaned, and
	// symlink-resolved as far as it can be — so containment judges where a path
	// lands rather than how it was written. It must agree with ResolutionRoot:
	// the two results are compared against each other.
	Resolve(path string) string
}

// OSFiles resolves imported models against the local filesystem. Its path
// methods are in root.go, alongside the containment rules they implement.
type OSFiles struct{}

// importedModel is one successfully resolved entry of a model's imports list.
type importedModel struct {
	index int    // position in the importing model's imports list, for the finding path
	path  string // the path as written in imports
	loadedImport
}

// loadedImport is a contained import that was read successfully. model is set only
// when the contents parsed as a supported domain model.
type loadedImport struct {
	resolvedPath string
	source       []byte
	model        *model.Model
}

type importLoadFailureKind uint8

// importLoadFailure retains the data loadImports needs to preserve its
// import-specific diagnostics.
type importLoadFailure struct {
	kind         importLoadFailureKind
	resolvedPath string
	err          error
	version      string
}

// unboundScope accumulates the references to one scope no import binds, so a
// single missing import is reported once rather than once per reference site.
type unboundScope struct {
	path  string // where the first reference is
	typ   string // the first reference, verbatim
	item  string
	count int
}

// importLoader resolves a model's imports one entry at a time.
type importLoader struct {
	modelPath string
	root      string
	inRepo    bool
	files     Files
	res       *Result
	vendored  bool
	byScope   map[string]importedModel
	claimed   map[string]string
}

// qualifiedTypeChecker resolves qualified attribute types against the imports,
// remembering which scopes were referenced and which were bound to nothing.
type qualifiedTypeChecker struct {
	byScope map[string]importedModel
	claimed map[string]string
	res     *Result
	used    map[string]bool
	unbound map[string]*unboundScope
}

// ReadFile reads the named file from the local filesystem.
func (OSFiles) ReadFile(path string) ([]byte, error) {
	//nolint:wrapcheck // the error text is the finding's detail ("cannot be read: open ..."); a wrapping prefix would change that contract, and os.ReadFile's error already names the path.
	return os.ReadFile(path)
}

// runImports resolves the model's imports, checks every qualified reference
// against them, and reports an import nothing references.
//
// vendored says the model is a copy whose home is another repository, which
// silences the errors its imports list would raise here (see loadImports).
func runImports(modelPath string, m *model.Model, files Files, res *Result, vendored bool) {
	byScope, claimed := loadImports(modelPath, m, files, res, vendored)
	used := checkQualifiedTypes(m, byScope, claimed, res)
	for scope := range checkQualifiedEntities(m, byScope, claimed, res) {
		used[scope] = true
	}
	// An unreferenced import is a completeness finding, alongside the unused
	// enum and the unused glossary term: vocabulary the model declares and
	// nothing uses. Sharing their category means sharing their promotion under
	// --completeness error.
	for _, scope := range sortedMapKeys(byScope) {
		if used[scope] {
			continue
		}
		imp := byScope[scope]
		res.Findings = append(res.Findings, Finding{
			Severity: SeverityWarning,
			Category: CategoryCompleteness,
			Path:     fmt.Sprintf("/imports/%d", imp.index),
			Message: fmt.Sprintf(
				"import %q (scope %q) is never referenced — drop it, or reference one of its enums as %s.Name in an attribute type or one of its entities as %s.Entity in a relationship or subtypeOf",
				imp.path,
				scope,
				scope,
				scope,
			),
		})
	}
}

// loadImports reads each declared import and returns the ones that resolved,
// keyed by the scope the importing model binds them to, plus every scope the
// list claimed at all — including entries that then failed to load — mapped to
// the path of the first entry that claimed it. Every rejection is reported as an
// error: an import that cannot be resolved is a broken reference, not a gap.
//
// A vendored model is the exception, and it reports nothing here at all. Its
// imports name paths in the repository it came from, which do not exist in this
// one, so every entry would fail for a reason its authors cannot see and this
// repository is not expected to fix. Resolution is still attempted, because two
// vendored peers that import each other do resolve here; and every entry still
// claims its scope, which is what keeps a reference into an import that did not
// load from reading as a reference to nothing (see checkQualifiedTypes).
func loadImports(
	modelPath string,
	m *model.Model,
	files Files,
	res *Result,
	vendored bool,
) (byScope map[string]importedModel, claimed map[string]string) {
	byScope = map[string]importedModel{}
	claimed = map[string]string{}
	if len(m.Imports) == 0 {
		return byScope, claimed
	}
	root, inRepo := files.ResolutionRoot(modelPath)
	l := &importLoader{
		modelPath: modelPath,
		root:      root,
		inRepo:    inRepo,
		files:     files,
		res:       res,
		vendored:  vendored,
		byScope:   byScope,
		claimed:   claimed,
	}
	for i, imp := range m.Imports {
		l.load(i, imp)
	}
	return byScope, claimed
}

func (l *importLoader) reject(i int, format string, args ...any) {
	if l.vendored {
		return
	}
	l.res.add(
		SeverityError,
		CategorySemantic,
		fmt.Sprintf("/imports/%d", i),
		fmt.Sprintf(format, args...),
	)
}

func (l *importLoader) load(i int, imp model.Import) {
	if imp.Path == "" {
		return // the schema's minLength reports the empty path
	}
	// The scope is settled before the path is judged, and claimed before the
	// file is opened. Reference sites depend on the scope alone, so an entry
	// that names a usable one holds it whatever else is wrong with the entry:
	// a duplicate binding is then reported on its own terms instead of being
	// pre-empted by the second entry's unrelated trouble, and a broken path
	// does not also make every "scope.Name" that names it look like a
	// reference to nothing. A written scope is held to this same pattern by
	// the schema, which runs before this layer and gates it, so a scope that
	// fails here was derived from the filename — which is what the advice
	// below assumes. TestInvariant_ScopeSlugMatchesSchema guards the two.
	scopeOK := scopeRE.MatchString(imp.Scope)
	if scopeOK && l.claimDuplicate(i, imp) {
		return
	}
	if l.rejectPath(i, imp, scopeOK) {
		return
	}
	// Containment is decided before the file is opened, because opening it
	// is the leak: whether a path resolves, is missing, is unreadable, or
	// holds no model are four distinct diagnostics, and together they let a
	// model from an untrusted source probe the filesystem of whatever runner
	// lints it (ADR-0013).
	loaded, failure := loadImport(l.modelPath, l.root, l.inRepo, imp.Path, l.files)
	if failure != nil {
		l.rejectFailure(i, imp.Path, failure)
		return
	}
	l.byScope[imp.Scope] = importedModel{index: i, path: imp.Path, loadedImport: loaded}
}

// claimDuplicate binds imp's scope and reports whether another import already
// had, rejecting this one if so.
func (l *importLoader) claimDuplicate(i int, imp model.Import) bool {
	if prev, dup := l.claimed[imp.Scope]; dup {
		l.reject(
			i,
			"import %q binds scope %q, which import %q already binds — give one of them an explicit, different scope so %s.Name resolves unambiguously",
			imp.Path,
			imp.Scope,
			prev,
			imp.Scope,
		)
		return true
	}
	l.claimed[imp.Scope] = imp.Path
	return false
}

// rejectPath rejects an import whose path or derived scope cannot be used, and
// reports whether it did.
func (l *importLoader) rejectPath(i int, imp model.Import, scopeOK bool) bool {
	// A control character is never part of a filename, and one that reached
	// here would be carried into this diagnostic, the shell, and the rendered
	// Markdown. Every message here quotes the path with %q, so an escape
	// survives as text rather than as a line break in the output.
	if at := strings.IndexFunc(imp.Path, unicode.IsControl); at >= 0 {
		l.reject(
			i,
			"import path %q contains a control character at byte %d — a filename cannot contain one; check for a stray newline or escape in the YAML",
			imp.Path,
			at,
		)
		return true
	}
	if !scopeOK {
		// A filename that yields no usable slug is the case the explicit form
		// exists for.
		l.reject(
			i,
			"import %q binds the scope %q taken from its filename, which is not a valid slug (lowercase kebab-case) — name the scope explicitly instead: `- {scope: <slug>, path: %q}`",
			imp.Path,
			imp.Scope,
			imp.Path,
		)
		return true
	}
	if filepath.IsAbs(imp.Path) || imp.Path[0] == '/' {
		l.reject(
			i,
			"import %q is an absolute path — imports are relative to this model so they resolve in any checkout",
			imp.Path,
		)
		return true
	}
	return false
}

func (l *importLoader) rejectFailure(i int, importPath string, failure *importLoadFailure) {
	switch failure.kind {
	case importOutsideRepository:
		l.reject(
			i,
			"import %q resolves to %q, outside %q — that directory is the repository holding this model (the nearest ancestor with a .git entry), and an import may not name a file beyond it",
			importPath,
			failure.resolvedPath,
			l.root,
		)
	case importOutsideModelDirectory:
		l.reject(
			i,
			"import %q resolves to %q, outside %q — this model is in no repository, so resolution is confined to the directory holding it; move the imported model into that directory or below it",
			importPath,
			failure.resolvedPath,
			l.root,
		)
	case importUnreadable:
		l.reject(i, "import %q cannot be read: %v", importPath, failure.err)
	case importNotDomainModel:
		l.reject(
			i,
			"import %q is not a domain model — lint it on its own with `indomain lint` to see why",
			importPath,
		)
	case importUnsupportedSchema:
		l.reject(
			i,
			"import %q declares schema version %q, which this indomain does not support: %s (upgrade indomain, or move that model to a supported version)",
			importPath,
			failure.version,
			strings.Join(schema.SupportedVersions(), ", "),
		)
	}
}

// loadImport reads and validates an imported model after its path syntax and
// scope have been checked by loadImports.
func loadImport(
	modelPath, root string,
	inRepo bool,
	importPath string,
	files Files,
) (loadedImport, *importLoadFailure) {
	joined := filepath.Join(filepath.Dir(modelPath), importPath)
	resolvedPath := files.Resolve(joined)
	if !withinRoot(root, resolvedPath) {
		kind := importOutsideModelDirectory
		if inRepo {
			kind = importOutsideRepository
		}
		return loadedImport{}, &importLoadFailure{kind: kind, resolvedPath: resolvedPath}
	}
	data, err := files.ReadFile(joined)
	if err != nil {
		return loadedImport{}, &importLoadFailure{kind: importUnreadable, err: err}
	}
	loaded := loadedImport{resolvedPath: resolvedPath, source: data}
	imported, err := model.Parse(data)
	if err != nil || imported.Kind != "DomainModel" {
		return loaded, &importLoadFailure{kind: importNotDomainModel}
	}
	if !schema.Supported(imported.Version) {
		return loaded, &importLoadFailure{kind: importUnsupportedSchema, version: imported.Version}
	}
	loaded.model = imported
	return loaded, nil
}

// checkQualifiedTypes resolves every qualified attribute type against the
// imports and returns the set of scopes that were referenced.
//
// A qualified type that does not resolve is an error, where an unqualified
// PascalCase type that names no enum is only a warning (runSemantic). The
// asymmetry is deliberate: an unqualified name may be a primitive the author
// invented, while "scope.Name" can only be a cross-model reference.
func checkQualifiedTypes(
	m *model.Model,
	byScope map[string]importedModel,
	claimed map[string]string,
	res *Result,
) map[string]bool {
	c := &qualifiedTypeChecker{
		byScope: byScope,
		claimed: claimed,
		res:     res,
		used:    map[string]bool{},
		unbound: map[string]*unboundScope{},
	}
	for _, name := range m.EntityNames() {
		for i, attr := range m.Entities[name].Attributes {
			// A dot in a type can only be a cross-model reference: primitives are
			// lowercase words and enum names are PascalCase, neither of which
			// carries one. So a dotted type that is not well formed is a typo to
			// report, not a type to pass over.
			if !strings.Contains(attr.Type, ".") {
				continue
			}
			c.check(fmt.Sprintf("/entities/%s/attributes/%d/type", name, i), attr.Type)
		}
	}
	reportUnboundScopes(c.unbound, res)
	return c.used
}

func (c *qualifiedTypeChecker) broken(path, format string, args ...any) {
	c.res.add(SeverityError, CategorySemantic, path, fmt.Sprintf(format, args...))
}

func (c *qualifiedTypeChecker) check(path, typ string) {
	match := qualifiedRefRE.FindStringSubmatch(typ)
	if match == nil {
		c.broken(
			path,
			"attribute type %q is a malformed cross-model reference (%s) — write it as scope.Name: one dot, a lowercase kebab-case scope, a PascalCase item name",
			typ,
			malformedRefReason(typ),
		)
		// The scope the author was reaching for is still referenced, so a
		// typo in the item name does not also read as an unused import.
		if head, _, _ := strings.Cut(typ, "."); c.byScope[head].model != nil {
			c.used[head] = true
		}
		return
	}
	scope, item := match[1], match[2]
	imp, ok := c.byScope[scope]
	if !ok {
		c.noteUnbound(scope, item, path, typ)
		return
	}
	// The import is referenced even when the item does not resolve, so
	// the same mistake never reads as an unused import too.
	c.used[scope] = true
	if _, ok := imp.model.Enums[item]; ok {
		return
	}
	if _, ok := imp.model.Entities[item]; ok {
		c.broken(
			path,
			"attribute type %q resolves to the entity %q in %q — only an enum can be referenced across models",
			typ,
			item,
			imp.path,
		)
		return
	}
	c.broken(path, "%s", unresolvedItemMessage(typ, item, imp))
}

// noteUnbound records a reference to a scope no import loaded as.
func (c *qualifiedTypeChecker) noteUnbound(scope, item, path, typ string) {
	if _, listed := c.claimed[scope]; listed {
		// An import claims this scope but failed to load, and that
		// failure is already reported against the imports list. It is
		// the one thing to fix; saying the scope is unbound here would
		// report the same mistake again at every reference site.
		c.used[scope] = true
		return
	}
	if u, seen := c.unbound[scope]; seen {
		u.count++
		return
	}
	c.unbound[scope] = &unboundScope{path: path, typ: typ, item: item, count: 1}
}

// checkQualifiedEntities resolves qualified relationship targets and subtype
// parents against direct imports. Their imported semantics end at the boundary:
// local reciprocity, ownership, and subtype traversal do not inspect that model.
func checkQualifiedEntities(
	m *model.Model,
	byScope map[string]importedModel,
	claimed map[string]string,
	res *Result,
) map[string]bool {
	used := map[string]bool{}
	check := func(path, ref, kind string) {
		match := qualifiedRefRE.FindStringSubmatch(ref)
		if match == nil {
			return
		}
		scope, item := match[1], match[2]
		imp, ok := byScope[scope]
		if !ok {
			if _, listed := claimed[scope]; listed {
				used[scope] = true
				return
			}
			res.Findings = append(res.Findings, Finding{
				Severity: SeverityError,
				Category: CategorySemantic,
				Path:     path,
				Message: fmt.Sprintf(
					"%s %q references the scope %q, which no import binds — add the model that defines %s to `imports:`",
					kind,
					ref,
					scope,
					item,
				),
			})
			return
		}
		used[scope] = true
		if _, ok := imp.model.Entities[item]; !ok {
			res.Findings = append(res.Findings, Finding{
				Severity: SeverityError,
				Category: CategorySemantic,
				Path:     path,
				Message: fmt.Sprintf(
					"%s %q names no entity %q in %q — check the name, or whether you meant to import a different model",
					kind,
					ref,
					item,
					imp.path,
				),
			})
		}
	}
	for _, name := range m.EntityNames() {
		ent := m.Entities[name]
		check(fmt.Sprintf("/entities/%s/subtypeOf", name), ent.SubtypeOf, "subtype parent")
		for i, rel := range ent.Relationships {
			check(
				fmt.Sprintf("/entities/%s/relationships/%d/entity", name, i),
				rel.Entity,
				"relationship target",
			)
		}
	}
	return used
}

// unresolvedItemMessage explains a qualified type whose scope resolved but
// whose item is not there.
//
// The interesting case is an imported model that has imports of its own: the
// item may well be defined in one of them, and nothing about the reference site
// says why that does not resolve. Vendoring fetches one file and resolution
// reaches one hop (ADR-0010, ADR-0015), so the answer is always to import that
// model here as well and give it its own scope — never to expect this one to
// follow the chain. Saying so only when the imported model actually has imports
// keeps the advice off the plain case, where the mistake is a typo.
func unresolvedItemMessage(typ, item string, imp importedModel) string {
	msg := fmt.Sprintf("attribute type %q names no enum %q in %q", typ, item, imp.path)
	n := len(imp.model.Imports)
	if n == 0 {
		return msg + " — check the name, or whether you meant to import a different model"
	}
	theirs := fmt.Sprintf("%d models of its own", n)
	if n == 1 {
		theirs = "a model of its own"
	}
	return msg + fmt.Sprintf(
		" — that model imports %s, and resolution is not transitive: if %q is defined in one of them, add that model to this model's `imports:` too and reference it with its own scope",
		theirs,
		item,
	)
}

func reportUnboundScopes(unbound map[string]*unboundScope, res *Result) {
	for _, scope := range sortedMapKeys(unbound) {
		u := unbound[scope]
		msg := fmt.Sprintf(
			"attribute type %q references the scope %q, which no import binds — add the model that defines %s to `imports:`",
			u.typ,
			scope,
			u.item,
		)
		if u.count > 1 {
			msg += fmt.Sprintf(
				" (%d attribute types reference this scope; this is the first)",
				u.count,
			)
		}
		res.Findings = append(res.Findings, Finding{
			Severity: SeverityError,
			Category: CategorySemantic,
			Path:     u.path,
			Message:  msg,
		})
	}
}

// malformedRefReason names what is wrong with a type that contains a dot but is
// not a well-formed "scope.Name", so the diagnostic points at the fix instead of
// only restating the shape.
func malformedRefReason(typ string) string {
	scope, item, _ := strings.Cut(typ, ".")
	switch {
	case scope == "":
		return "nothing before the dot"
	case item == "":
		return "nothing after the dot"
	case strings.Contains(item, "."):
		return "more than one dot"
	case !scopeRE.MatchString(scope):
		return fmt.Sprintf("the scope %q is not lowercase kebab-case", scope)
	default:
		return fmt.Sprintf("the item name %q is not PascalCase", item)
	}
}
