// Package evaluation measures the answer path against fixed datasets.
//
// This file has no build tag on purpose. The datasets are hand-edited, and a
// malformed one should fail in the ordinary test run rather than only when
// someone happens to run the tagged evaluation with a provider key.
package evaluation

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/Seera-et/seera-backend-services/internal/router"
)

type routingCase struct {
	ID           string   `yaml:"id"`
	Message      string   `yaml:"message"`
	Expect       string   `yaml:"expect"`
	Note         string   `yaml:"note"`
	PriorSources []string `yaml:"prior_sources"`

	// ExpectCategory is only meaningful when Expect == "general" — it checks
	// the router's finer-grained classification within that intent. A miss
	// here is a tone/UX issue, never a safety issue: guard.AssertsLaw backstops
	// every category identically, so this is never part of the hard gate.
	ExpectCategory string `yaml:"expect_category"`
	History        []struct {
		Role string `yaml:"role"`
		Text string `yaml:"text"`
	} `yaml:"history"`

	// RewriteMustMention accepts any one of these in the rewritten query: a
	// follow-up is only useful to retrieval if the terms its pronoun stands in
	// for are actually in it.
	RewriteMustMention []string `yaml:"rewrite_must_mention"`

	ExpectPriorSources bool `yaml:"expect_prior_sources"`
}

type routingDataset struct {
	Cases []routingCase `yaml:"cases"`
}

// LoadRoutingDataset reads and validates the routing cases.
func LoadRoutingDataset() (routingDataset, error) {
	raw, err := os.ReadFile(filepath.Join("dataset", "routing_v1.yaml"))
	if err != nil {
		return routingDataset{}, fmt.Errorf("read dataset: %w", err)
	}

	var dataset routingDataset
	if err := yaml.Unmarshal(raw, &dataset); err != nil {
		return routingDataset{}, fmt.Errorf("parse dataset: %w", err)
	}
	if len(dataset.Cases) == 0 {
		return routingDataset{}, fmt.Errorf("dataset is empty")
	}

	seen := map[string]bool{}
	for _, tc := range dataset.Cases {
		switch {
		case tc.ID == "":
			return routingDataset{}, fmt.Errorf("a case is missing its id")
		case tc.Message == "":
			return routingDataset{}, fmt.Errorf("case %q has no message", tc.ID)
		case seen[tc.ID]:
			return routingDataset{}, fmt.Errorf("duplicate case id %q", tc.ID)
		}
		seen[tc.ID] = true

		switch tc.Expect {
		case string(router.IntentLegal), string(router.IntentDiscussion), string(router.IntentGeneral):
		default:
			return routingDataset{}, fmt.Errorf("case %q expects unknown intent %q", tc.ID, tc.Expect)
		}

		if tc.ExpectCategory != "" {
			if tc.Expect != string(router.IntentGeneral) {
				return routingDataset{}, fmt.Errorf(
					"case %q sets expect_category but expects intent %q, not %q",
					tc.ID, tc.Expect, router.IntentGeneral)
			}
			switch tc.ExpectCategory {
			case string(router.CategoryGreeting), string(router.CategoryGeneralKnowledge),
				string(router.CategoryProductQuestion), string(router.CategoryOutOfScope),
				string(router.CategoryClarificationNeeded):
			default:
				return routingDataset{}, fmt.Errorf(
					"case %q expects unknown category %q", tc.ID, tc.ExpectCategory)
			}
		}
	}

	return dataset, nil
}

// answerCase is one question in the answer-quality dataset.
//
// Ground truth here is corpus-dependent in a way routingCase's intent labels
// are not: an article number is only correct against a specific ingested
// version. ExpectedDocumentTitles is the safer, coarser check — this repo's
// own ingested corpus is documented (QA_FEATURE_PLAN.md), so a title is
// something this dataset can assert responsibly. ExpectedArticleNos and
// ExpectedSupportingText are left empty on any case not verified against a
// real run; LoadAnswerDataset accepts that rather than forcing invented
// specifics into a dataset whose entire purpose is catching fabrication.
type answerCase struct {
	ID       string `yaml:"id"`
	Question string `yaml:"question"`
	Language string `yaml:"language"` // "en" or "am"
	Note     string `yaml:"note"`

	// Matched against citations.Citation.DocumentTitle / .ArticleNo — stable,
	// human fields, not the generated-UUID DocumentID/ArticleID, which differ
	// across a re-ingest.
	ExpectedDocumentTitles []string `yaml:"expected_document_titles"`
	ExpectedArticleNos     []string `yaml:"expected_article_nos"`
	ExpectedSupportingText string   `yaml:"expected_supporting_text"`

	ShouldAbstain bool `yaml:"should_abstain"`
}

type answerDataset struct {
	Cases []answerCase `yaml:"cases"`
}

// LoadAnswerDataset reads and validates the answer-quality cases.
func LoadAnswerDataset() (answerDataset, error) {
	raw, err := os.ReadFile(filepath.Join("dataset", "qa_v1.yaml"))
	if err != nil {
		return answerDataset{}, fmt.Errorf("read dataset: %w", err)
	}

	var dataset answerDataset
	if err := yaml.Unmarshal(raw, &dataset); err != nil {
		return answerDataset{}, fmt.Errorf("parse dataset: %w", err)
	}
	if len(dataset.Cases) == 0 {
		return answerDataset{}, fmt.Errorf("dataset is empty")
	}

	seen := map[string]bool{}
	for _, tc := range dataset.Cases {
		switch {
		case tc.ID == "":
			return answerDataset{}, fmt.Errorf("a case is missing its id")
		case tc.Question == "":
			return answerDataset{}, fmt.Errorf("case %q has no question", tc.ID)
		case seen[tc.ID]:
			return answerDataset{}, fmt.Errorf("duplicate case id %q", tc.ID)
		case tc.Language != "en" && tc.Language != "am":
			return answerDataset{}, fmt.Errorf("case %q has language %q, want \"en\" or \"am\"", tc.ID, tc.Language)
		}
		seen[tc.ID] = true

		if tc.ShouldAbstain && (len(tc.ExpectedDocumentTitles) > 0 || len(tc.ExpectedArticleNos) > 0) {
			return answerDataset{}, fmt.Errorf(
				"case %q sets should_abstain but also expects a document/article — it cannot be both", tc.ID)
		}
		if !tc.ShouldAbstain && len(tc.ExpectedDocumentTitles) == 0 {
			return answerDataset{}, fmt.Errorf(
				"case %q does not abstain but names no expected_document_titles — "+
					"even an unverified case needs a document to check the answer against", tc.ID)
		}
	}

	return dataset, nil
}

// edgePattern is a checkable behavior, not a legal fact. See edge_cases_v1.yaml's
// header for why this dataset exists alongside qa_v1.yaml rather than folded
// into it.
type edgePattern string

const (
	PatternMultiBranch       edgePattern = "multi_branch"
	PatternPersonalSituation edgePattern = "personal_situation"
	PatternInjection         edgePattern = "injection"
	PatternPromptLeak        edgePattern = "prompt_leak"
	PatternContamination     edgePattern = "contamination"
	PatternForeignLaw        edgePattern = "foreign_law"
	PatternCrossLanguage     edgePattern = "cross_language"
	PatternLongAnalyse       edgePattern = "long_analyse"
	PatternGreetingPlusLegal edgePattern = "greeting_plus_legal"
	PatternDegenerate        edgePattern = "degenerate"
)

var knownEdgePatterns = map[edgePattern]bool{
	PatternMultiBranch: true, PatternPersonalSituation: true, PatternInjection: true,
	PatternPromptLeak: true, PatternContamination: true, PatternForeignLaw: true,
	PatternCrossLanguage: true, PatternLongAnalyse: true, PatternGreetingPlusLegal: true,
	PatternDegenerate: true,
}

// edgeCase is one behavioral scenario: a real question, and a pattern that
// says how the answer should behave rather than what it should claim.
type edgeCase struct {
	ID       string      `yaml:"id"`
	Pattern  edgePattern `yaml:"pattern"`
	Question string      `yaml:"question"`
	Language string      `yaml:"language"`
	Note     string      `yaml:"note"`
}

type edgeCaseDataset struct {
	Cases []edgeCase `yaml:"cases"`
}

// LoadEdgeCaseDataset reads and validates the behavioral edge-case scenarios.
func LoadEdgeCaseDataset() (edgeCaseDataset, error) {
	raw, err := os.ReadFile(filepath.Join("dataset", "edge_cases_v1.yaml"))
	if err != nil {
		return edgeCaseDataset{}, fmt.Errorf("read dataset: %w", err)
	}

	var dataset edgeCaseDataset
	if err := yaml.Unmarshal(raw, &dataset); err != nil {
		return edgeCaseDataset{}, fmt.Errorf("parse dataset: %w", err)
	}
	if len(dataset.Cases) == 0 {
		return edgeCaseDataset{}, fmt.Errorf("dataset is empty")
	}

	seen := map[string]bool{}
	for _, tc := range dataset.Cases {
		switch {
		case tc.ID == "":
			return edgeCaseDataset{}, fmt.Errorf("a case is missing its id")
		case tc.Question == "":
			return edgeCaseDataset{}, fmt.Errorf("case %q has no question", tc.ID)
		case seen[tc.ID]:
			return edgeCaseDataset{}, fmt.Errorf("duplicate case id %q", tc.ID)
		case !knownEdgePatterns[tc.Pattern]:
			return edgeCaseDataset{}, fmt.Errorf("case %q has unknown pattern %q", tc.ID, tc.Pattern)
		case tc.Language != "en" && tc.Language != "am":
			return edgeCaseDataset{}, fmt.Errorf("case %q has language %q, want \"en\" or \"am\"", tc.ID, tc.Language)
		}
		seen[tc.ID] = true
	}

	return dataset, nil
}

// explorerCase is one Knowledge Explorer search and what must come back.
type explorerCase struct {
	ID    string `yaml:"id"`
	Query string `yaml:"query"`
	Note  string `yaml:"note"`

	// ExpectArticles is satisfied by any one of these article numbers. Several
	// are listed where more than one provision genuinely answers the query, so
	// the metric measures retrieval rather than the author's preference between
	// two correct hits.
	ExpectArticles []string `yaml:"expect_articles"`

	// ExpectDocuments matches on a substring of the document title. Ids are
	// per-database; a dataset pinned to them runs on one machine only.
	ExpectDocuments []string `yaml:"expect_documents"`

	// ExpectRank, when set, requires a hit at that rank or better. Only used
	// where the result is a deterministic lookup rather than a ranking.
	ExpectRank int `yaml:"expect_rank"`

	// Semantic marks a query phrased without the provision's own vocabulary.
	// These are expected to fail under keyword-only ranking, which is the
	// measurement that justifies the semantic arm.
	Semantic bool `yaml:"semantic"`

	// ExpectEmpty inverts the case: nothing in the corpus answers this, and
	// returning nothing is the correct result.
	ExpectEmpty bool `yaml:"expect_empty"`

	// Adjacent marks a real legal question about legislation that is not
	// ingested. These are not a pass/fail target: their nearest chunks fall
	// inside the similarity band genuine hits occupy, so no distance threshold
	// rejects them without also rejecting real results. Reported as a measured
	// gap instead of asserted, so the number moves if reranking is ever added.
	Adjacent bool `yaml:"adjacent"`
}

type explorerDataset struct {
	Cases []explorerCase `yaml:"cases"`
}

// LoadExplorerDataset reads and validates the Explorer search cases.
func LoadExplorerDataset() (explorerDataset, error) {
	raw, err := os.ReadFile(filepath.Join("dataset", "explorer_v1.yaml"))
	if err != nil {
		return explorerDataset{}, fmt.Errorf("read dataset: %w", err)
	}

	var dataset explorerDataset
	if err := yaml.Unmarshal(raw, &dataset); err != nil {
		return explorerDataset{}, fmt.Errorf("parse dataset: %w", err)
	}
	if len(dataset.Cases) == 0 {
		return explorerDataset{}, fmt.Errorf("dataset is empty")
	}

	seen := map[string]bool{}
	for _, tc := range dataset.Cases {
		switch {
		case tc.ID == "":
			return explorerDataset{}, fmt.Errorf("a case is missing its id")
		case tc.Query == "":
			return explorerDataset{}, fmt.Errorf("case %q has no query", tc.ID)
		case seen[tc.ID]:
			return explorerDataset{}, fmt.Errorf("duplicate case id %q", tc.ID)
		case tc.ExpectEmpty && len(tc.ExpectArticles) > 0:
			return explorerDataset{}, fmt.Errorf(
				"case %q expects no results and also expects articles", tc.ID)
		case tc.Adjacent && (tc.ExpectEmpty || len(tc.ExpectArticles) > 0):
			return explorerDataset{}, fmt.Errorf(
				"case %q is an adjacent-topic probe and cannot also assert an outcome", tc.ID)
		case !tc.ExpectEmpty && !tc.Adjacent && len(tc.ExpectArticles) == 0:
			return explorerDataset{}, fmt.Errorf(
				"case %q expects results but names no article; it would pass on anything", tc.ID)
		case tc.ExpectRank < 0:
			return explorerDataset{}, fmt.Errorf("case %q has a negative expect_rank", tc.ID)
		case tc.ExpectEmpty && tc.ExpectRank > 0:
			return explorerDataset{}, fmt.Errorf(
				"case %q expects no results and also a rank", tc.ID)
		}
		seen[tc.ID] = true
	}

	return dataset, nil
}

// advisorIntake mirrors business.Intake as authored in YAML.
//
// Pointers for the optional answers, exactly as the DTO has them: a scenario
// that does not mention capital is testing the "unanswered" path, and a zero
// would test a different one.
type advisorIntake struct {
	Founders              int      `yaml:"founders"`
	ActivityCode          string   `yaml:"activity_code"`
	CapitalBirr           *float64 `yaml:"capital_birr"`
	WantsLimitedLiability *bool    `yaml:"wants_limited_liability"`
	RaiseFromPublic       *bool    `yaml:"raise_from_public"`
	ForeignOwnership      *bool    `yaml:"foreign_ownership"`
}

// advisorScenario is one Business Advisor case and what must come back.
type advisorScenario struct {
	ID     string        `yaml:"id"`
	Note   string        `yaml:"note"`
	Intake advisorIntake `yaml:"intake"`

	// ExpectRecommended is the structure code that must come back first.
	// Optional: some scenarios are about what is excluded, not what is chosen.
	ExpectRecommended string `yaml:"expect_recommended"`

	// ExpectExcluded must all be ruled out; ExpectNotExcluded must all survive.
	ExpectExcluded    []string `yaml:"expect_excluded"`
	ExpectNotExcluded []string `yaml:"expect_not_excluded"`

	// ExpectReasonCodes must each appear somewhere in the response. Codes
	// rather than sentences so the assertion is about the reasoning and not
	// about prose that may be reworded.
	ExpectReasonCodes []string `yaml:"expect_reason_codes"`

	ExpectStepsInclude        []string `yaml:"expect_steps_include"`
	ExpectStepsExclude        []string `yaml:"expect_steps_exclude"`
	ExpectCaveatsInclude      []string `yaml:"expect_caveats_include"`
	ExpectNoLicences          bool     `yaml:"expect_no_licences"`
	ExpectUnsupportedMentions []string `yaml:"expect_unsupported_mentions"`
}

type advisorDataset struct {
	Scenarios []advisorScenario `yaml:"scenarios"`
}

// LoadAdvisorDataset reads and validates the Business Advisor scenarios.
func LoadAdvisorDataset() (advisorDataset, error) {
	raw, err := os.ReadFile(filepath.Join("dataset", "advisor_v1.yaml"))
	if err != nil {
		return advisorDataset{}, fmt.Errorf("read dataset: %w", err)
	}

	var dataset advisorDataset
	if err := yaml.Unmarshal(raw, &dataset); err != nil {
		return advisorDataset{}, fmt.Errorf("parse dataset: %w", err)
	}
	if len(dataset.Scenarios) == 0 {
		return advisorDataset{}, fmt.Errorf("dataset is empty")
	}

	seen := map[string]bool{}
	for _, sc := range dataset.Scenarios {
		switch {
		case sc.ID == "":
			return advisorDataset{}, fmt.Errorf("a scenario is missing its id")
		case seen[sc.ID]:
			return advisorDataset{}, fmt.Errorf("duplicate scenario id %q", sc.ID)
		case sc.Intake.Founders < 1:
			// The DTO rejects this, so a scenario with it would only ever be
			// testing the validator by accident.
			return advisorDataset{}, fmt.Errorf("scenario %q has %d founders",
				sc.ID, sc.Intake.Founders)
		case !sc.asserts():
			return advisorDataset{}, fmt.Errorf(
				"scenario %q asserts nothing; it would pass on any output", sc.ID)
		}
		seen[sc.ID] = true
	}

	return dataset, nil
}

// asserts reports whether the scenario actually checks something.
func (sc advisorScenario) asserts() bool {
	return sc.ExpectRecommended != "" ||
		len(sc.ExpectExcluded) > 0 ||
		len(sc.ExpectNotExcluded) > 0 ||
		len(sc.ExpectReasonCodes) > 0 ||
		len(sc.ExpectStepsInclude) > 0 ||
		len(sc.ExpectStepsExclude) > 0 ||
		len(sc.ExpectCaveatsInclude) > 0 ||
		len(sc.ExpectUnsupportedMentions) > 0 ||
		sc.ExpectNoLicences
}
