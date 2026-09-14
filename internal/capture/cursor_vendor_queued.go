package capture

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/pa-arth/promptster-teams-cli/internal/event"
	"github.com/pa-arth/promptster-teams-cli/internal/state"
)

// A full repair bounds the time a row accepted by transport but lost at storage
// can prevent a completion from validating. Identical IDs make the repair a DB
// no-op; it costs only the occasional upload.
const cursorVendorRowsRefreshInterval = 6 * time.Hour

type cursorVendorQueuedEntry struct {
	SnapshotID string    `json:"snapshotId"`
	AccountRef string    `json:"accountRef"`
	CycleStart string    `json:"cycleStart"`
	CycleEnd   string    `json:"cycleEnd"`
	RowHashes  []string  `json:"rowHashes"` // index is the durable ordinal
	LastFullAt time.Time `json:"lastFullAt"`
}

type cursorVendorQueuedSnapshots struct {
	ByDevice map[string]cursorVendorQueuedEntry `json:"byDevice"`
}

func cursorVendorQueuedSnapshotPath() string {
	return filepath.Join(state.StateDir(), "cursor-vendor-queued-v2.json")
}

func loadCursorVendorQueuedSnapshots() cursorVendorQueuedSnapshots {
	b, err := os.ReadFile(cursorVendorQueuedSnapshotPath())
	if err != nil {
		return cursorVendorQueuedSnapshots{ByDevice: map[string]cursorVendorQueuedEntry{}}
	}
	var prior cursorVendorQueuedSnapshots
	if json.Unmarshal(b, &prior) != nil || prior.ByDevice == nil {
		return cursorVendorQueuedSnapshots{ByDevice: map[string]cursorVendorQueuedEntry{}}
	}
	return prior
}

func cursorVendorHash(parts ...string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%q", parts)))
	return hex.EncodeToString(sum[:])
}

type vendorQueuedRow struct {
	hash  string
	event event.Event
}

// queueCompleteCursorVendorSnapshot implements protocol v2. Within one
// generation, ordinals append and prior rows remain immutable. A mutable vendor
// correction (any old row disappears) starts a fresh generation, preserving the
// previous completion as last-good until the replacement validates. New rows
// alone never cause a full-cycle restatement.
func queueCompleteCursorVendorSnapshot(
	snapshot cursorVendorSnapshot, deviceID string, capturedAt time.Time,
	cursorVersion string, enqueue func(event.Event) bool,
) bool {
	priorFile := loadCursorVendorQueuedSnapshots()
	prior := priorFile.ByDevice[deviceID]
	cycleStart := snapshot.CycleStart.UTC().Format(time.RFC3339Nano)
	cycleEnd := snapshot.CycleEnd.UTC().Format(time.RFC3339Nano)
	baseID := cursorVendorHash("cursor-vendor-v2", deviceID, cycleStart, cycleEnd, snapshot.ContentSha256)

	events := snapshot.rowEvents(deviceID)
	rows := make([]vendorQueuedRow, 0, len(events))
	for i, ev := range events {
		// JSON includes the vendor's additional metadata. A change to one of
		// those fields must not silently reuse a stored row whose value is stale.
		raw, _ := json.Marshal(snapshot.Rows[i])
		rows = append(rows, vendorQueuedRow{hash: cursorVendorHash(string(raw)), event: ev})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].hash < rows[j].hash })
	currentCounts := map[string]int{}
	for _, row := range rows {
		currentCounts[row.hash]++
	}
	priorCounts := map[string]int{}
	for _, hash := range prior.RowHashes {
		priorCounts[hash]++
	}

	sameCycle := prior.SnapshotID != "" && prior.CycleStart == cycleStart && prior.CycleEnd == cycleEnd
	sameAccount := prior.AccountRef == snapshot.AccountRef || prior.AccountRef == "" || prior.AccountRef == "unknown"
	appendOnly := sameCycle && sameAccount
	for hash, count := range priorCounts {
		if currentCounts[hash] < count {
			appendOnly = false
			break
		}
	}
	snapshotID := baseID
	rowHashes := []string{}
	lastFullAt := capturedAt
	full := !appendOnly
	if appendOnly {
		snapshotID = prior.SnapshotID
		rowHashes = append(rowHashes, prior.RowHashes...)
		lastFullAt = prior.LastFullAt
		age := capturedAt.Sub(lastFullAt)
		full = age < 0 || age >= cursorVendorRowsRefreshInterval
		if full {
			lastFullAt = capturedAt
		}
	} else if sameCycle {
		// A correction or real account switch must not mix incompatible row
		// populations under one ID. Stable for retries until the marker commits.
		snapshotID = cursorVendorHash("cursor-vendor-v2-rewrite", prior.SnapshotID, snapshot.ContentSha256, snapshot.AccountRef)
	}

	snapshot.SnapshotID = snapshotID
	sentCounts := map[string]int{}
	newOrdinal := len(rowHashes)
	queuedAll := true
	for _, row := range rows {
		sentCounts[row.hash]++
		if !full && sentCounts[row.hash] <= priorCounts[row.hash] {
			continue
		}
		ordinal := newOrdinal
		if full && appendOnly && sentCounts[row.hash] <= priorCounts[row.hash] {
			// Reuse the prior ordinal for a repair, rather than creating a
			// second physical row. Each duplicate hash has its own ordinal.
			remaining := sentCounts[row.hash]
			for idx, old := range prior.RowHashes {
				if old == row.hash {
					remaining--
					if remaining == 0 {
						ordinal = idx
						break
					}
				}
			}
		} else {
			rowHashes = append(rowHashes, row.hash)
			newOrdinal++
		}
		ev := row.event
		data := ev.Data.(map[string]interface{})
		data["snapshotId"] = snapshotID
		data["ordinal"] = ordinal
		delete(data, "accountRef") // attribution belongs to the completion
		ev.ID = event.DeterministicUUID("cursorVendorUsage:" + snapshotID + ":" + strconv.Itoa(ordinal))
		if !enqueue(ev) {
			queuedAll = false
		}
	}
	completion := snapshot.completionEvent(deviceID, capturedAt, cursorVersion)
	completion.Data.(map[string]interface{})["protocolVersion"] = 2
	if !enqueue(completion) {
		queuedAll = false
	}
	if queuedAll {
		priorFile.ByDevice[deviceID] = cursorVendorQueuedEntry{
			SnapshotID: snapshotID, AccountRef: snapshot.AccountRef,
			CycleStart: cycleStart, CycleEnd: cycleEnd,
			RowHashes: rowHashes, LastFullAt: lastFullAt.UTC(),
		}
		if err := rememberCursorVendorQueuedSnapshots(priorFile); err != nil {
			fmt.Fprintf(os.Stderr, "cursor-vendor: cannot remember queued snapshot: %v\n", err)
		}
	}
	return queuedAll
}

func rememberCursorVendorQueuedSnapshots(entries cursorVendorQueuedSnapshots) error {
	path := cursorVendorQueuedSnapshotPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cursor-vendor-queued-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
