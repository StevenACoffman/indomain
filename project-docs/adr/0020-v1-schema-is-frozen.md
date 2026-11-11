---
Status: proposed
---

# The v1 schema is frozen in indomain

indomain reads `*.modelith.yaml` files written for modelith, and its embedded
v1 schema is byte-identical to upstream modelith's. Every schema object is
`additionalProperties: false`, so any change to it, even an optional field,
makes the files that use the change unreadable by modelith. The format would
fork without either tool reporting it. indomain therefore does not change the
v1 schema. Improvements land as lint checks, renderer changes, or
documentation and skill guidance. A format extension is proposed upstream, or
becomes a separately versioned format under a URL indomain owns.

This replaces, for this repository, the `CLAUDE.md` rule that v1 "evolves in
place" while pre-release. That rule assumed no external corpus. Reading
modelith's files is now the corpus.

`TestADR_0020_SchemaV1Frozen` pins the embedded v1 bytes by SHA-256, which
matched upstream's `internal/schema/v1/modelith.schema.json` when this was
written.

## Considered options

- **Evolve v1 in place** (the earlier rule) was rejected. Each additive field
  silently forks the format from upstream.
- **Bump to v2 inside indomain** was rejected. The `modelith.sh/.../v2.json`
  namespace is upstream's to define, and a second v2 would collide with it.

## Consequences

The schema extensions that Voelter's domain-analysis guidance suggested are
deferred until upstream adopts them: glossary synonyms and "not covered"
notes, and a top-level list of open questions. The lint checks from the same
review need no schema change and are semantic warnings: an owned
relationship whose owner side is not `1`, and one name that ends with
another.
