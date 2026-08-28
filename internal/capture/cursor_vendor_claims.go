package capture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/sign"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// The vendor rail owns monetary/token columns for a joined conversation; the
// hook rail continues to own prompts, edits, outcomes and timing. This ledger
// makes that precedence per-column instead of suppressing either rail whole.
type cursorVendorCostClaims struct {
	Claims map[string]int64 `json:"claims"`
	V      int              `json:"v"`
}

const cursorVendorCostClaimTTL = 7 * 24 * time.Hour

func cursorVendorCostClaimsPath() string {
	return filepath.Join(state.StateDir(), "cursor-vendor-cost-claims.json")
}

func loadCursorVendorCostClaims() cursorVendorCostClaims {
	c := cursorVendorCostClaims{Claims: map[string]int64{}, V: 1}
	b, err := os.ReadFile(cursorVendorCostClaimsPath())
	if err == nil {
		_ = json.Unmarshal(b, &c)
	}
	if c.Claims == nil {
		c.Claims = map[string]int64{}
	}
	return c
}

func recordCursorVendorCostClaims(rows []cursorVendorRow) {
	_ = sign.WithBufferLock(cursorVendorCostClaimsPath()+".lock", func() error {
		c := loadCursorVendorCostClaims()
		now := time.Now()
		for id, at := range c.Claims {
			if now.Sub(time.UnixMilli(at)) > cursorVendorCostClaimTTL {
				delete(c.Claims, id)
			}
		}
		for _, row := range rows {
			if row.ConversationID != "" {
				c.Claims[row.ConversationID] = now.UnixMilli()
			}
		}
		b, err := json.Marshal(c)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(state.StateDir(), 0o700); err != nil {
			return err
		}
		tmp, err := os.CreateTemp(state.StateDir(), "cursor-vendor-claims-*.tmp")
		if err != nil {
			return err
		}
		name := tmp.Name()
		defer os.Remove(name)
		if _, err = tmp.Write(b); err != nil {
			_ = tmp.Close()
			return err
		}
		if err = tmp.Close(); err != nil {
			return err
		}
		return os.Rename(name, cursorVendorCostClaimsPath())
	})
}

func suppressCursorHookCostIfVendorClaimed(ev *event.Event) {
	if ev == nil || ev.Source != CursorHookIntegration || ev.SessionID == "" {
		return
	}
	at, ok := loadCursorVendorCostClaims().Claims[ev.SessionID]
	if !ok || time.Since(time.UnixMilli(at)) > cursorVendorCostClaimTTL {
		return
	}
	data, ok := ev.Data.(map[string]interface{})
	if !ok {
		return
	}
	for _, key := range []string{"inputTokens", "outputTokens", "cacheReadTokens", "cacheWriteTokens", "cacheWriteInputTokens", "totalTokens", "costUsd", "chargedCents", "totalCents"} {
		delete(data, key)
	}
}
