package capture

import (
	"bytes"
	"io"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func response(body string) *http.Response {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewBufferString(body)), Header: make(http.Header)}
}

func TestGetTeamsRefusesTeamAndSendsCredentialOnlyInHeader(t *testing.T) {
	secret := "cursor-secret-mutation-canary"
	client := &cursorVendorClient{base: "https://api2.cursor.sh", http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != cursorMethodTeams {
			t.Fatalf("path=%s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte(secret)) {
			t.Fatal("credential entered request body")
		}
		if r.Header.Get("Authorization") != "Bearer "+secret {
			t.Fatal("missing bearer header")
		}
		return response(`{"teams":[{"id":"1","name":"Acme"}]}`), nil
	})}}
	onTeam, err := client.cursorAccountIsOnTeam(cursorCredential{token: secret})
	if err != nil || !onTeam {
		t.Fatalf("onTeam=%v err=%v", onTeam, err)
	}
}

func TestSnapshotIsDeterministicAndCompletionIsLastContract(t *testing.T) {
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(31 * 24 * time.Hour)
	one, two := int64(1), int64(2)
	r1 := cursorVendorRow{Timestamp: "1785542400000", ConversationID: "b", TokenUsage: &cursorVendorTokenUsage{InputTokens: &one}}
	r2 := cursorVendorRow{Timestamp: "1785542401000", ConversationID: "a", TokenUsage: &cursorVendorTokenUsage{InputTokens: &two}}
	a := buildCursorVendorSnapshot([]cursorVendorRow{r1, r2}, start, end, nil, cursorVendorShapeRecord{ObservedFields: []string{"timestamp"}, HTTPStatus: 200})
	b := buildCursorVendorSnapshot([]cursorVendorRow{r2, r1}, start, end, nil, a.Shape)
	if a.SnapshotID != b.SnapshotID || a.ContentSha256 != b.ContentSha256 || !reflect.DeepEqual(a.Rows, b.Rows) {
		t.Fatal("snapshot depends on pagination order")
	}
	rows := a.rowEvents("device")
	completion := a.completionEvent("device", time.Now(), "2026.08")
	if len(rows) != 2 || rows[0].Data.(map[string]interface{})["ordinal"] != 0 || completion.Data.(map[string]interface{})["rowCount"] != 2 {
		t.Fatal("invalid staging/completion")
	}
	if completion.Data.(map[string]interface{})["contentSha256"] != a.ContentSha256 {
		t.Fatal("completion digest mismatch")
	}
}

func TestCollectCursorVendorRowsBoundsCurrentPeriodAndPaginates(t *testing.T) {
	page := 0
	client := &cursorVendorClient{base: "https://api2.cursor.sh", http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		page++
		if page == 1 {
			return response(`{"totalUsageEventsCount":2,"usageEventsDisplay":[{"timestamp":"1785542401000","model":"default","kind":"x","conversationId":"in","tokenUsage":{"inputTokens":1,"outputTokens":0,"cacheReadTokens":0,"totalCents":1}}]}`), nil
		}
		return response(`{"totalUsageEventsCount":2,"usageEventsDisplay":[{"timestamp":"1785456000000","model":"default","kind":"x","conversationId":"old","tokenUsage":{"inputTokens":1,"outputTokens":0,"cacheReadTokens":0,"totalCents":1}}]}`), nil
	})}}
	start := time.UnixMilli(1785542400000)
	rows, shape, err := collectCursorVendorRows(client, cursorCredential{token: "s"}, start, start.Add(time.Hour))
	if err != nil || len(rows) != 1 || rows[0].ConversationID != "in" || page != 2 || len(shape.MissingFields) != 2 {
		t.Fatalf("rows=%#v shape=%#v page=%d err=%v", rows, shape, page, err)
	}
}

func TestVendorCostClaimSuppressesOnlyHookCost(t *testing.T) {
	t.Setenv("PROMPTSTER_STATE_DIR", t.TempDir())
	recordCursorVendorCostClaims([]cursorVendorRow{{ConversationID: "conversation-1"}})
	ev := event.NewEvent("ai_response", "conversation-1")
	ev.Source = CursorHookIntegration
	ev.Data = map[string]interface{}{"inputTokens": int64(10), "costUsd": 1.25, "model": "default", "durationMs": 40}
	suppressCursorHookCostIfVendorClaimed(&ev)
	data := ev.Data.(map[string]interface{})
	if _, ok := data["inputTokens"]; ok {
		t.Fatal("hook token cost survived vendor claim")
	}
	if _, ok := data["costUsd"]; ok {
		t.Fatal("hook dollar cost survived vendor claim")
	}
	if data["model"] != "default" || data["durationMs"] != 40 {
		t.Fatalf("behavior columns were suppressed: %#v", data)
	}
}
