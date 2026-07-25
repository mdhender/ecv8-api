// Copyright (c) 2026 Michael D Henderson. All rights reserved.

// These tests guard the catalogue's invariants rather than its contents. Which
// agents exist is expected to change with every release; that keys are unique,
// well-formed, and storable is what must not.
package engine

import (
	"regexp"
	"testing"
)

// keyPattern is the format 0003_agent_key.sql enforces on the column, written
// as a regexp. A key that fails here would be accepted by the registry and
// rejected by SQLite at the moment a game master tried to seat it, which is the
// worst place to find out.
var keyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

func TestAgentKeysAreStorable(t *testing.T) {
	for _, agent := range Agents() {
		if !keyPattern.MatchString(agent.Key) {
			t.Errorf("agent key %q does not match %s; the database CHECK would reject it",
				agent.Key, keyPattern)
		}
		if len(agent.Key) > 40 {
			t.Errorf("agent key %q is %d bytes, want at most 40", agent.Key, len(agent.Key))
		}
	}
}

// A duplicate key would make AgentByKey return whichever came first and leave
// the other agent permanently unreachable, with nothing to say so.
func TestAgentKeysAreUnique(t *testing.T) {
	seen := make(map[string]string)
	for _, agent := range Agents() {
		if first, ok := seen[agent.Key]; ok {
			t.Errorf("key %q is used by both %q and %q", agent.Key, first, agent.Name)
			continue
		}
		seen[agent.Key] = agent.Name
	}
}

// Name and Description are what a game master reads. An empty one is not a
// stylistic problem; it is a blank row in the picker.
func TestAgentsAreDescribed(t *testing.T) {
	for _, agent := range Agents() {
		if agent.Name == "" {
			t.Errorf("agent %q has no name", agent.Key)
		}
		if agent.Description == "" {
			t.Errorf("agent %q has no description", agent.Key)
		}
	}
}

func TestAgentByKey(t *testing.T) {
	catalogue := Agents()
	if len(catalogue) == 0 {
		t.Fatal("the catalogue is empty; a build that can play nothing cannot seat an agent")
	}

	want := catalogue[0]
	got, ok := AgentByKey(want.Key)
	if !ok {
		t.Fatalf("AgentByKey(%q) reported unknown, but it is in the catalogue", want.Key)
	}
	if got != want {
		t.Errorf("AgentByKey(%q) = %+v, want %+v", want.Key, got, want)
	}
}

// This is the case the whole design exists for: a seat written by a build that
// had an agent this one does not. It must be answerable, not fatal.
func TestAgentByKeyRejectsUnknown(t *testing.T) {
	for _, key := range []string{"", "no-such-agent", "PASSIVE", " passive"} {
		if _, ok := AgentByKey(key); ok {
			t.Errorf("AgentByKey(%q) reported known", key)
		}
	}
}

// The lookup is case-sensitive and does not trim, because the stored key is
// already normalised by the handler and the database CHECK. A lookup that
// papered over either would let two spellings of one key into the database.
func TestAgentByKeyIsExact(t *testing.T) {
	first := Agents()[0].Key
	if _, ok := AgentByKey(" " + first); ok {
		t.Errorf("AgentByKey tolerated leading whitespace on %q", first)
	}
}

// Agents returns a copy. A caller that could reorder or truncate the catalogue
// would be changing what this build is able to play.
func TestAgentsReturnsACopy(t *testing.T) {
	first := Agents()
	if len(first) == 0 {
		t.Fatal("the catalogue is empty")
	}
	original := first[0]

	first[0] = Descriptor{Key: "mutated", Name: "Mutated"}
	if second := Agents(); second[0] != original {
		t.Errorf("mutating the result changed the catalogue: got %+v, want %+v", second[0], original)
	}
	if _, ok := AgentByKey("mutated"); ok {
		t.Error("mutating the result made a new key resolvable")
	}
}

// AgentKeys feeds the "choose one of" message in a 422, so it must agree with
// the catalogue exactly — a stale or partial list would tell a caller to use a
// key that does not work, or hide one that does.
func TestAgentKeysMatchesCatalogue(t *testing.T) {
	catalogue := Agents()
	keys := AgentKeys()
	if len(keys) != len(catalogue) {
		t.Fatalf("AgentKeys returned %d keys, want %d", len(keys), len(catalogue))
	}
	for i := range catalogue {
		if keys[i] != catalogue[i].Key {
			t.Errorf("AgentKeys()[%d] = %q, want %q", i, keys[i], catalogue[i].Key)
		}
	}
}
