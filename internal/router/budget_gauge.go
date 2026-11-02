/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package router

import (
	"sync"
	"time"
)

// budgetGaugeInterval is the minimum spacing between two full republishes of
// the token budget gauge. A republish walks every scope key under the budget
// store's lock (up to maxTeamKeys team keys), so it is paced like the idle-key
// sweep rather than run per request. Usage is kept in bucketDuration slices,
// so publishing more often would show little that is new.
const budgetGaugeInterval = bucketDuration

// gaugePublisher runs a full republish off the request path, at most once per
// interval, and always after the last request that asked for one: a request
// that arrives while no run is scheduled schedules one, immediately when the
// previous run started at least an interval ago and otherwise at that run's
// start plus the interval. A run clears its scheduled flag before it publishes,
// so a request racing a run either lands in that run's snapshot or schedules
// the next run. No request is left unpublished, and a burst inside one
// interval costs one run.
type gaugePublisher struct {
	publish func()

	mu sync.Mutex
	// interval is the minimum spacing between run starts.
	interval time.Duration
	// pending is set while a run is scheduled and has not yet started.
	pending bool
	// last is when the most recent run started.
	last   time.Time
	timer  *time.Timer
	closed bool
	// runs tracks scheduled and running runs, so close can wait for them.
	runs sync.WaitGroup
}

func newGaugePublisher(interval time.Duration, publish func()) *gaugePublisher {
	return &gaugePublisher{publish: publish, interval: interval}
}

// request asks for a republish. It takes one short lock and at most one timer
// per interval, and never publishes on the caller's goroutine.
func (g *gaugePublisher) request() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pending || g.closed {
		return
	}
	g.pending = true
	g.runs.Add(1)
	g.timer = time.AfterFunc(max(g.interval-time.Since(g.last), 0), g.run)
}

// run is the scheduled republish.
func (g *gaugePublisher) run() {
	defer g.runs.Done()
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return
	}
	g.pending = false
	g.last = time.Now()
	g.mu.Unlock()
	g.publish()
}

// close cancels a scheduled run, refuses new ones, and waits for a run in
// flight to finish, so nothing publishes after it returns.
func (g *gaugePublisher) close() {
	g.mu.Lock()
	g.closed = true
	if g.timer != nil && g.timer.Stop() {
		// The run never started, so it will not call Done itself.
		g.runs.Done()
	}
	g.mu.Unlock()
	g.runs.Wait()
}
