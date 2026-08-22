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
