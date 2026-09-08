// Package main — reactive layer compatibility aliases.
//
// The reactive layer's pure functions (prompt building, decision parsing,
// trigger detection, dedupe helpers) live in pkg/prompt. This file keeps
// type and constant aliases so the rest of the main package (reactive_runner,
// main.go, tests) can refer to them without the prompt. prefix. Function
// calls go directly to prompt.Xxx.
//
// See pkg/prompt/reactive.go for the pure implementations and
// pkg/prompt/types.go for the shared ReactiveTrigger / ReactiveInput types.

package main

import "github.com/AgentTown/agenttown-mcp/pkg/prompt"

// ─── Type aliases (zero-cost, fully interchangeable with prompt package) ───

// ReactiveTrigger identifies what triggered a reactive evaluation.
// Used for dedupe, logging, and as part of ReactiveInput.
type ReactiveTrigger = prompt.ReactiveTrigger

// ReactiveInput aggregates all inputs needed by BuildReactive.
type ReactiveInput = prompt.ReactiveInput

// ReactionKind enumerates reactive layer decision types.
type ReactionKind = prompt.ReactionKind

// ReactiveDecision is the JSON decision expected from Ollama.
type ReactiveDecision = prompt.ReactiveDecision

// ─── Trigger constants ───

const (
	TriggerZoneChange    = prompt.TriggerZoneChange    // NPC entered a new zone
	TriggerNewObject     = prompt.TriggerNewObject     // nearby_objects gained a new object
	TriggerEventNotify   = prompt.TriggerEventNotify   // received event_notification
	TriggerPhysicalAlert = prompt.TriggerPhysicalAlert // physical state crossed alert threshold
	TriggerActionDone    = prompt.TriggerActionDone    // action_completed, natural evaluation point
	TriggerPeriodic      = prompt.TriggerPeriodic      // periodic trigger: force evaluation every N perceptions
)

// ─── Reaction constants ───

const (
	ReactionContinue = prompt.ReactionContinue // do not interrupt; let current action proceed
	ReactionObserve  = prompt.ReactionObserve  // do not interrupt; record event for tactical layer
	ReactionReplan   = prompt.ReactionReplan   // trigger tactical layer to replan the whole slot
)

// ─── Dedupe windows and intervals (aliased for reactive_runner.go) ───

const (
	// reactiveDedupeWindow is the dedupe window for event-type triggers
	// (zone/objects/physical/event). Matches doc decision 3.
	reactiveDedupeWindow = prompt.ReactiveDedupeWindow

	// reactivePeriodicDedupeWindow is the dedupe window for periodic triggers.
	reactivePeriodicDedupeWindow = prompt.ReactivePeriodicDedupeWindow

	// replanDedupeGameMinutes limits reaction=replan frequency: at most 1 per
	// game hour. See prompt.ReplanDedupeGameMinutes for rationale.
	replanDedupeGameMinutes = prompt.ReplanDedupeGameMinutes

	// periodicTriggerInterval is the perception count interval for periodic
	// forced triggers. See prompt.PeriodicTriggerInterval.
	periodicTriggerInterval = prompt.PeriodicTriggerInterval
)
