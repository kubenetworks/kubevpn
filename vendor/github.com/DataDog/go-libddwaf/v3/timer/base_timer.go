// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2022 Datadog, Inc.

package timer

import (
	"errors"
	"time"
)

// baseTimer is the type used for all the timers that won't have children
// It's implementation is more lightweight than nodeTimer and can be used as a standalone timer using NewTimer
type baseTimer struct {

	// config is the configuration of the timer
	config config

	clock

	// start is the time when the timer was started
	start time.Time

	// parent is the parent timer. It is used to progate the stop of the timer to the parent timer and get the remaining time in case the budget has to be inherited.
	parent NodeTimer

	// componentName is the name of the component of the timer. It is used to store the time spent in the component and to propagate the stop of the timer to the parent timer.
	componentName string

	// spent is the time spent on the timer, set after calling stop
	spent time.Duration
}

var _ Timer = (*baseTimer)(nil)

// NewTimer creates a new Timer with the given options. You have to specify either the option WithBudget or WithUnlimitedBudget.
func NewTimer(options ...Option) (Timer, error) {
	config := newConfig(options...)
	if config.budget == DynamicBudget {
		return nil, errors.New("root timer cannot inherit parent budget, please provide a budget using timer.WithBudget() or timer.WithUnlimitedBudget()")
	}

	if len(config.components) > 0 {
		return nil, errors.New("NewTimer: timer that have components must use NewTreeTimer()")
	}

	return &baseTimer{
		config: config,
		clock:  newTimeCache(),
	}, nil
}

func (timer *baseTimer) Start() time.Time {
	// already started before
	if timer.start != (time.Time{}) {
		return timer.start
	}

	if timer.config.budget == DynamicBudget && timer.parent != nil {
		timer.config.budget = timer.config.dynamicBudget(timer.parent)
	}

	timer.start = timer.now()
	return timer.start
}

func (timer *baseTimer) Spent() time.Duration {
	// timer was never started
	if timer.start == (time.Time{}) {
		return 0
	}

	// timer was already stopped
	if timer.spent != 0 {
		return timer.spent
	}

	return time.Since(timer.start)
}

func (timer *baseTimer) Remaining() time.Duration {
	if timer.config.budget == UnlimitedBudget {
		return UnlimitedBudget
	}

	remaining := timer.config.budget - timer.Spent()
	if remaining < 0 {
		return 0
	}

	return remaining
}

func (timer *baseTimer) Exhausted() bool {
	if timer.config.budget == UnlimitedBudget {
		return false
	}

	return timer.Spent() > timer.config.budget
}

func (timer *baseTimer) Stop() time.Duration {
	// If the current timer has already stopped, return the current spent time
	if timer.spent != 0 {
		return timer.spent
	}

	timer.spent = timer.Spent()
	if timer.parent != nil {
		timer.parent.childStopped(timer.componentName, timer.spent)
	}

	return timer.spent
}

func (timer *baseTimer) Timed(timedFunc func(timer Timer)) (spent time.Duration) {
	timer.Start()
	defer func() {
		spent = timer.Stop()
	}()

	timedFunc(timer)
	return
}
