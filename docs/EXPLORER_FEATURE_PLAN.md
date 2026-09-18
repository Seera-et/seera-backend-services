# Feature 2 — Ethiopian Legal Knowledge Explorer: search

**Status:** built and **verified end to end against the local corpus** on 2026-09-18.
AC-002 passes at 100% success@5 (target 95%), all four exact article lookups at rank 1,
search latency inside the < 1s budget. One limitation measured rather than closed — see
"The gap that is not closed".
**Scope rule:** Business Advisor (`internal/business/`) and Document Intelligence remain
out of scope. `internal/business/*.go` are still bare `package` clauses.

---

## 1. What already existed

The Explorer's **browse** half shipped with the Q&A feature: `internal/documents` served
the catalogue (`GET /documents`, `/documents/{id}`, `/documents/{id}/articles`) and
`/corpus/stats`, and `seera-web`'s `features/explorer/` rendered it. Its own package
comment said it plainly — *"It is a browse API, not a search API."*

So Feature 2's requirements split cleanly:

| Requirement | Before | Now |
|---|---|---|
| FR-EXP-001 keyword search | title + issuing authority only | full text of every provision |
| FR-EXP-002 semantic search | — | embedding arm, fused |
| FR-EXP-003 filters | language, type | + domain, year, document, version status |
| FR-EXP-004 browse by category | — | domain facets from `corpus/stats` |
| FR-EXP-005 open a document | done | unchanged |
| FR-EXP-006 navigate to an article | list from the top only | deep link + start cursor |
| FR-EXP-007 metadata | done | unchanged |
| FR-EXP-008 historical versions | done | + `include_superseded` on search |

---

## 2. Decisions

| Area | Decision | Why |
|---|---|---|
| Where search lives | `internal/documents`, not `internal/search` | `internal/search` is the answer path: published **and in force**, top-K, no paging. The Explorer legitimately shows superseded law and must page with a total. One package cannot hold both visibility rules without one of them being wrong somewhere. |
| Reuse from `internal/search` | `ParseArticleRefs`, `NormalizeNumber`, `RRFConstant` | Article-reference parsing and Ge'ez numerals are the same problem on both paths. Fusion is re-implemented (30 lines) because it fuses *articles* and reports every arm that agreed, where the answer path fuses chunks and collapses them to one label. |
| Result granularity | one hit per **article** | Three chunks of Article 45 listed separately reads as three findings when it is one. Arms return chunks and are reduced to one per article, first-seen — which, since arms order best-first, is that article's best chunk. |
| Two phases | rank over identifiers, then hydrate one page | Ranking touches no article text. Snippets and text are built for the 20 rows a reader will see, not the 240 that were ranked. Ranking once and paging inside the pool also keeps page 2 consistent with page 1. |
| Arms run concurrently | `errgroup` | Three independent queries, one of which waits on an embedding HTTP call, against a < 1s budget. Running them in sequence is the mistake the answer path made and had measured back at it (QA_FEATURE_PLAN §7). |
| `mode` is exposed | `hybrid` \| `keyword` \| `semantic` | The SRS asks for keyword and semantic as separate capabilities, and §5 below shows they genuinely disagree. A reader who knows which they want should be able to say so. |
| Highlighting | Private Use Area markers, split client-side | The snippet is legal text from the corpus. There must be no path from a document's contents to markup the browser executes, so the client splits on `U+E000`/`U+E001` and builds elements. Never `dangerouslySetInnerHTML`. |
| `total` is the pool | plus a `capped` flag | RRF over per-arm top-N has no corpus-wide count. Reporting 240 when 240 is really a truncation point would be a false claim about the corpus, so `capped` makes the number a floor and the UI says "More than 240". |
| Degrades without an embedder | hybrid → keyword, reported in the response | Browsing and lexical search need only a database. A deployment with no API key serves the search rather than failing it — and says which mode ran, so a reader is never told two modes are the same when one never ran. |

### Superseded law is opt-in

`include_superseded` defaults to **false**. A reader searching the corpus means the law as
it stands, and quietly mixing repealed provisions into that is the same mistake in the
Explorer that the answer path exists to prevent. Every hit carries its version status
either way, so when the flag is on the reader can see which is which.

---

## 3. Surface

```
GET /api/v1/documents/search
    ?q=                      required
    &mode=hybrid|keyword|semantic
    &language=en|am  &type=  &domain=  &year=  &document=
    &include_superseded=     &limit=(1..50)  &offset=

GET /api/v1/documents        + &domain= &year=
GET /api/v1/corpus/stats     + "domains": [{domain, documents, articles}]
```

Unknown filter values are **rejected**, not ignored — a filter that silently does nothing
looks to a reader like a corpus with holes in it.

`migrations/0006_article_number_lookup` adds `articles (article_no)`. Both the Explorer's
article arm and the answer path's exact-article arm search across every published version,
so neither could use `articles_version_article_no_idx`, which leads on `version_id`. The
answer path had been sequentially scanning since it was written.

---

## 4. The gap that is not closed

The semantic arm has a similarity floor (`documents.vectorMinSimilarity = 0.50`). kNN has
no natural floor — it returns the nearest chunks however far away they are — so without
one, *"how do I bake sourdough bread"* returned five articles of the Commercial Code.
That is not a weak result, it is a misleading one.

The floor is set from the measured distribution over the ingested corpus:

| Query class | Top cosine similarity |
|---|---|
| In corpus | 0.640 – 0.715 |
| Off topic (recipe, medical) | 0.485 – 0.498 |
| **Adjacent legal topic not in corpus** (VAT, traffic, labour) | **0.588 – 0.636** |

The first two bands are far apart, so rejecting off-topic queries is reliable and not a
delicate judgement. **The third band overlaps the first.** A threshold that excluded a
question about VAT (0.611) would also exclude a real hit at 0.640. So:

> Telling *"no tax law is indexed"* apart from *"here is the tax law"* needs a reranker
> judging relevance, not a distance.

That is not attempted here: a rerank call on the search path does not fit the < 1s budget
(SRS §14.1). It is **measured instead of hidden** — `TestExplorerAdjacentTopics` probes
three such queries, reports how many still return provisions (currently 3/3), and never
fails. A test that fails on a known gap is noise in every run until the gap closes; one
that prints the number makes it obvious when reranking finally moves it.

A found-and-fixed bug worth recording: the floor was first written as
`distance < (1 - $n)`, which let Postgres infer the parameter's type from the integer
literal. The predicate matched everything — the floor existed and filtered nothing, and
only the evaluation caught it. The distance is now computed in Go and bound as one
explicitly-cast value.

---

## 5. Measured

`go test -tags eval ./tests/evaluation/... -run TestExplorer` against the local Docker
corpus (Commercial Code 1243/2021 + the 1931 Constitution, 878 articles, 879 chunks,
`gemini-embedding-001`). Dataset: `tests/evaluation/dataset/explorer_v1.yaml`, 24 cases,
every article number read out of the database rather than assumed.

```
Explorer search — hybrid
  answerable cases      19
  success@5             100% (target 95%)      <- AC-002
  exact lookups at rank 4/4                    <- includes አንቀጽ ፪፻፵፭ → Article 245
  out-of-corpus correct 2/2
  over 1s               0 (worst 906ms)        <- SRS §14.1

Explorer search — by mode (success@5)
  mode       all cases           plain-language cases
  keyword    14/19 ( 74%)        0/ 4 (  0%)
  semantic   19/19 (100%)        4/ 4 (100%)
  hybrid     19/19 (100%)        4/ 4 (100%)
```

**Keyword search scores zero on plain-language queries.** "How do I pay for shares with
property instead of money" shares no vocabulary with Article 282, and lexically it finds
nothing at all. That is the number that justifies the embedding call in the search path.

**Semantic alone matches hybrid on this corpus, so the lexical arm's contribution is
unproven here.** Two documents is not enough to measure it: lexical retrieval earns its
place on exact legal terminology across many similarly-worded codes, which this corpus
does not yet contain. Do not read this row as "the keyword arm is unnecessary" — read it
as "not yet measurable". Re-run it when the corpus grows.

---

## 6. Not done

1. **Amharic is unmeasured.** The query path is exercised (Ge'ez numerals convert, the
   `simple` configuration and the trigram fallback both run without error) but no Amharic
   document is ingested, so Amharic *recall* is untested. Same blocker as the answer
   path's: see CORPUS.md.
2. **`legal_domain` is whatever ingestion was told.** The facets are honest about the
   corpus but only as good as the metadata entered at ingest; documents ingested without
   a domain are excluded from the category list rather than shown under an empty one.
3. **No repository-level tests against Postgres.** The SQL is verified by the evaluation
   suite and by hand against the running stack, not by unit tests with a fixture database.
   `documents` unit tests cover filter parsing, fusion and paging only.
4. **`docs/SYSTEM_DESIGN.md` is still empty** (0 bytes), and the PRD/SRS still say
   Next.js where the repo is a Vite SPA — the staleness QA_FEATURE_PLAN §2 already noted.
