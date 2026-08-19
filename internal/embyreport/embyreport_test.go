package embyreport

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/beechfuzz/watch-party/internal/cryptox"
	"github.com/beechfuzz/watch-party/internal/dbx"
	"github.com/beechfuzz/watch-party/internal/emby"
	"github.com/beechfuzz/watch-party/internal/party"
)

type recordedCall struct {
	path          string
	itemID        string
	positionTicks int64
	isPaused      bool
}

func newFakeEmbyReportServer(t *testing.T) (*httptest.Server, *sync.Mutex, *[]recordedCall) {
	t.Helper()
	var mu sync.Mutex
	var calls []recordedCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		pos, _ := body["PositionTicks"].(float64)
		paused, _ := body["IsPaused"].(bool)
		itemID, _ := body["ItemId"].(string)
		calls = append(calls, recordedCall{path: r.URL.Path, itemID: itemID, positionTicks: int64(pos), isPaused: paused})
		mu.Unlock()
		w.WriteHeader(204)
	}))
	t.Cleanup(srv.Close)
	return srv, &mu, &calls
}

func setupReporter(t *testing.T) (*Reporter, *party.Hub, *dbx.Store, *sync.Mutex, *[]recordedCall) {
	t.Helper()
	dir := t.TempDir()
	db, err := dbx.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store := dbx.NewStore(db)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	hub := party.NewHub(store, party.Tuning{
		SnapshotInterval: time.Hour, SoftDriftMS: 300, HardDriftMS: 1500,
		MaxRateAdjustment: 0.05, HostGracePeriod: 20 * time.Second,
	}, logger)
	t.Cleanup(func() { hub.Shutdown(context.Background()) })

	srv, mu, calls := newFakeEmbyReportServer(t)
	embyClient := emby.NewClient(srv.URL)

	key := make([]byte, 32)
	cipher, err := cryptox.NewTokenCipher(key)
	if err != nil {
		t.Fatal(err)
	}

	rp := New(hub, store, embyClient, cipher, 30*time.Millisecond, logger)

	ctx := context.Background()
	if err := store.UpsertUser(ctx, "user1", "Alice", mustEncrypt(t, cipher, "tok1"), time.Now()); err != nil {
		t.Fatal(err)
	}

	return rp, hub, store, mu, calls
}

func mustEncrypt(t *testing.T, c *cryptox.TokenCipher, plaintext string) string {
	t.Helper()
	enc, err := c.Encrypt(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	return enc
}

func TestRecordPlaySession_SendsStartReport(t *testing.T) {
	rp, hub, _, mu, calls := setupReporter(t)
	ctx := context.Background()
	p, err := hub.CreateParty(ctx, "party1", "Test Party", party.Settings{AutoAdvance: true, ShowNextDialog: true, AutoplayEnabled: true, AutoplayDelaySeconds: 5}, "user1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Join("user1", "Alice"); err != nil {
		t.Fatal(err)
	}

	rp.RecordPlaySession(ctx, "party1", "user1", "src1", "sess1", "DirectStream")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(*calls)
		mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(*calls) == 0 {
		t.Fatal("expected a Sessions/Playing start report")
	}
	if (*calls)[0].path != "/Sessions/Playing" {
		t.Errorf("path = %q, want /Sessions/Playing", (*calls)[0].path)
	}
}

func TestPeriodicProgress_ReportsServerDerivedPosition(t *testing.T) {
	rp, hub, _, mu, calls := setupReporter(t)
	ctx := context.Background()
	p, err := hub.CreateParty(ctx, "party1", "Test Party", party.Settings{AutoAdvance: true, ShowNextDialog: true, AutoplayEnabled: true, AutoplayDelaySeconds: 5}, "user1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Join("user1", "Alice"); err != nil {
		t.Fatal(err)
	}
	if err := p.HandleControl("user1", "play", 0); err != nil {
		t.Fatal(err)
	}
	rp.RecordPlaySession(ctx, "party1", "user1", "src1", "sess1", "DirectStream")

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go rp.Run(runCtx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(*calls)
		mu.Unlock()
		if n >= 2 { // start + at least one progress
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	foundProgress := false
	for _, c := range *calls {
		if c.path == "/Sessions/Playing/Progress" {
			foundProgress = true
			if c.isPaused {
				t.Error("progress report says paused, but party is playing")
			}
		}
	}
	if !foundProgress {
		t.Fatal("expected at least one Sessions/Playing/Progress report")
	}
}

func TestPartyEnd_TriggersStopReport(t *testing.T) {
	rp, hub, _, mu, calls := setupReporter(t)
	ctx := context.Background()
	p, err := hub.CreateParty(ctx, "party1", "Test Party", party.Settings{AutoAdvance: true, ShowNextDialog: true, AutoplayEnabled: true, AutoplayDelaySeconds: 5}, "user1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Join("user1", "Alice"); err != nil {
		t.Fatal(err)
	}
	rp.RecordPlaySession(ctx, "party1", "user1", "src1", "sess1", "DirectStream")

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go rp.Run(runCtx)

	if err := hub.EndParty(ctx, "party1"); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		found := false
		for _, c := range *calls {
			if c.path == "/Sessions/Playing/Stopped" {
				found = true
			}
		}
		mu.Unlock()
		if found {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("expected a Sessions/Playing/Stopped report after party end")
}

// TestItemChanged_TriggersStopReportForOutgoingItem covers the playlist
// advance case: when the party's current item changes, the reporter must
// send a Stopped report for the OUTGOING item (not the new one) -- a report
// tagging the wrong item's Emby resume point/watch history would be exactly
// the kind of corruption the spec's server-derived-position rule exists to
// prevent.
func TestItemChanged_TriggersStopReportForOutgoingItem(t *testing.T) {
	rp, hub, _, mu, calls := setupReporter(t)
	ctx := context.Background()
	p, err := hub.CreateParty(ctx, "party1", "Test Party", party.Settings{AutoAdvance: true, ShowNextDialog: true, AutoplayEnabled: true, AutoplayDelaySeconds: 5}, "user1")
	if err != nil {
		t.Fatal(err)
	}
	itemA, err := p.AddPlaylistItem(ctx, "user1", "item-a", 1000*10_000_000)
	if err != nil {
		t.Fatal(err)
	}
	itemB, err := p.AddPlaylistItem(ctx, "user1", "item-b", 1000*10_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Join("user1", "Alice"); err != nil {
		t.Fatal(err)
	}
	if err := p.SelectCurrentItem("user1", itemA.ID); err != nil {
		t.Fatal(err)
	}
	rp.RecordPlaySession(ctx, "party1", "user1", "src-a", "sess-a", "DirectStream")

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go rp.Run(runCtx)

	// Host explicitly switches to item B -- item A should be stop-reported.
	if err := p.SelectCurrentItem("user1", itemB.ID); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		for _, c := range *calls {
			if c.path == "/Sessions/Playing/Stopped" {
				gotItemID := c.itemID
				mu.Unlock()
				if gotItemID != "item-a" {
					t.Fatalf("Stopped report itemID = %q, want %q (the outgoing item)", gotItemID, "item-a")
				}
				return
			}
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("expected a Sessions/Playing/Stopped report for the outgoing item after an item change")
}
