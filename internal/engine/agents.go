// Copyright (c) 2026 Michael D Henderson. All rights reserved.

package engine

import "slices"

// Descriptor is one agent this build can play: the key the engine dispatches
// on, and the two strings a game master reads when choosing one.
//
// Key is the durable half. It is written onto a seat and outlives any number of
// releases, so renaming one silently orphans every seat that referenced it —
// treat a key as permanent once it has shipped. Name and Description are the
// display half and may be reworded freely.
type Descriptor struct {
	Key         string
	Name        string
	Description string
}

// agents is the catalogue of every agent this binary can play.
//
// It is an explicit list rather than a registry each agent adds itself to from
// an init function. Self-registration would put mutable package state and
// init-order dependence underneath the one thing the engine must be certain
// about, and this codebase keeps that sort of state out on purpose — the logger
// is injected for the same reason. Adding an agent is a line here next to the
// code that implements it, which is one commit, reviewable in one diff, and
// impossible to get half-done.
//
// Keys must be unique, lowercase, and match the format 0003_agent_key.sql
// enforces on the column. Nothing checks that mechanically, which is the price
// of keeping the catalogue a plain list: it is short, and it is reviewed.
// The order here is the order a game master sees.
var agents = []Descriptor{
	{
		Key:  "passive",
		Name: "Passive",
		Description: "Holds what it starts with and issues no orders. " +
			"The neutral opponent, and the baseline every other agent is measured against.",
	},
}

// Agents returns every agent this build can play, in display order.
//
// It returns a copy: the catalogue is fixed at build time and a caller that
// could reorder or extend it would be changing what the engine is able to do.
func Agents() []Descriptor {
	return slices.Clone(agents)
}

// AgentByKey looks an agent up by key, reporting whether this build has one.
//
// This is the authority on whether a seat is playable. A seat's agent_key comes
// from a database that may have been written by a different release, so the
// answer is a fact about this binary and can only be asked here — which is why
// the set of valid keys is deliberately not a database constraint.
func AgentByKey(key string) (Descriptor, bool) {
	for _, agent := range agents {
		if agent.Key == key {
			return agent, true
		}
	}
	return Descriptor{}, false
}

// AgentKeys returns just the keys, for an error message that tells a caller
// what there was to choose between.
func AgentKeys() []string {
	keys := make([]string, 0, len(agents))
	for _, agent := range agents {
		keys = append(keys, agent.Key)
	}
	return keys
}
