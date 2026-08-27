package capture

import "testing"

// THE MIRROR IS HAND-MAINTAINED, SO THE PIN IS THE ONLY THING HOLDING IT.
//
// The authority is promptster-backend `packages/contracts/src/cursorVendorUsage.ts`.
// Go cannot import it, and a literal that drifts from the TypeScript side is not
// a compile error on either side — it is a field the backend strips silently,
// which reads to a user as "you're on an older CLI". These tests are the drift
// gate. Every expected value below is transcribed from the contract; if one
// fails, reconcile against that file, not against this test.

func TestCursorVendorIntegrationIDs(t *testing.T) {
	if CursorHookIntegration != "cursor" {
		t.Errorf("hook integration = %q, want %q", CursorHookIntegration, "cursor")
	}
	if CursorVendorIntegration != "cursor-vendor" {
		t.Errorf("vendor integration = %q, want %q", CursorVendorIntegration, "cursor-vendor")
	}
	if CursorVendorUsageScope != "request" {
		t.Errorf("usage scope = %q, want %q", CursorVendorUsageScope, "request")
	}
	if CursorVendorUsagePolicyFlag != "cursorVendorUsage" {
		t.Errorf("policy flag = %q, want %q", CursorVendorUsagePolicyFlag, "cursorVendorUsage")
	}
	if CursorVendorJoinKey != "conversationId" {
		t.Errorf("join key = %q, want %q", CursorVendorJoinKey, "conversationId")
	}
}

// The two rails must stay DISTINCT (the backend's exact-match cost sets key on
// this id, and a per-request rail treated as a turn sum reports its context as
// unknown when it is known) while staying SUBSTRING-COMPATIBLE (the backend's
// family classifiers match `includes("cursor")`, and a rail that falls out of
// them disappears from every family rollup with no error anywhere).
//
// Both properties at once are what lets ONE tool carry TWO rails. A rename that
// satisfies only one of them compiles, passes vet, and is wrong.
func TestCursorVendorIDIsDistinctButSubstringCompatible(t *testing.T) {
	if CursorVendorIntegration == CursorHookIntegration {
		t.Fatal("the vendor rail must not share the hook rail's integration id")
	}
	if !contains(CursorVendorIntegration, CursorHookIntegration) {
		t.Fatalf(
			"vendor id %q must contain %q — the backend's family classifiers match on that substring",
			CursorVendorIntegration, CursorHookIntegration,
		)
	}
}

func TestCursorVendorAbsenceVocabulary(t *testing.T) {
	want := []string{
		"credential_absent",
		"credential_expired",
		"platform_unsupported",
		"collector_not_permitted",
		"vendor_reported_none",
		"vendor_unreachable",
		"vendor_shape_unrecognized",
	}
	got := CursorVendorAbsenceReasons()
	if len(got) != len(want) {
		t.Fatalf("absence vocabulary has %d values, contract has %d",
			len(got), len(want))
	}
	for i, w := range want {
		if string(got[i]) != w {
			t.Errorf("absence reason %d = %q, want %q", i, got[i], w)
		}
	}
	// The three that must stay separable, spelled out because collapsing any two
	// is the whole failure this vocabulary exists to prevent: a collector that
	// was denied, a credential that expired, and a vendor that answered with
	// nothing are three different facts, and only one of them is ours to fix.
	seen := map[CursorVendorAbsenceReason]bool{}
	for _, r := range got {
		if seen[r] {
			t.Errorf("duplicate absence reason %q", r)
		}
		seen[r] = true
	}
}

// A returned copy must not be able to corrupt the vocabulary other callers read.
func TestCursorVendorAbsenceVocabularyIsNotMutableByCallers(t *testing.T) {
	first := CursorVendorAbsenceReasons()
	first[0] = "clobbered"
	if CursorVendorAbsenceReasons()[0] != CursorVendorAbsenceCredentialAbsent {
		t.Fatal("a caller mutated the shared vocabulary; it must be handed out as a copy")
	}
	plats := CursorVendorSupportedPlatforms()
	plats[0] = "clobbered"
	if CursorVendorSupportedPlatforms()[0] != "darwin" {
		t.Fatal("a caller mutated the shared platform list; it must be handed out as a copy")
	}
}

func TestCursorVendorPlatformSupport(t *testing.T) {
	if !CursorVendorPlatformSupported("darwin") {
		t.Error("darwin must be supported in v1")
	}
	// Not silence — the caller emits platform_unsupported. An unsupported
	// platform that emits nothing is indistinguishable from an engineer who
	// stopped using Cursor, which is the absence-vs-zero defect this rail exists
	// to fix rather than to reproduce.
	for _, goos := range []string{"linux", "windows"} {
		if CursorVendorPlatformSupported(goos) {
			t.Errorf("%s must not be supported until the credential store is verified there", goos)
		}
	}
}

func contains(haystack, needle string) bool {
	if len(needle) > len(haystack) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
