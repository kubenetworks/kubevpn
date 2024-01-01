// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2023-present Datadog, Inc.
package appsec

import "encoding/json"

// DefaultRuleset returns the marshaled default recommended security rules for AppSec
func DefaultRuleset() ([]byte, error) {
	rules, err := DefaultRulesetMap()
	if err != nil {
		return nil, err
	}
	return json.Marshal(rules)
}

// DefaultRulesetMap returns the unmarshaled default recommended security rules for AppSec
func DefaultRulesetMap() (map[string]any, error) {
	var rules map[string]any
	var processors map[string]any
	if err := json.Unmarshal([]byte(StaticRecommendedRules), &rules); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(StaticProcessors), &processors); err != nil {
		return nil, err
	}
	for k, v := range processors {
		rules[k] = v
	}

	return rules, nil
}
