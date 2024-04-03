// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2022 Datadog, Inc.

package timer

import (
	"math"
	"time"
)

const (
	// UnlimitedBudget is a special value for the budget that means the timer has no budget
	UnlimitedBudget = time.Duration(math.MaxInt64)

	// DynamicBudget is a special value for the budget that means the timer should inherit the budget from its parent
	// It is the default value if no options such as WithBudget, WithUnlimitedBudget or WithInheritedBudget are provided
	DynamicBudget = ^time.Duration(0)
)

// DynamicBudgetFunc is a function that is called on all children when a change to the parent happens
type DynamicBudgetFunc func(timer NodeTimer) time.Duration

// config is the configuration of a timer. It can be created through the use of options
type config struct {
	dynamicBudget DynamicBudgetFunc
	// components store all the components of the timer
	components []string
	// budget is the time budget for the timer
	budget time.Duration
}

func newConfig(options ...Option) config {
	config := config{}
	// Make sure the budget is inherited by default
	WithInheritedSumBudget()(&config)
	for _, option := range options {
		option(&config)
	}
	return config
}

// Option are the configuration options for any type of timer. Please read the documentation of said timer to see which options are available
type Option func(*config)

// WithBudget is an Option that sets the budget value
func WithBudget(budget time.Duration) Option {
	return func(c *config) {
		c.budget = budget
	}
}

// WithUnlimitedBudget is an Option that sets the UnlimitedBudget flag on config.budget
func WithUnlimitedBudget() Option {
	return func(c *config) {
		c.budget = UnlimitedBudget
	}
}

// WithInheritedBudget is an Option that sets the DynamicBudget flag on config.budget
func WithInheritedBudget() Option {
	return func(c *config) {
		c.budget = DynamicBudget
		c.dynamicBudget = func(timer NodeTimer) time.Duration {
			return timer.Remaining()
		}
	}
}

// WithInheritedSumBudget is an Option that sets the DynamicBudget flag on config.budget and sets the DynamicBudgetFunc to sum the remaining time of all children
func WithInheritedSumBudget() Option {
	return func(c *config) {
		c.budget = DynamicBudget
		c.dynamicBudget = func(timer NodeTimer) time.Duration {
			return timer.SumRemaining()
		}
	}
}

// WithComponents is an Option that adds multiple components to the components list
func WithComponents(components ...string) Option {
	return func(c *config) {
		c.components = append(c.components, components...)
	}
}
