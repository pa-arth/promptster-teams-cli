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

func DeliveryHealthNow() DeliveryHealth {
	deliveryHealthMu.RLock()
	defer deliveryHealthMu.RUnlock()
	var oldest DeliveryHealth
	for _, lane := range []string{"live", "backfill"} {
		item := deliveryHealthByLane[lane]
		if item.State == "retrying" && (oldest.State != "retrying" || item.FailureAt < oldest.FailureAt) {
			oldest = item
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
	deliveryHealthMu.Lock()
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
	deliveryHealthMu.Unlock()
}

func recordDeliverySuccess(lane string) {
	deliveryHealthMu.Lock()
	deliveryHealthByLane[lane] = DeliveryHealth{State: "ok"}
	deliveryHealthMu.Unlock()
}
