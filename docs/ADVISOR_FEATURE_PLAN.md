# Feature 3 — Business & License Advisor

**Status:** built and **verified end to end against the local corpus** on 2026-09-18.
AC-003 passes at 12/12 scenarios (100%, which is the required bar), every citation
resolving to the article it claims. Two things the SRS specifies are deliberately not
implemented — see §6.
**Scope rule:** Feature 4 (Document Intelligence) remains out of scope.

---

## 1. The problem this feature has that the others do not

Architecture rule 9 says no legal thresholds in application code. Rule 11 says business
rules are deterministic Go, not LLM reasoning. Feature 3 needs both at once: *"a private
limited company may not have less than two or more than fifty members"* is simultaneously
a legal threshold and the business rule that decides the recommendation.

The resolution is the shape of this package:

> **The rule engine is code. The rule data is in the database, and every row cites the
> article it came from.**

`internal/business/rules.go` contains no legal number. It reads bounds off
`business_structures` rows and reports which provision each finding rests on. The PRD's own
data model anticipated this — `BusinessRule` is listed as an entity in §8.

### The check that makes it real

A rule set is loaded through `POST /api/v1/admin/business/rules`. Before anything is
written, **every citation is resolved against the published corpus**, and an unresolvable
one fails the whole publish:

```
$ curl ... -d '{"label":"x","structures":[{"code":"x","name":"X",
    "sources":[{"document":"Investment Proclamation No. 1180/2020","article":"5"}]}]}'
422 unverifiable_citation: no published document matches "Investment Proclamation
    No. 1180/2020", so a rule citing it cannot be verified
```

So "the advisor's rules are cited" is a property the system enforces, not a promise about
the curation process. A structure with no citation is rejected; so is a step or a licence.
Caveats may be uncited, because *"no investment legislation is indexed"* is a true
statement about the index with no article behind it by nature.

---

## 2. Decisions

| Area | Decision | Why |
|---|---|---|
| **No model on this path** | The recommendation is 100% deterministic | The SRS flow has an "LLM Explanation" stage. A model here could only paraphrase thresholds it must not restate, and a recommendation that cannot be traced to the rule that produced it fails AC-003's premise. Every sentence a reader sees is either a rule's own reason or the verbatim text of a provision. |
| Transport | JSON, not SSE | Nothing is generated, so there is no first-token latency to hide. Measured 11–48ms in-process, 213ms over HTTP. |
| Citations by `(document, article_no)` | not by `articles.id` | Article rows belong to a document *version*. Re-ingesting a document creates new rows with new ids, silently orphaning every rule pointing at the old ones. A document's identity is stable and an article number is how a lawyer cites. |
| Unknown bounds are `NULL` and **permissive** | with an explicit caveat | Same reasoning as `legal.Version.InForceAt` on an unknown effective date: silently dropping a lawful option is worse than offering one whose limit is unverified, provided the gap is stated. `NULL` is never rendered as "no limit" — those are different claims and only one is true. |
| Preferences never disqualify | only re-rank | Wanting limited liability is a wish. Offering shares to the public is a legal bar (Art. 495/2). The engine keeps the two categories apart, and the UI styles them differently. |
| Rule data lives in `workers/business/rules_v1.json` | loaded, not compiled | Same shape as document ingestion: curated offline, reviewed by a human, applied through an admin endpoint. No legal content in the binary. |

### Citations are attached per *facet*, not per structure

The first working version attached a structure's whole citation list to every finding about
it. The visible result: a recommendation whose **capital** reason cited Art. 174 *("Types of
Business Organizations")* instead of Art. 496 *("Capital")*. The reference resolved, pointed
at real law, and did not support the sentence beside it — which is exactly the failure
AC-005 is written against.

`business_rule_sources.facet` (migration 0008) fixes it. Now:

```
"Private limited company suits 2 founders."        → Art 495/4  Definition
"Your capital meets the 15000 ETB minimum."        → Art 496/1  Capital
"Members are not personally liable for its debts." → Art 495/1  Definition
```

Art. 495 is cited three times under three facets, which is why the uniqueness constraint
had to include the facet column.

---

## 3. Surface

```
GET  /api/v1/business/intake     the questionnaire, built from the published rule set
POST /api/v1/business/advisor    the recommendation
POST /api/v1/admin/business/rules   load a rule set (admin token, citations verified)
```

The questionnaire's options come from the rule set rather than the frontend. "The legal
forms available in Ethiopia" and "what counts as a trade" are statements about Ethiopian
law; a list of them hardcoded in TypeScript would be a legal claim in application code with
no source attached.

Frontend: `/advisor` — a six-step workflow where only the founder count is required,
because it alone decides most of the answer. A reader can ask for a recommendation as soon
as it is in.

---

## 4. What the corpus does and does not support

The Commercial Code 1243/2021 is indexed, and it carries the business-organization forms,
their bounds, and the commercial-register provisions. So **structure recommendation and
registration steps are fully groundable.**

Licensing is not. No Commercial Registration and Business Licensing Proclamation and no
Investment Proclamation are in the corpus (the banking, insurance and microfinance
proclamations exist in the source folder but are not ingested). The advisor therefore:

- returns **no licence rows at all**, because none could be cited;
- says so, in the response, scoped to what the reader actually asked;
- labels regulated sectors so the gap lands where it matters.

```
licences: []
caveat:   "Financial services are licensed under legislation that is not indexed here…
           Treat the absence of a licence below as missing information, not as permission."
unsupported: "Foreign ownership rules: no investment legislation is indexed…"
```

An empty licence list that read as "no licence needed" would be the most dangerous output
this feature could produce. It is the one thing the response goes out of its way to deny.

### A threshold deliberately left out

Art. 473/1/a makes a share company dissolvable where the number of shareholders falls below
*"the legally required minimum"* — **without stating the number anywhere in the indexed
text.** So `share_company.min_members` is NULL, the engine reports the bound as
unestablished, and a caveat tells the reader to confirm it. Writing 5 there from memory
would have been the easiest line in the feature and the worst.

---

## 5. Measured

```
go test -tags eval ./tests/evaluation/... -run TestAdvisorScenarios -v

Business Advisor — AC-003
  scenarios  12
  passed     12 (100%, target 100%)
```

100% is a fair bar here and nowhere else in this project: no model, no similarity ranking,
no temperature, so the same intake against the same rule set always produces the same
recommendation. A failure is a rule bug or a rule-data error, never variance.

Every scenario additionally asserts, cross-cutting, that **each reason cites at least one
provision and that every cited article resolved** in the published corpus — so a rule that
loses its citation fails whichever scenario reaches it first.

`tests/evaluation/dataset/advisor_v1.yaml` carries 12 scenarios (SRS §24 asks for 10),
each with a note naming the provision that makes its expectation correct. Without that
note a later reader cannot tell a deliberate expectation from a recorded bug.

### Two bugs the run found

- **A sole proprietorship was being told to draw up a memorandum of association.** Art. 172
  requires one of a business *organization*; Art. 3 treats a sole trader separately. Fixed
  by scoping the step (`structure_codes`) rather than leaving it global.
- **"General partnership: the corpus does not state how many members it requires"** — false.
  It states a minimum of 2 (Art. 172/1, "persons"); only the maximum is unstated. The
  engine now distinguishes a fully-known range, a known floor, a known ceiling, and
  neither.

### One ranking flaw the tests caught

With `scorePublicSharesFit` below `scoreMembersFit`, a query for five founders wanting
public subscription recommended a **general partnership** — whose own
public-subscription status the corpus does not establish, so it was never penalised — over
the **share company**, which is the form for exactly that. Ranking "nothing rules this out"
above "this does what you asked" is the wrong way round, and the weights now say so.

---

## 6. Not done

1. **No LLM explanation**, deliberately — see §2. Cheap to add later if wanted: `rag.Service`
   already accepts pre-supplied candidates, so the cited articles could be handed straight
   to the existing grounded-generation path without new retrieval.
2. **No licensing or investment law**, so the two questions entrepreneurs most want
   answered are answered with an explicit refusal rather than content. Ingesting the
   banking/insurance/microfinance proclamations would make the financial-services branch
   real; general trade licensing needs a document nobody has collected yet.
3. **No repository tests against Postgres.** The SQL is covered by the AC-003 eval and by
   hand against the running stack; the unit tests cover the engine, which is where the
   decisions are.
4. **`region` is carried on `Intake` but unused** — no rule turns on it, so the DTO does not
   accept it. It exists so the field has one obvious home if regional licensing law is ever
   ingested.
