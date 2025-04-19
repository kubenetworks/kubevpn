// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2022 Datadog, Inc.

package timer

import (
	"errors"
	"fmt"
	"time"
)

// nodeTimer is the type used for all the timers that can (and will) have children
type nodeTimer struct {
	baseTimer
	components
}

var _ NodeTimer = (*nodeTimer)(nil)

// NewTreeTimer creates a new Timer with the given options. You have to specify either the option WithBudget or WithUnlimitedBudget and at least one component using the WithComponents option.
func NewTreeTimer(options ...Option) (NodeTimer, error) {
	config := newConfig(options...)
	if config.budget == DynamicBudget {
		return nil, errors.New("root timer cannot inherit parent budget, please provide a budget using timer.WithBudget() or timer.WithUnlimitedBudget()")
	}

	if len(config.components) == 0 {
		return nil, errors.New("NewTreeTimer: tree timer must have at least one component, otherwise use NewTimer()")
	}

	return &nodeTimer{
		baseTimer: baseTimer{
			config: config,
			clock:  newTimeCache(),
		},
		components: newComponents(config.components),
	}, nil
}

func (timer *nodeTimer) NewNode(name string, options ...Option) (NodeTimer, error) {
	config := newConfig(options...)
	if len(config.components) == 0 {
		return nil, errors.New("NewNode: node timer must have at least one component, otherwise use NewLeaf()")
	}

	_, ok := timer.components.lookup[name]
	if !ok {
		return nil, fmt.Errorf("NewNode: component %s not found", name)
	}

	return &nodeTimer{
		baseTimer: baseTimer{
			config:        config,
			clock:         timer.clock,
			parent:        timer,
			componentName: name,
		},
		components: newComponents(config.components),
	}, nil
}

func (timer *nodeTimer) NewLeaf(name string, options ...Option) (Timer, error) {
	config := newConfig(options...)
	if len(config.components) != 0 {
		return nil, errors.New("NewLeaf: leaf timer cannot have components, otherwise use NewNode()")
	}

	_, ok := timer.components.lookup[name]
	if !ok {
		return nil, fmt.Errorf("NewLeaf: component %s not found", name)
	}

	return &baseTimer{
		clock:         timer.clock,
		config:        config,
		componentName: name,
		parent:        timer,
	}, nil
}

func (timer *nodeTimer) MustLeaf(name string, options ...Option) Timer {
	leaf, err := timer.NewLeaf(name, options...)
	if err != nil {
		panic(err)
	}
	return leaf
}

func (timer *nodeTimer) childStarted() {}

func (timer *nodeTimer) childStopped(componentName string, duration time.Duration) {
	timer.components.lookup[componentName].Add(int64(duration))
	if timer.parent == nil {
		return
	}

	timer.parent.childStopped(timer.componentName, duration)
}

func (timer *nodeTimer) AddTime(name string, duration time.Duration) {
	value, ok := timer.components.lookup[name]
	if !ok {
		return
	}

	value.Add(int64(duration))
}

func (timer *nodeTimer) Stats() map[string]time.Duration {
	stats := make(map[string]time.Duration, len(timer.components.lookup))
	for name, component := range timer.components.lookup {
		stats[name] = time.Duration(component.Load())
	}

	return stats
}

func (timer *nodeTimer) SumSpent() time.Duration {
	var sum time.Duration
	for _, component := range timer.components.lookup {
		sum += time.Duration(component.Load())
	}
	return sum
}

func (timer *nodeTimer) SumRemaining() time.Duration {
	if timer.config.budget == UnlimitedBudget {
		return UnlimitedBudget
	}

	remaining := timer.config.budget - timer.SumSpent()
	if remaining < 0 {
		return 0
	}

	return remaining
}

func (timer *nodeTimer) SumExhausted() bool {
	if timer.config.budget == UnlimitedBudget {
		return false
	}

	return timer.SumSpent() > timer.config.budget
}
