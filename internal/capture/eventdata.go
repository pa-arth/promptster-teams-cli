package capture

import (
	"encoding/json"

	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// eventDataMap converts a typed payload struct into the map[string]interface{}
// shape that event.Event.Data must hold.
//
// WHY THIS EXISTS — a trap worth reading before you write a new emitter:
//
// event.Event.Data is an interface{}, and redact.ProjectEvent (the choke point
// every event funnels through before it is signed, buffered, or sent) reads it
// with `data, ok := e.Data.(map[string]interface{})` and default-denies when
// !ok. A STRUCT value in an interface{} never type-asserts to a map, however
// map-shaped its json tags are — so assigning a payload struct straight to
// e.Data silently replaces the whole payload with {} on the wire. It compiles,
// it signs, it ingests 200 OK, and every field is gone.
//
// Most emitters are immune by accident: internal/normalize/* builds Data by
// unmarshalling JSON, so it already holds a map. Emitters that assemble a typed
// struct (config_census, presence) MUST route it through here.
//
// The conversion round-trips through encoding/json so the resulting keys are
// exactly the struct's json tags — which is what the projector's allowlist keys
// against — and so nested arrays-of-structs land as []interface{} of maps,
// which is the only shape the projector's array-element allowlist can walk.
//
// Best-effort, matching the rest of the capture path: a payload that will not
// marshal is logged under debug and yields an empty map (the same payload the
// projector would have produced anyway). It never panics and never blocks the
// event — a heartbeat with a thin payload still proves the device is alive.
func eventDataMap(payload interface{}) map[string]interface{} {
	raw, err := json.Marshal(payload)
	if err != nil {
		state.HookDebugf("event data marshal error (payload dropped): %v", err)
		return map[string]interface{}{}
	}
	var data map[string]interface{}
	if err := json.Unmarshal(raw, &data); err != nil {
		state.HookDebugf("event data unmarshal error (payload dropped): %v", err)
		return map[string]interface{}{}
	}
	return data
}
