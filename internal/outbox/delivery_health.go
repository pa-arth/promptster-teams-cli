package outbox

import (
	"errors"
	"net"
	"os"
	"sync"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/ingest"
)

// DeliveryHealth contains only bounded transport metadata, never event data or
// response bodies. The oldest retrying lane wins so a healthy sibling cannot
// hide a stalled queue.
type DeliveryHealth struct {
	State        string `json:"deliveryState"`
	Lane         string `json:"deliveryLane,omitempty"`
	FailureClass string `json:"deliveryFailureClass,omitempty"`
	HTTPStatus   int    `json:"deliveryHTTPStatus,omitempty"`
	MemberStatus int    `json:"deliveryMemberStatus,omitempty"`
	FailureAt    string `json:"deliveryFailureAt,omitempty"`
}

var deliveryHealthMu sync.RWMutex
var deliveryHealthByLane = map[string]DeliveryHealth{}

// deliveryOutcomes counts recorded outcomes PER LANE, so a waiter can tell "this
// lane's drain answered since I started waiting" from a stale entry. Guarded by
// deliveryHealthMu.
var deliveryOutcomes = map[string]uint64{}

// AwaitDeliveryOutcome blocks until every non-empty lane's drain has recorded an
// outcome since the call, timeout passes, or stop closes. It reports false only
// when stop closed first.
//
// It exists for the STARTUP beat. RunTeamsWatch starts the heartbeat before the
// watchers that start the drain, so a beat built immediately describes a queue
// nobody has tried yet: "unknown" beside whatever piled up while the daemon was
// down. The backend reads "unknown" as "no health data" and pages on that queue's
// age, while the drain empties it a second later. Waiting for the first answer
// makes the first beat a measurement.
//
// EVERY non-empty lane, not the first to answer: live succeeding while a backlog
// sits untried in backfill would otherwise beat "ok" for the whole queue. The
// bound keeps a daemon whose drain never starts honest: it still beats, and
// "unknown" is then the truth.
func AwaitDeliveryOutcome(stop <-chan struct{}, timeout time.Duration) bool {
	lanes := []Lane{LaneLive(), LaneBackfill()}
	deliveryHealthMu.RLock()
	start := map[string]uint64{}
	for _, lane := range lanes {
		start[lane.Name] = deliveryOutcomes[lane.Name]
	}
	deliveryHealthMu.RUnlock()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		settled := true
		for _, lane := range lanes {
			deliveryHealthMu.RLock()
			answered := deliveryOutcomes[lane.Name] != start[lane.Name]
			deliveryHealthMu.RUnlock()
			if !answered && !laneEmpty(lane) { // file IO outside the lock
				settled = false
			}
		}
		if settled {
			return true
		}
		select {
		case <-stop:
			return false
		case <-deadline.C:
			return true
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// laneEmpty is true only when the lane PROVABLY holds nothing undelivered: no
// file, or a cursor at its end. Any read error is "not empty" — an unreadable
// queue is exactly the one whose drain is about to report a local failure, and
// PendingStateNow's zero-on-error would release the wait before it could.
func laneEmpty(lane Lane) bool {
	fi, err := os.Stat(lane.path())
	if os.IsNotExist(err) {
		return true
	}
	if err != nil {
		return false
	}
	cursor := readCursor(lane)
	if cursor > fi.Size() {
		cursor = 0 // compacted underneath us, as pendingStateIn reads it
	}
	return fi.Size() == cursor
}

func DeliveryHealthNow() DeliveryHealth {
	deliveryHealthMu.RLock()
	defer deliveryHealthMu.RUnlock()
	var oldest DeliveryHealth
	for _, lane := range []string{"live", "backfill"} {
		item := deliveryHealthByLane[lane]
		if item.State == "retrying" {
			itemAt, _ := time.Parse(time.RFC3339Nano, item.FailureAt)
			oldestAt, _ := time.Parse(time.RFC3339Nano, oldest.FailureAt)
			if oldest.State != "retrying" || itemAt.Before(oldestAt) {
				oldest = item
			}
		}
	}
	if oldest.State == "retrying" {
		return oldest
	}
	if deliveryHealthByLane["live"].State == "ok" || deliveryHealthByLane["backfill"].State == "ok" {
		return DeliveryHealth{State: "ok"}
	}
	return DeliveryHealth{State: "unknown"}
}

// memberStatus -1 means a 207 omitted the head result row.
func recordDeliveryFailure(lane string, err error, memberStatus int) {
	class := "transport"
	status := ingest.HTTPStatus(err)
	if memberStatus != 0 {
		class = "member"
	} else if status != 0 {
		class = "http"
	} else {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			class = "timeout"
		}
	}
	if memberStatus < 0 {
		memberStatus = 0
	}
	recordRetrying(lane, class, status, memberStatus)
}

// recordLocalDeliveryFailure marks a lane whose head cannot advance for a reason
// on this machine (the cursor cannot be written, the queue cannot be opened).
// Without it that drain recorded nothing, or "ok" for a head it kept re-sending.
func recordLocalDeliveryFailure(lane string) {
	recordRetrying(lane, "local", 0, 0)
}

func recordRetrying(lane, class string, status, memberStatus int) {
	deliveryHealthMu.Lock()
	defer deliveryHealthMu.Unlock()
	deliveryOutcomes[lane]++
	prior := deliveryHealthByLane[lane]
	failureAt := time.Now().UTC().Format(time.RFC3339Nano)
	if prior.State == "retrying" {
		failureAt = prior.FailureAt
	}
	deliveryHealthByLane[lane] = DeliveryHealth{
		State: "retrying", Lane: lane, FailureClass: class,
		HTTPStatus: status, MemberStatus: memberStatus,
		FailureAt: failureAt,
	}
}

// recordDeliverySuccess means the lane's HEAD ADVANCED — the cursor write landed.
// Not "the backend answered 2xx": a drain that cannot persist its cursor re-sends
// an accepted head forever, and calling that ok is how a frozen queue beat "ok".
func recordDeliverySuccess(lane string) {
	deliveryHealthMu.Lock()
	defer deliveryHealthMu.Unlock()
	deliveryOutcomes[lane]++
	deliveryHealthByLane[lane] = DeliveryHealth{State: "ok"}
}
