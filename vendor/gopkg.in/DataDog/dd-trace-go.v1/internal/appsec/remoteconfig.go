// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2022 Datadog, Inc.

//go:build appsec
// +build appsec

package appsec

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/remoteconfig"

	rc "github.com/DataDog/datadog-agent/pkg/remoteconfig/state"
)

func genApplyStatus(ack bool, err error) rc.ApplyStatus {
	status := rc.ApplyStatus{
		State: rc.ApplyStateUnacknowledged,
	}
	if err != nil {
		status.State = rc.ApplyStateError
		status.Error = err.Error()
	} else if ack {
		status.State = rc.ApplyStateAcknowledged
	}

	return status
}

func statusesFromUpdate(u remoteconfig.ProductUpdate, ack bool, err error) map[string]rc.ApplyStatus {
	statuses := make(map[string]rc.ApplyStatus, len(u))
	for path := range u {
		statuses[path] = genApplyStatus(ack, err)
	}
	return statuses
}

func mergeMaps[K comparable, V any](m1 map[K]V, m2 map[K]V) map[K]V {
	for key, value := range m2 {
		m1[key] = value
	}
	return m1
}

// combineRCRulesUpdates updates the state of the given rulesManager with the combination of all the provided rules updates
func combineRCRulesUpdates(r *rulesManager, updates map[string]remoteconfig.ProductUpdate) (map[string]rc.ApplyStatus, error) {
	statuses := map[string]rc.ApplyStatus{}
	// Set the default statuses for all updates to unacknowledged
	for _, u := range updates {
		statuses = mergeMaps(statuses, statusesFromUpdate(u, false, nil))
	}
	var err error
updateLoop:
	// Process rules related updates
	for p, u := range updates {
		if u != nil && len(u) == 0 {
			continue
		}
		switch p {
		case rc.ProductASMData:
			// Merge all rules data entries together and store them as a rulesManager edit entry
			rulesData, status := mergeRulesData(u)
			statuses = mergeMaps(statuses, status)
			r.addEdit("asmdata", rulesFragment{RulesData: rulesData})
		case rc.ProductASMDD:
			// Switch the base rules of the rulesManager if the config received through ASM_DD is valid
			// If the config was removed, switch back to the static recommended rules
			if len(u) > 1 { // Don't process configs if more than one is received for ASM_DD
				log.Debug("appsec: Remote config: more than one config received for ASM_DD. Updates won't be applied")
				err = errors.New("More than one config received for ASM_DD")
				statuses = mergeMaps(statuses, statusesFromUpdate(u, true, err))
				break updateLoop
			}
			for path, data := range u {
				if data == nil {
					log.Debug("appsec: Remote config: ASM_DD config removed. Switching back to default rules")
					r.changeBase(defaultRulesFragment(), "")
					break
				}
				var newBase rulesFragment
				if err = json.Unmarshal(data, &newBase); err != nil {
					log.Debug("appsec: Remote config: could not unmarshall ASM_DD rules: %v", err)
					statuses[path] = genApplyStatus(true, err)
					break updateLoop
				}
				log.Debug("appsec: Remote config: switching to %s as the base rules file", path)
				r.changeBase(newBase, path)
			}
		case rc.ProductASM:
			// Store each config received through ASM as an edit entry in the rulesManager
			// Those entries will get merged together when the final rules are compiled
			// If a config gets removed, the rulesManager edit entry gets removed as well
			for path, data := range u {
				log.Debug("appsec: Remote config: processing the %s ASM config", path)
				if data == nil {
					log.Debug("appsec: Remote config: ASM config %s was removed", path)
					r.removeEdit(path)
					continue
				}
				var f rulesFragment
				if err = json.Unmarshal(data, &f); err != nil {
					log.Debug("appsec: Remote config: error processing ASM config %s: %v", path, err)
					statuses[path] = genApplyStatus(true, err)
					break updateLoop
				}
				r.addEdit(path, f)
			}
		default:
			log.Debug("appsec: Remote config: ignoring unsubscribed product %s", p)
		}
	}

	// Set all statuses to ack if no error occured
	if err == nil {
		for _, u := range updates {
			statuses = mergeMaps(statuses, statusesFromUpdate(u, true, nil))
		}
	}

	return statuses, err

}

// onRemoteActivation is the RC callback called when an update is received for ASM_FEATURES
func (a *appsec) onRemoteActivation(updates map[string]remoteconfig.ProductUpdate) map[string]rc.ApplyStatus {
	statuses := map[string]rc.ApplyStatus{}
	if u, ok := updates[rc.ProductASMFeatures]; ok {
		statuses = a.handleASMFeatures(u)
	}
	return statuses

}

// onRCRulesUpdate is the RC callback called when security rules related RC updates are available
func (a *appsec) onRCRulesUpdate(updates map[string]remoteconfig.ProductUpdate) map[string]rc.ApplyStatus {
	// If appsec was deactivated through RC, stop here
	if !a.started {
		return map[string]rc.ApplyStatus{}
	}

	// Create a new local rulesManager
	r := a.cfg.rulesManager.clone()
	statuses, err := combineRCRulesUpdates(r, updates)
	if err != nil {
		log.Debug("appsec: Remote config: not applying any updates because of error: %v", err)
		return statuses
	}

	// Compile the final rules once all updates have been processed and no error occurred
	r.compile()
	log.Debug("appsec: Remote config: final compiled rules: %s", r)

	// If an error occurs while updating the WAF handle, don't swap the rulesManager and propagate the error
	// to all config statuses since we can't know which config is the faulty one
	if err = a.swapWAF(r.latest); err != nil {
		log.Error("appsec: Remote config: could not apply the new security rules: %v", err)
		for k := range statuses {
			statuses[k] = genApplyStatus(true, err)
		}
		return statuses
	}
	// Replace the rulesManager with the new one holding the new state
	a.cfg.rulesManager = r

	return statuses
}

// handleASMFeatures deserializes an ASM_FEATURES configuration received through remote config
// and starts/stops appsec accordingly.
func (a *appsec) handleASMFeatures(u remoteconfig.ProductUpdate) map[string]rc.ApplyStatus {
	statuses := statusesFromUpdate(u, false, nil)
	if l := len(u); l > 1 {
		log.Error("appsec: Remote config: %d configs received for ASM_FEATURES. Expected one at most, returning early", l)
		return statuses
	}
	for path, raw := range u {
		var data rc.ASMFeaturesData
		status := rc.ApplyStatus{State: rc.ApplyStateAcknowledged}
		var err error
		log.Debug("appsec: Remote config: processing %s", path)

		// A nil config means ASM was disabled, and we stopped receiving the config file
		// Don't ack the config in this case and return early
		if raw == nil {
			log.Debug("appsec: Remote config: Stopping AppSec")
			a.stop()
			return statuses
		}
		if err = json.Unmarshal(raw, &data); err != nil {
			log.Error("appsec: Remote config: error while unmarshalling %s: %v. Configuration won't be applied.", path, err)
		} else if data.ASM.Enabled && !a.started {
			log.Debug("appsec: Remote config: Starting AppSec")
			if err = a.start(); err != nil {
				log.Error("appsec: Remote config: error while processing %s. Configuration won't be applied: %v", path, err)
			}
		} else if !data.ASM.Enabled && a.started {
			log.Debug("appsec: Remote config: Stopping AppSec")
			a.stop()
		}
		if err != nil {
			status = genApplyStatus(false, err)
		}
		statuses[path] = status
	}

	return statuses
}

func mergeRulesData(u remoteconfig.ProductUpdate) ([]ruleDataEntry, map[string]rc.ApplyStatus) {
	// Following the RFC, merging should only happen when two rules data with the same ID and same Type are received
	// allRulesData[ID][Type] will return the rules data of said id and type, if it exists
	allRulesData := make(map[string]map[string]ruleDataEntry)
	statuses := statusesFromUpdate(u, true, nil)

	for path, raw := range u {
		log.Debug("appsec: Remote config: processing %s", path)

		// A nil config means ASM_DATA was disabled, and we stopped receiving the config file
		// Don't ack the config in this case
		if raw == nil {
			log.Debug("appsec: remote config: %s disabled", path)
			statuses[path] = genApplyStatus(false, nil)
			continue
		}

		var rulesData rulesData
		if err := json.Unmarshal(raw, &rulesData); err != nil {
			log.Debug("appsec: Remote config: error while unmarshalling payload for %s: %v. Configuration won't be applied.", path, err)
			statuses[path] = genApplyStatus(false, err)
			continue
		}

		// Check each entry against allRulesData to see if merging is necessary
		for _, ruleData := range rulesData.RulesData {
			if allRulesData[ruleData.ID] == nil {
				allRulesData[ruleData.ID] = make(map[string]ruleDataEntry)
			}
			if data, ok := allRulesData[ruleData.ID][ruleData.Type]; ok {
				// Merge rules data entries with the same ID and Type
				data.Data = mergeRulesDataEntries(data.Data, ruleData.Data)
				allRulesData[ruleData.ID][ruleData.Type] = data
			} else {
				allRulesData[ruleData.ID][ruleData.Type] = ruleData
			}
		}
	}

	// Aggregate all the rules data before passing it over to the WAF
	var rulesData []ruleDataEntry
	for _, m := range allRulesData {
		for _, data := range m {
			rulesData = append(rulesData, data)
		}
	}
	return rulesData, statuses
}

// mergeRulesDataEntries merges two slices of rules data entries together, removing duplicates and
// only keeping the longest expiration values for similar entries.
func mergeRulesDataEntries(entries1, entries2 []rc.ASMDataRuleDataEntry) []rc.ASMDataRuleDataEntry {
	mergeMap := map[string]int64{}

	for _, entry := range entries1 {
		mergeMap[entry.Value] = entry.Expiration
	}
	// Replace the entry only if the new expiration timestamp goes later than the current one
	// If no expiration timestamp was provided (default to 0), then the data doesn't expire
	for _, entry := range entries2 {
		if exp, ok := mergeMap[entry.Value]; !ok || entry.Expiration == 0 || entry.Expiration > exp {
			mergeMap[entry.Value] = entry.Expiration
		}
	}
	// Create the final slice and return it
	entries := make([]rc.ASMDataRuleDataEntry, 0, len(mergeMap))
	for val, exp := range mergeMap {
		entries = append(entries, rc.ASMDataRuleDataEntry{
			Value:      val,
			Expiration: exp,
		})
	}
	return entries
}

func (a *appsec) startRC() error {
	if a.cfg.rc != nil {
		return remoteconfig.Start(*a.cfg.rc)
	}
	return nil
}

func (a *appsec) stopRC() {
	if a.cfg.rc != nil {
		remoteconfig.Stop()
	}
}

func (a *appsec) registerRCProduct(p string) error {
	if a.cfg.rc == nil {
		return fmt.Errorf("no valid remote configuration client")
	}
	return remoteconfig.RegisterProduct(p)
}

func (a *appsec) registerRCCapability(c remoteconfig.Capability) error {
	if a.cfg.rc == nil {
		return fmt.Errorf("no valid remote configuration client")
	}
	return remoteconfig.RegisterCapability(c)
}

func (a *appsec) unregisterRCCapability(c remoteconfig.Capability) error {
	if a.cfg.rc == nil {
		log.Debug("appsec: Remote config: no valid remote configuration client")
		return nil
	}
	return remoteconfig.UnregisterCapability(c)
}

func (a *appsec) enableRemoteActivation() error {
	if a.cfg.rc == nil {
		return fmt.Errorf("no valid remote configuration client")
	}
	err := a.registerRCProduct(rc.ProductASMFeatures)
	if err != nil {
		return err
	}
	err = a.registerRCCapability(remoteconfig.ASMActivation)
	if err != nil {
		return err
	}
	return remoteconfig.RegisterCallback(a.onRemoteActivation)
}

func (a *appsec) enableRCBlocking() {
	if a.cfg.rc == nil {
		log.Debug("appsec: Remote config: no valid remote configuration client")
		return
	}

	products := []string{rc.ProductASM, rc.ProductASMDD, rc.ProductASMData}
	for _, p := range products {
		if err := a.registerRCProduct(p); err != nil {
			log.Debug("appsec: Remote config: couldn't register product %s: %v", p, err)
		}
	}

	if err := remoteconfig.RegisterCallback(a.onRCRulesUpdate); err != nil {
		log.Debug("appsec: Remote config: couldn't register callback: %v", err)
	}

	if _, isSet := os.LookupEnv(rulesEnvVar); !isSet {
		caps := []remoteconfig.Capability{
			remoteconfig.ASMUserBlocking,
			remoteconfig.ASMRequestBlocking,
			remoteconfig.ASMIPBlocking,
			remoteconfig.ASMDDRules,
			remoteconfig.ASMExclusions,
			remoteconfig.ASMCustomRules,
			remoteconfig.ASMCustomBlockingResponse,
		}
		for _, c := range caps {
			if err := a.registerRCCapability(c); err != nil {
				log.Debug("appsec: Remote config: couldn't register capability %v: %v", c, err)
			}
		}
	}
}

func (a *appsec) disableRCBlocking() {
	if a.cfg.rc == nil {
		return
	}
	caps := []remoteconfig.Capability{
		remoteconfig.ASMDDRules,
		remoteconfig.ASMExclusions,
		remoteconfig.ASMIPBlocking,
		remoteconfig.ASMRequestBlocking,
		remoteconfig.ASMUserBlocking,
		remoteconfig.ASMCustomRules,
	}
	for _, c := range caps {
		if err := a.unregisterRCCapability(c); err != nil {
			log.Debug("appsec: Remote config: couldn't unregister capability %v: %v", c, err)
		}
	}
	if err := remoteconfig.UnregisterCallback(a.onRCRulesUpdate); err != nil {
		log.Debug("appsec: Remote config: couldn't unregister callback: %v", err)
	}
}
