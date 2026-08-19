// Package embyreport is the per-user Emby playback-progress reporting side
// channel described in the spec: it keeps each participant's own Emby
// "Continue Watching"/resume state accurate, independent of the
// party-internal WebSocket sync protocol. It is explicitly best-effort —
// failures here must never block or degrade party playback sync.
package embyreport

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/beechfuzz/watch-party/internal/cryptox"
	"github.com/beechfuzz/watch-party/internal/dbx"
	"github.com/beechfuzz/watch-party/internal/emby"
	"github.com/beechfuzz/watch-party/internal/party"
	"github.com/beechfuzz/watch-party/internal/syncalg"
)

// maxBackoffTicks caps how many consecutive interval ticks a repeatedly-
// failing (partyID, userID) report is skipped for, so a prolonged Emby
// outage doesn't turn into a tight retry loop, without needing a separate
// timer per participant.
const maxBackoffTicks = 5

type playState struct {
	MediaSourceID string
	PlaySessionID string
	PlayMethod    string
	failureCount  int
	skipTicks     int
}

type Reporter struct {
	hub      *party.Hub
	store    *dbx.Store
	emby     *emby.Client
	cipher   *cryptox.TokenCipher
	interval time.Duration
	logger   *slog.Logger

	mu       sync.Mutex
	sessions map[string]map[string]*playState // partyID -> userID -> playState
}

func New(hub *party.Hub, store *dbx.Store, embyClient *emby.Client, cipher *cryptox.TokenCipher, interval time.Duration, logger *slog.Logger) *Reporter {
	return &Reporter{
		hub: hub, store: store, emby: embyClient, cipher: cipher, interval: interval, logger: logger,
		sessions: make(map[string]map[string]*playState),
	}
}

// Run drives both event-triggered (start/stop) and periodic (progress)
// reporting until ctx is cancelled (server shutdown).
func (rp *Reporter) Run(ctx context.Context) {
	events := rp.hub.Events()
	ticker := time.NewTicker(rp.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-events:
			if !ok {
				return
			}
			rp.handleEvent(ctx, evt)
		case <-ticker.C:
			rp.reportAllProgress(ctx)
		}
	}
}

// RecordPlaySession registers the PlaySessionId/MediaSourceId/PlayMethod a
// user's client is actually using (captured when the HTTP layer hands them
// a playback URL) and immediately sends the Sessions/Playing "start"
// report. Reusing the real PlaySessionId — rather than minting a separate
// one just for reporting — means Emby associates the report with the
// user's actual playback session instead of tracking a second, phantom one.
func (rp *Reporter) RecordPlaySession(ctx context.Context, partyID, userID, mediaSourceID, playSessionID, playMethod string) {
	rp.mu.Lock()
	if rp.sessions[partyID] == nil {
		rp.sessions[partyID] = make(map[string]*playState)
	}
	rp.sessions[partyID][userID] = &playState{MediaSourceID: mediaSourceID, PlaySessionID: playSessionID, PlayMethod: playMethod}
	rp.mu.Unlock()

	rp.report(ctx, partyID, userID, rp.emby.ReportPlaybackStart)
}

func (rp *Reporter) handleEvent(ctx context.Context, evt party.Event) {
	switch evt.Type {
	case party.EventLeft, party.EventDisconnected:
		// The party is still registered in the hub at this point (only
		// EventEnded triggers removal), so the live lookup in report() is
		// both safe and gives a slightly fresher extrapolated position than
		// the event's captured snapshot would.
		rp.report(ctx, evt.PartyID, evt.UserID, rp.emby.ReportPlaybackStopped)
		rp.forget(evt.PartyID, evt.UserID)
	case party.EventItemChanged:
		// The party's current item just changed (playlist advance, end of
		// playlist, or a host selecting a different item) -- evt.ItemID/
		// PositionTicks describe the OUTGOING item's final state (see the
		// EventItemChanged doc comment), not the new one. A live report()
		// lookup would already see the NEW item (the actor's state has
		// already moved on by the time this event is processed), so this
		// uses sendReport directly with the event's captured values instead
		// -- same reason EventEnded does. Every tracked user for this party
		// gets a Stopped report for the item that just ended, then their
		// play-session tracking is forgotten (not the whole party, which
		// stays active): the new item's tracking begins lazily, the same
		// way it does on join, whenever each client fetches a playback URL
		// for it (RecordPlaySession).
		rp.mu.Lock()
		members := rp.sessions[evt.PartyID]
		userIDs := make([]string, 0, len(members))
		for uid := range members {
			userIDs = append(userIDs, uid)
		}
		rp.mu.Unlock()
		for _, uid := range userIDs {
			rp.sendReport(ctx, evt.PartyID, uid, evt.ItemID, evt.PositionTicks, true, rp.emby.ReportPlaybackStopped)
			rp.forget(evt.PartyID, uid)
		}
	case party.EventEnded:
		// Hub.EndParty removes the party from the hub immediately after
		// emitting this event, so a live hub.Get() here would already miss
		// — use the final state captured directly on the event instead
		// (see the Event doc comment in the party package).
		rp.mu.Lock()
		members := rp.sessions[evt.PartyID]
		userIDs := make([]string, 0, len(members))
		for uid := range members {
			userIDs = append(userIDs, uid)
		}
		rp.mu.Unlock()
		for _, uid := range userIDs {
			rp.sendReport(ctx, evt.PartyID, uid, evt.ItemID, evt.PositionTicks, !evt.IsPlaying, rp.emby.ReportPlaybackStopped)
		}
		rp.mu.Lock()
		delete(rp.sessions, evt.PartyID)
		rp.mu.Unlock()
	}
}

func (rp *Reporter) forget(partyID, userID string) {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	if m, ok := rp.sessions[partyID]; ok {
		delete(m, userID)
	}
}

func (rp *Reporter) reportAllProgress(ctx context.Context) {
	for _, p := range rp.hub.All() {
		snap := p.Snapshot()
		for _, m := range snap.Members {
			if m.ConnectionStatus != "connected" {
				continue
			}
			rp.mu.Lock()
			ps, ok := rp.sessions[snap.PartyID][m.UserID]
			if ok && ps.skipTicks > 0 {
				ps.skipTicks--
				ok = false
			}
			rp.mu.Unlock()
			if !ok {
				continue // no recorded play session yet (or currently backed off), nothing to report
			}
			rp.report(ctx, snap.PartyID, m.UserID, rp.emby.ReportPlaybackProgress)
		}
	}
}

type reportFunc func(ctx context.Context, accessToken, deviceID string, r emby.PlayingReport) error

// report computes this user's server-derived position from the party's
// *current, live* authoritative state (never trusting client-reported
// position — see spec) and sends the given report type under the user's
// own token. Only valid while the party is still registered in the hub —
// for EventEnded, use sendReport directly with the event's captured final
// state instead (see handleEvent).
func (rp *Reporter) report(ctx context.Context, partyID, userID string, fn reportFunc) {
	p, ok := rp.hub.Get(partyID)
	if !ok {
		return
	}
	snap := p.Snapshot()

	serverTS, err := time.Parse(time.RFC3339Nano, snap.ServerTimestamp)
	if err != nil {
		rp.logger.Error("embyreport: parse server_timestamp failed", "error", err)
		return
	}
	position := syncalg.ExpectedPosition(syncalg.AuthoritativeState{
		PositionTicks: snap.PositionTicks, IsPlaying: snap.IsPlaying, ServerTimestamp: serverTS,
	}, time.Now(), 0)

	rp.sendReport(ctx, partyID, userID, snap.ItemID, position, !snap.IsPlaying, fn)
}

// sendReport is the shared core: given an already-known position/pause
// state (either freshly computed by report(), or captured directly on a
// party.Event for EventEnded), looks up the recorded play session and the
// user's own decrypted token, sends the report, and updates per-user
// backoff state on failure.
func (rp *Reporter) sendReport(ctx context.Context, partyID, userID, itemID string, positionTicks int64, isPaused bool, fn reportFunc) {
	rp.mu.Lock()
	ps, ok := rp.sessions[partyID][userID]
	rp.mu.Unlock()
	if !ok {
		return
	}

	user, err := rp.store.GetUser(ctx, userID)
	if err != nil {
		return
	}
	token, err := rp.cipher.Decrypt(user.EncryptedAccessToken)
	if err != nil {
		rp.logger.Error("embyreport: decrypt token failed", "error", err)
		return
	}

	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err = fn(reqCtx, token, emby.DeviceID(userID), emby.PlayingReport{
		ItemID: itemID, MediaSourceID: ps.MediaSourceID, PositionTicks: positionTicks,
		IsPaused: isPaused, PlayMethod: ps.PlayMethod, PlaySessionID: ps.PlaySessionID, CanSeek: true,
	})

	rp.mu.Lock()
	defer rp.mu.Unlock()
	// Re-fetch: the map entry may have been deleted (forget()) concurrently
	// with this in-flight report; nothing left to update backoff state on.
	cur, stillTracked := rp.sessions[partyID][userID]
	if !stillTracked || cur != ps {
		return
	}
	if err != nil {
		if ps.failureCount < maxBackoffTicks {
			ps.failureCount++
		}
		ps.skipTicks = ps.failureCount
		rp.logger.Warn("embyreport: report failed, will retry with backoff", "party_id", partyID, "user_id", userID, "error", err)
		return
	}
	ps.failureCount = 0
	ps.skipTicks = 0
}
