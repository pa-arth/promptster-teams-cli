package normalize

import "github.com/pa-arth/promptster-teams-cli/internal/event"

// stampLaneID marks every event with the concurrent agent process that produced
// it, so within-session parallelism is computable downstream.
//
// WHY EVERY EVENT AND NOT A START/END PAIR. A lane's interval has to come from
// the first and last moment it was WORKING. Boundary events are the obvious
// alternative and they are not reliable: across 58,688 events of real capture
// there were 2 session_start and 2 session_end. A lane derived from boundaries
// would be absent for almost every lane that ran.
//
// WHY ONE HELPER AND NOT ONE PER RAIL. The three rails learn their lane from
// three unrelated places — Cursor from the child transcript's name, Codex from
// session_meta's thread id, Claude from the sidechain's own agent id — but a
// consumer comparing tools has to read one field with one meaning. Three copies
// of this loop is three chances for one rail to stamp a session id instead, and
// a session id in this field does not read as wrong: it reads as a tool that
// runs exactly one lane.
//
// An empty laneID stamps NOTHING, on purpose. Absent means "this rail did not
// tell us", which is a state the consumer contract already distinguishes from
// "one lane"; a placeholder would merge every unidentifiable lane into one.
func stampLaneID(events []event.Event, laneID string) []event.Event {
	if laneID == "" {
		return events
	}
	for i := range events {
		// Data is `interface{}` on the envelope, so a type assertion is the only
		// honest way in. A non-map Data is left ALONE rather than replaced: the
		// lane id is worth less than whatever a future kind chose to put there,
		// and the projector already logs a non-map payload as an emitter bug.
		data, ok := events[i].Data.(map[string]interface{})
		if !ok {
			if events[i].Data != nil {
				continue
			}
			data = map[string]interface{}{}
		}
		data["agentId"] = laneID
		events[i].Data = data
	}
	return events
}
