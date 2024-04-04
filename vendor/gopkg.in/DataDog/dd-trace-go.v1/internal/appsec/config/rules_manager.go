// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2023 Datadog, Inc.

package config

import (
	"encoding/json"
	"fmt"

	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"

	rules "github.com/DataDog/appsec-internal-go/appsec"
	rc "github.com/DataDog/datadog-agent/pkg/remoteconfig/state"
)

type (
	// RulesManager is used to build a full rules file from a combination of rules fragments
	// The `Base` fragment is the default rules (either local or received through ASM_DD),
	// and the `Edits` fragments each represent a remote configuration update that affects the rules.
	// `BasePath` is either empty if the local Base rules are used, or holds the path of the ASM_DD config.
	RulesManager struct {
		Latest   RulesFragment
		Base     RulesFragment
		BasePath string
		Edits    map[string]RulesFragment
	}
	// RulesFragment can represent a full ruleset or a fragment of it.
	RulesFragment struct {
		Version     string          `json:"version,omitempty"`
		Metadata    interface{}     `json:"metadata,omitempty"`
		Rules       []interface{}   `json:"rules,omitempty"`
		Overrides   []interface{}   `json:"rules_override,omitempty"`
		Exclusions  []interface{}   `json:"exclusions,omitempty"`
		RulesData   []RuleDataEntry `json:"rules_data,omitempty"`
		Actions     []ActionEntry   `json:"actions,omitempty"`
		CustomRules []interface{}   `json:"custom_rules,omitempty"`
		Processors  []interface{}   `json:"processors,omitempty"`
		Scanners    []interface{}   `json:"scanners,omitempty"`
	}

	// RuleDataEntry represents an entry in the "rules_data" top level field of a rules file
	RuleDataEntry rc.ASMDataRuleData
	// RulesData is a slice of RulesDataEntry
	RulesData struct {
		RulesData []RuleDataEntry `json:"rules_data"`
	}

	// ActionEntry represents an entry in the "actions" top level field of a rules file
	ActionEntry struct {
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

// DefaultRulesFragment returns a RulesFragment created using the default static recommended rules
func DefaultRulesFragment() RulesFragment {
	var f RulesFragment
	if err := json.Unmarshal([]byte(rules.StaticRecommendedRules), &f); err != nil {
		log.Debug("appsec: error unmarshalling default rules: %v", err)
	}
	return f
}

func (f *RulesFragment) clone() (clone RulesFragment) {
	clone.Version = f.Version
	clone.Metadata = f.Metadata
	clone.Overrides = cloneSlice(f.Overrides)
	clone.Exclusions = cloneSlice(f.Exclusions)
	clone.RulesData = cloneSlice(f.RulesData)
	clone.CustomRules = cloneSlice(f.CustomRules)
	clone.Processors = cloneSlice(f.Processors)
	clone.Scanners = cloneSlice(f.Scanners)
	// TODO (Francois Mazeau): copy more fields once we handle them
	return
}

func cloneSlice[T any](slice []T) []T {
	// TODO: use slices.Clone once go1.21 is the min supported go runtime.
	clone := make([]T, len(slice), cap(slice))
	copy(clone, slice)
	return clone
}

// NewRulesManeger initializes and returns a new RulesManager using the provided rules.
// If no rules are provided (nil), the default rules are used instead.
// If the provided rules are invalid, an error is returned
func NewRulesManeger(rules []byte) (*RulesManager, error) {
	var f RulesFragment
	if rules == nil {
		f = DefaultRulesFragment()
		log.Debug("appsec: RulesManager: using default rules configuration")
	} else if err := json.Unmarshal(rules, &f); err != nil {
		log.Debug("appsec: cannot create RulesManager from specified rules")
		return nil, err
	}
	return &RulesManager{
		Latest: f,
		Base:   f,
		Edits:  map[string]RulesFragment{},
	}, nil
}

// Clone returns a duplicate of the current rules manager object
func (r *RulesManager) Clone() (clone RulesManager) {
	clone.Edits = make(map[string]RulesFragment, len(r.Edits))
	for k, v := range r.Edits {
		clone.Edits[k] = v
	}
	clone.BasePath = r.BasePath
	clone.Base = r.Base.clone()
	clone.Latest = r.Latest.clone()
	return
}

// AddEdit appends the configuration to the map of edits in the rules manager
func (r *RulesManager) AddEdit(cfgPath string, f RulesFragment) {
	r.Edits[cfgPath] = f
}

// RemoveEdit deletes the configuration associated to `cfgPath` in the edits slice
func (r *RulesManager) RemoveEdit(cfgPath string) {
	delete(r.Edits, cfgPath)
}

// ChangeBase sets a new rules fragment base for the rules manager
func (r *RulesManager) ChangeBase(f RulesFragment, basePath string) {
	r.Base = f
	r.BasePath = basePath
}

// Compile compiles the RulesManager fragments together stores the result in r.Latest
func (r *RulesManager) Compile() {
	if r.Base.Rules == nil || len(r.Base.Rules) == 0 {
		r.Base = DefaultRulesFragment()
	}
	r.Latest = r.Base

	// Simply concatenate the content of each top level rule field as specified in our RFCs
	for _, v := range r.Edits {
		r.Latest.Overrides = append(r.Latest.Overrides, v.Overrides...)
		r.Latest.Exclusions = append(r.Latest.Exclusions, v.Exclusions...)
		r.Latest.Actions = append(r.Latest.Actions, v.Actions...)
		r.Latest.RulesData = append(r.Latest.RulesData, v.RulesData...)
		r.Latest.CustomRules = append(r.Latest.CustomRules, v.CustomRules...)
		r.Latest.Processors = append(r.Latest.Processors, v.Processors...)
		r.Latest.Scanners = append(r.Latest.Scanners, v.Scanners...)
	}
}

// Raw returns a compact json version of the rules
func (r *RulesManager) Raw() []byte {
	data, _ := json.Marshal(r.Latest)
	return data
}

// String returns the string representation of the Latest compiled json rules.
func (r *RulesManager) String() string {
	return fmt.Sprintf("%+v", r.Latest)
}
