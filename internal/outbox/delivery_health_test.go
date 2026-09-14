package outbox

import (
	"context"
	"errors"
	"testing"
)

func TestDeliveryHealthReportsOnlyBoundedFailureMetadata(t *testing.T) {
	t.Cleanup(func() { recordDeliverySuccess("live"); recordDeliverySuccess("backfill") })
	recordDeliveryFailure("backfill", context.DeadlineExceeded, 0)
	got := DeliveryHealthNow()
	if got.State != "retrying" || got.Lane != "backfill" || got.FailureClass != "timeout" || got.FailureAt == "" {
		t.Fatalf("timeout health = %+v", got)
	}

	recordDeliverySuccess("backfill")
	recordDeliveryFailure("live", errors.New("sensitive response body"), 500)
	got = DeliveryHealthNow()
	if got.Lane != "live" || got.FailureClass != "member" || got.MemberStatus != 500 || got.HTTPStatus != 0 {
		t.Fatalf("member health = %+v", got)
	}
	firstFailureAt := got.FailureAt
	recordDeliveryFailure("live", errors.New("another response body"), 500)
	if got := DeliveryHealthNow(); got.FailureAt != firstFailureAt {
		t.Fatalf("failure start changed on retry: %+v", got)
	}
	recordDeliverySuccess("backfill")
	if got := DeliveryHealthNow(); got.State != "retrying" || got.Lane != "live" {
		t.Fatalf("backfill success hid live stall: %+v", got)
	}
	recordDeliverySuccess("live")
	if got := DeliveryHealthNow(); got.State != "ok" || got.FailureClass != "" || got.MemberStatus != 0 {
		t.Fatalf("recovered health = %+v", got)
	}

}
