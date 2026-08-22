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
	History      []struct {
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
	}

	return dataset, nil
}
