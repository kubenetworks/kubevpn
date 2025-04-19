// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2022 Datadog, Inc.

package timer

import (
	"time"
)

// Timer is the default interface for all timers. NewTimer will provide you with a Timer.
// Keep in mind that they are NOT thread-safe and once Stop() is called, the Timer cannot be restarted.
type Timer interface {
	// Start starts the timer and returns the start time.
	// If the timer was already started, it returns the previous start time.
	// If the timer was started without specifying a budget, it will inherit the budget from its parent when calling Start().
	// if the timer has no parent and no budget was specified, the call creating the timer (either NewTreeTimer or NewTimer) will return an error asking to specify a budget (which can be unlimited).
	// Start is NOT thread-safe
	Start() time.Time

	// Stop ends the timer and returns the time spent on the timer as Spent() would.
	// Stop will trigger the computation of sum timers if the timer is part of a tree. See NodeTimer for more information.
	// Stop is NOT thread-safe
	Stop() time.Duration

	// Spent returns the current time spent between Start() and Stop() or between Start() and now if the timer is still running.
	// Spent is thread-safe
	Spent() time.Duration

	// Remaining returns the time remaining before the timer reaches its budget. (budget - Spent())
	// It returns 0 if the timer is exhausted. Remaining may never return a value below zero.
	// Remaining only makes sense if the timer has a budget. If the timer has no budget, it returns the special value UnlimitedBudget.
	// Remaining is thread-safe
	Remaining() time.Duration

	// Exhausted returns true if the timer spent in the timer is greater than the budget. (Spent() > budget)
	// Exhausted may return true only in case the time has a budget. If the timer has n, it returns false.
	// Exhausted is thread-safe
	Exhausted() bool

	// Timed is a convenience function that starts the timer, calls the provided function and stops the timer.
	// Timed is panic-safe and will stop the timer even if the function panics.
	// Timed is NOT thread-safe
	Timed(timedFunc func(timer Timer)) time.Duration
}

// SumTimer is a sub-interface for timers capable of having children and making the sum of their time spent.
// NewTreeTimer will provide you with a timer supporting this interface
type SumTimer interface {
	// SumSpent returns the sum of the time spent in each component of the timer.
	// SumSpent is thread-safe
	SumSpent() time.Duration

	// SumRemaining returns the sum of the time remaining in each component of the timer.
	// SumRemaining returns UnlimitedBudget if the timer has no budget. (UnlimitedBudget)
	// SumRemaining is thread-safe
	SumRemaining() time.Duration

	// SumExhausted returns true if the sum of the time spent in each component of the timer is greater than the budget.
	// SumExhausted returns false if the timer has no budget. (UnlimitedBudget)
	// SumExhausted is thread-safe
	SumExhausted() bool
}

// NodeTimer is the interface for tree timers. NewTreeTimer will provide you with a NodeTimer.
// NodeTimer can have children (NodeTimer or Timer) and will compute the sum of their spent time each time a children timer calls its Stop() method.
// To add children to a NodeTimer, you have to specify component names when creating the timer with the WithComponent and WithComponents options.
// The component names must be unique and cannot be empty. The component names are used to identify the children timers.
// The returned timer can now create children timers using the NewNode and NewLeaf functions using the names provided when creating the parent timer.
// Multiple timers from the same component can be used in parallel and will be summed together.
// In parallel to that, NodeTimer can have their own wall time timer and budget that will apply to the sum of their children and their own timer.
// The following functions are the same as the Timer interface but works using the sum of the children timers:
// - SumSpent() -> Spent()
// - SumRemaining() -> Remaining()
// - SumExhausted() -> Exhausted()
// Keep in mind that the timer itself (only Start and Stop) is NOT thread-safe and once Stop() is called, the NodeTimer cannot be restarted.
type NodeTimer interface {
	Timer
	SumTimer

	// NewNode creates a new NodeTimer with the given name and options. The given name must match one of the component name of the parent timer.
	// A node timer is required to have at least one component. If no component is provided, it will return an error asking you to use NewLeaf instead.
	// If no budget is provided, it will inherit the budget from its parent when calling Start().
	// NewNode is thread-safe
	NewNode(name string, options ...Option) (NodeTimer, error)

	// NewLeaf creates a new Timer with the given name and options. The given name must match one of the component name of the parent timer.
	// A leaf timer is forbidden to have components. If a component is provided, it will return an error asking you to use NewNode instead.
	// If no budget is provided, it will inherit the budget from its parent when calling Start().
	// NewLeaf is thread-safe
	NewLeaf(name string, options ...Option) (Timer, error)

	// MustLeaf creates a new Timer with the given name and options.  The given name must match one of the component name of the parent timer.
	// MustLeaf wraps a call to NewLeaf but will panic if the error is not nil.
	// MustLeaf is thread-safe
	MustLeaf(name string, options ...Option) Timer

	// AddTime adds the given duration to the component of the timer with the given name.
	// AddTime is thread-safe
	AddTime(name string, duration time.Duration)

	// Stats returns a map of the time spent in each component of the timer.
	// Stats is thread-safe
	Stats() map[string]time.Duration

	// childStarted is used to propagate the start of a child timer to the parent timer through the whole tree.
	childStarted()

	// childStopped is used to propagate the time spent in a child timer to the parent timer through the whole tree.
	childStopped(componentName string, duration time.Duration)

	// now is a convenience wrapper to swap the time.Now() function for testing and performance purposes.
	now() time.Time
}
