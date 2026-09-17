package outbox

import (
	"errors"
	"net"
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

// deliveryOutcomes counts every recorded outcome, so a waiter can tell "the
// drain answered since I started waiting" from a stale entry. Guarded by
// deliveryHealthMu.
var deliveryOutcomes uint64

// AwaitDeliveryOutcome blocks until this process's drain records an outcome, the
// queue is empty, or timeout passes — whichever is first.
//
// It exists for the STARTUP beat. RunTeamsWatch starts the heartbeat before the
// watchers that start the drain, so a beat built immediately describes a queue
// nobody has tried yet: "unknown" beside whatever piled up while the daemon was
// down. The backend reads "unknown" as "no health data" and pages on that queue's
// age, while the drain empties it a second later. Waiting for the first answer
// makes the first beat a measurement. The bound keeps a daemon whose drain never
// starts honest: it still beats, and "unknown" is then the truth.
func AwaitDeliveryOutcome(timeout time.Duration) {
	deliveryHealthMu.RLock()
	start := deliveryOutcomes
	deliveryHealthMu.RUnlock()
	deadline := time.Now().Add(timeout)
	for {
		deliveryHealthMu.RLock()
		answered := deliveryOutcomes != start
		deliveryHealthMu.RUnlock()
		if answered || PendingStateNow().Count == 0 || !time.Now().Before(deadline) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
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
	deliveryOutcomes++
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
	deliveryOutcomes++
	deliveryHealthByLane[lane] = DeliveryHealth{State: "ok"}
}
