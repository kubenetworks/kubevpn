// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2023 Datadog, Inc.

package appsec

import (
	"encoding/json"
	"fmt"

	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"

	rules "github.com/DataDog/appsec-internal-go/appsec"
	rc "github.com/DataDog/datadog-agent/pkg/remoteconfig/state"
)

type (
	// rulesManager is used to build a full rules file from a combination of rules fragments
	// The `base` fragment is the default rules (either local or received through ASM_DD),
	// and the `edits` fragments each represent a remote configuration update that affects the rules.
	// `basePath` is either empty if the local base rules are used, or holds the path of the ASM_DD config.
	rulesManager struct {
		latest   rulesFragment
		base     rulesFragment
		basePath string
		edits    map[string]rulesFragment
	}
	// rulesFragment can represent a full ruleset or a fragment of it.
	rulesFragment struct {
		Version     string          `json:"version,omitempty"`
		Metadata    interface{}     `json:"metadata,omitempty"`
		Rules       []interface{}   `json:"rules,omitempty"`
		Overrides   []interface{}   `json:"rules_override,omitempty"`
		Exclusions  []interface{}   `json:"exclusions,omitempty"`
		RulesData   []ruleDataEntry `json:"rules_data,omitempty"`
		Actions     []actionEntry   `json:"actions,omitempty"`
		CustomRules []interface{}   `json:"custom_rules,omitempty"`
	}

	ruleDataEntry rc.ASMDataRuleData
	rulesData     struct {
		RulesData []ruleDataEntry `json:"rules_data"`
	}

	actionEntry struct {
		ID         string `json:"id"`
		Type       string `json:"type"`
		Parameters struct {
			StatusCode     int    `json:"status_code"`
			GRPCStatusCode *int   `json:"grpc_status_code,omitempty"`
			Type           string `json:"type,omitempty"`
			Location       string `json:"location,omitempty"`
		} `json:"parameters,omitempty"`
	}
)

// defaultRulesFragment returns a rulesFragment created using the default static recommended rules
func defaultRulesFragment() rulesFragment {
	var f rulesFragment
	if err := json.Unmarshal([]byte(rules.StaticRecommendedRules), &f); err != nil {
		log.Debug("appsec: error unmarshalling default rules: %v", err)
	}
	return f
}

func (r_ *rulesFragment) clone() rulesFragment {
	var f rulesFragment
	f.Version = r_.Version
	f.Metadata = r_.Metadata
	f.Overrides = append(f.Overrides, r_.Overrides...)
	f.Exclusions = append(f.Exclusions, r_.Exclusions...)
	f.RulesData = append(f.RulesData, r_.RulesData...)
	f.CustomRules = append(f.CustomRules, r_.CustomRules...)
	// TODO (Francois Mazeau): copy more fields once we handle them
	return f
}

// newRulesManager initializes and returns a new rulesManager using the provided rules.
// If no rules are provided (nil), the default rules are used instead.
// If the provided rules are invalid, an error is returned
func newRulesManager(rules []byte) (*rulesManager, error) {
	var f rulesFragment
	if rules == nil {
		f = defaultRulesFragment()
		log.Debug("appsec: rulesManager: using default rules configuration")
	} else if err := json.Unmarshal(rules, &f); err != nil {
		log.Debug("appsec: cannot create rulesManager from specified rules")
		return nil, err
	}
	return &rulesManager{
		latest: f,
		base:   f,
		edits:  map[string]rulesFragment{},
	}, nil
}

func (r *rulesManager) clone() *rulesManager {
	var clone rulesManager
	clone.edits = make(map[string]rulesFragment, len(r.edits))
	for k, v := range r.edits {
		clone.edits[k] = v
	}
	clone.base = r.base.clone()
	clone.latest = r.latest.clone()
	return &clone
}

func (r *rulesManager) addEdit(cfgPath string, f rulesFragment) {
	r.edits[cfgPath] = f
}

func (r *rulesManager) removeEdit(cfgPath string) {
	delete(r.edits, cfgPath)
}

func (r *rulesManager) changeBase(f rulesFragment, basePath string) {
	r.base = f
	r.basePath = basePath
}

// compile compiles the rulesManager fragments together stores the result in r.latest
func (r *rulesManager) compile() {
	if r.base.Rules == nil || len(r.base.Rules) == 0 {
		r.base = defaultRulesFragment()
	}
	r.latest = r.base

	// Simply concatenate the content of each top level rule field as specified in our RFCs
	for _, v := range r.edits {
		r.latest.Overrides = append(r.latest.Overrides, v.Overrides...)
		r.latest.Exclusions = append(r.latest.Exclusions, v.Exclusions...)
		r.latest.Actions = append(r.latest.Actions, v.Actions...)
		r.latest.RulesData = append(r.latest.RulesData, v.RulesData...)
		r.latest.CustomRules = append(r.latest.CustomRules, v.CustomRules...)
	}
}

// raw returns a compact json version of the rules
func (r *rulesManager) raw() []byte {
	data, _ := json.Marshal(r.latest)
	return data
}

// String returns the string representation of the latest compiled json rules.
func (r *rulesManager) String() string {
	return fmt.Sprintf("%+v", r.latest)
}
