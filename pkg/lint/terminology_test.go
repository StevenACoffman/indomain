package lint

import (
	"strings"
	"testing"
)

// TestTerminology_ShortFormCollision pins when one name ending with another
// warns: across entities and glossary terms, at the longer name, unless the
// longer is a subtype of the shorter or its definition backticks the shorter.
// The warning never blocks.
func TestTerminology_ShortFormCollision(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		src  string
		want []Finding
	}{
		{
			name: "entity ends with a glossary term",
			src: `
glossary:
  Session: A login session.
entities:
  TreatmentSession:
    definition: A clinical appointment.
`,
			want: []Finding{collision("/entities/TreatmentSession", "TreatmentSession", "Session")},
		},
		{
			name: "two glossary terms",
			src: `
glossary:
  Session: A login session.
  TreatmentSession: A clinical appointment.
entities:
  Patient:
    definition: A person receiving care.
`,
			want: []Finding{collision("/glossary/TreatmentSession", "TreatmentSession", "Session")},
		},
		{
			name: "two entities, acronym prefix",
			src: `
entities:
  Request:
    definition: A unit of work.
  HTTPRequest:
    definition: An inbound call.
`,
			want: []Finding{collision("/entities/HTTPRequest", "HTTPRequest", "Request")},
		},
		{
			name: "shared prefix is not a collision",
			src: `
entities:
  Session:
    definition: A login session.
  SessionToken:
    definition: The credential for one session.
`,
		},
		{
			name: "subtype carries its parent's name",
			src: `
entities:
  Card:
    definition: A payment card.
  CreditCard:
    definition: A card drawing on credit.
    subtypeOf: Card
`,
		},
		{
			name: "transitive subtype carries its ancestor's name",
			src: `
entities:
  Card:
    definition: A payment card.
  BankCard:
    definition: A card a bank issues.
    subtypeOf: Card
  DebitBankCard:
    definition: A bank card drawing on a balance.
    subtypeOf: BankCard
`,
		},
		{
			name: "definition records the difference",
			src: `
glossary:
  Session: A login session.
entities:
  TreatmentSession:
    definition: A clinical appointment — not a ` + "`Session`" + `, which is a login.
`,
		},
		{
			name: "plural mention records the difference",
			src: `
glossary:
  Policy: A rule set.
entities:
  RetentionPolicy:
    definition: How long data is kept; unlike other ` + "`Policies`" + `, never edited.
`,
		},
		{
			name: "subtype cycle terminates",
			src: `
entities:
  Card:
    definition: A payment card.
    subtypeOf: CreditCard
  CreditCard:
    definition: A card drawing on credit.
    subtypeOf: Card
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res, err := Run(testModelPath, []byte("kind: DomainModel\nversion: v1\n"+tc.src), nil)
			if err != nil {
				t.Fatal(err)
			}
			var got []Finding
			for _, f := range res.Findings {
				if strings.Contains(f.Message, "which names a different concept") {
					got = append(got, f)
				}
			}
			if len(got) != len(tc.want) {
				t.Fatalf("expected %d collision findings, got %d: %+v", len(tc.want), len(got), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("finding %d mismatch\ngot:  %+v\nwant: %+v", i, got[i], tc.want[i])
				}
			}
			if (&Result{Findings: got}).HasBlocking(true) {
				t.Error("the collision warning must not block, even under --completeness error")
			}
		})
	}
}

func collision(path, long, short string) Finding {
	return Finding{
		Severity: SeverityWarning,
		Category: CategorySemantic,
		Path:     path,
		Message: `"` + long + `" ends with "` + short + `", which names a different concept, so shortening one to the other confuses them — rename one, or say how they differ by backticking ` +
			"`" + short + "` in " + long + "'s definition",
	}
}
