package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"

	"github.com/beechfuzz/watch-party/internal/dbx"
	"github.com/beechfuzz/watch-party/internal/emby"
	"github.com/beechfuzz/watch-party/internal/party"
	"github.com/beechfuzz/watch-party/internal/syncalg"
	"github.com/beechfuzz/watch-party/internal/wsproto"
)

// readTimeout bounds how long the server waits for the next frame from a
// client before treating the connection as dead. Clients are expected to
// send a ping well within this window (see web/static/js/ws-client.js);
// three missed beats' worth of tolerance avoids flapping on a single slow
// tick without leaving genuinely dead connections registered for minutes.
const readTimeout = 45 * time.Second

// wsConn implements party.Conn over a real WebSocket. Sends are buffered
// and drained by a dedicated writer goroutine so one slow client can never
// block the party actor (which calls Conn.Send synchronously while
// serializing all party state mutation).
type wsConn struct {
	c      *websocket.Conn
	send   chan wsproto.Envelope
	logger *slog.Logger

	closeOnce sync.Once
}

func newWSConn(c *websocket.Conn, logger *slog.Logger) *wsConn {
	wc := &wsConn{c: c, send: make(chan wsproto.Envelope, 64), logger: logger}
	go wc.writeLoop()
	return wc
}

func (wc *wsConn) writeLoop() {
	for env := range wc.send {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := wsjson.Write(ctx, wc.c, env)
		cancel()
		if err != nil {
			wc.logger.Debug("ws write failed", "error", err)
		}
	}
}

func (wc *wsConn) Send(env wsproto.Envelope) {
	defer func() { recover() }() // sending on a closed channel after Close(); safe to drop
	select {
	case wc.send <- env:
	default:
		wc.logger.Warn("ws send buffer full, dropping message for slow client")
	}
}

func (wc *wsConn) Close(code int, reason string) {
	wc.closeOnce.Do(func() {
		close(wc.send)
		wc.c.Close(websocket.StatusCode(code), reason)
	})
}

// handleWebSocket upgrades the connection, authenticates via the session
// cookie (WebSocket handshakes carry cookies automatically), validates
// Origin against the configured application origin(s), re-validates the
// user's access to the party's media item (media authorization must be
// re-checked on join, not just at party creation), and then runs the
// per-connection read loop dispatching control/lifecycle/transport frames
// to the party actor.
func (a *App) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	partyID := r.PathValue("id")

	sess, err := a.Sessions.Authenticate(r.Context(), r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	user, err := a.Store.GetUser(r.Context(), sess.UserID)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	if !a.originAllowed(r.Header.Get("Origin")) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}

	row, err := a.Store.GetParty(r.Context(), partyID)
	if err != nil {
		if errors.Is(err, dbx.ErrNotFound) {
			http.Error(w, "party not found", http.StatusNotFound)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if row.Status == dbx.PartyStatusEnded {
		http.Error(w, "party has ended", http.StatusGone)
		return
	}

	p, ok := a.Hub.Get(partyID)
	if !ok {
		http.Error(w, "party not found", http.StatusNotFound)
		return
	}

	// Media authorization must be re-validated on join, not just at party
	// creation. An idle party (nothing currently loaded — true for every
	// party immediately after creation now that media is added afterward
	// via the playlist) has nothing to authorize against yet; once there IS
	// a current item, the real per-participant, per-item check happens
	// again at handlePlaybackURL every time the current item changes, not
	// only here at join — see ARCHITECTURE.md's Playlist section.
	if currentItemID := p.Snapshot().ItemID; currentItemID != "" {
		token, err := a.TokenCipher.Decrypt(user.EncryptedAccessToken)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if _, err := a.Emby.GetItem(r.Context(), token, user.ID, currentItemID); err != nil {
			if errors.Is(err, emby.ErrUnauthorized) {
				_ = a.Store.DeleteSessionsForUser(r.Context(), user.ID)
			}
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}

	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: a.originHosts(),
	})
	if err != nil {
		return
	}

	snap, err := p.Join(user.ID, user.DisplayName)
	if err != nil {
		c.Close(websocket.StatusPolicyViolation, "could not join party")
		return
	}

	conn := newWSConn(c, a.Logger)
	p.AttachConn(user.ID, conn)

	payload, _ := json.Marshal(snap)
	conn.Send(wsproto.Envelope{ProtocolVersion: wsproto.ProtocolVersion, Type: wsproto.MsgSnapshot, Payload: payload})

	a.wsReadLoop(r.Context(), c, conn, p, user.ID)
}

func (a *App) wsReadLoop(ctx context.Context, c *websocket.Conn, conn *wsConn, p *party.Party, userID string) {
	defer func() {
		p.Disconnect(userID)
		conn.Close(int(websocket.StatusNormalClosure), "")
	}()

	for {
		readCtx, cancel := context.WithTimeout(ctx, readTimeout)
		var env wsproto.Envelope
		err := wsjson.Read(readCtx, c, &env)
		cancel()
		if err != nil {
			return
		}
		a.dispatchWSMessage(ctx, p, userID, conn, env)
		if env.Type == wsproto.MsgLeave {
			p.Leave(userID)
			return
		}
	}
}

func (a *App) dispatchWSMessage(ctx context.Context, p *party.Party, userID string, conn *wsConn, env wsproto.Envelope) {
	switch env.Type {
	case wsproto.MsgPlay, wsproto.MsgPause, wsproto.MsgSeek:
		var payload wsproto.ControlPayload
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			return
		}
		_ = p.HandleControl(userID, env.Type, payload.PositionTicks)

	case wsproto.MsgEndedHint:
		p.HandleEndedHint()

	case wsproto.MsgChatSend:
		var payload wsproto.ChatSendPayload
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			return
		}
		_ = p.HandleChatSend(ctx, userID, payload.Body)

	case wsproto.MsgHostTransfer:
		var payload wsproto.HostTransferPayload
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			return
		}
		_ = p.HandleHostTransfer(userID, payload.NewHostUserID)

	case wsproto.MsgClockSync:
		var ping wsproto.ClockSyncPing
		if err := json.Unmarshal(env.Payload, &ping); err != nil {
			return
		}
		t1 := time.Now().UnixMilli()
		pong := wsproto.ClockSyncPong{T0: ping.T0, T1: t1, T2: time.Now().UnixMilli()}
		respPayload, _ := json.Marshal(pong)
		conn.Send(wsproto.Envelope{ProtocolVersion: wsproto.ProtocolVersion, Type: wsproto.MsgClockSync, Payload: respPayload})

		a.logSyncDiagnostics(ctx, p, userID, ping)

	case wsproto.MsgPing:
		conn.Send(wsproto.Envelope{ProtocolVersion: wsproto.ProtocolVersion, Type: wsproto.MsgPong})

	case wsproto.MsgLeave:
		// handled by the caller (triggers loop exit); nothing else to do here.

	default:
		a.Logger.Debug("unhandled ws message type", "type", env.Type)
	}
}

// logSyncDiagnostics emits the spec's required debug-level sync diagnostics
// (party ID, user ID, sequence, authoritative-vs-client position,
// calculated drift, RTT, clock offset, action taken) and — since the
// client's self-reported position is available here anyway — throttles an
// update to party_members.last_reported_position_ticks off the same
// message, per the data model's "throttling state" column. None of this
// feeds back into authoritative state: it's diagnostics only, gated behind
// LOG_LEVEL=debug, and never includes anything on the "never log" list
// (tokens, session IDs, playback URLs) — only position/timing numbers.
func (a *App) logSyncDiagnostics(ctx context.Context, p *party.Party, userID string, ping wsproto.ClockSyncPing) {
	if ping.PositionTicks == nil {
		return
	}
	snap := p.Snapshot()
	serverTS, err := time.Parse(time.RFC3339Nano, snap.ServerTimestamp)
	if err != nil {
		return
	}
	authoritativeNow := syncalg.ExpectedPosition(syncalg.AuthoritativeState{
		PositionTicks: snap.PositionTicks, IsPlaying: snap.IsPlaying, ServerTimestamp: serverTS,
	}, time.Now(), 0)
	drift := *ping.PositionTicks - authoritativeNow

	attrs := []any{
		"party_id", p.ID, "user_id", userID, "sequence_number", snap.SequenceNumber,
		"authoritative_position_ticks", authoritativeNow, "client_position_ticks", *ping.PositionTicks,
		"drift_ticks", drift,
	}
	if ping.LastRTTMs != nil {
		attrs = append(attrs, "rtt_ms", *ping.LastRTTMs)
	}
	if ping.LastClockOffsetMs != nil {
		attrs = append(attrs, "clock_offset_ms", *ping.LastClockOffsetMs)
	}
	if ping.LastCorrectionAction != "" {
		attrs = append(attrs, "correction_action", ping.LastCorrectionAction)
	}
	a.Logger.Debug("sync diagnostics", attrs...)

	if err := a.Store.UpdateMemberLastReported(ctx, p.ID, userID, *ping.PositionTicks, time.Now()); err != nil {
		a.Logger.Debug("update member last reported failed", "error", err)
	}
}

func (a *App) originAllowed(origin string) bool {
	if origin == "" {
		return false
	}
	for _, o := range a.AppOrigins {
		if o == origin {
			return true
		}
	}
	return false
}

// originHosts extracts host[:port] from each configured origin for
// nhooyr.io/websocket's OriginPatterns (matched against the Origin
// header's host, not the full URL) — used as defense-in-depth alongside
// the explicit originAllowed check above, which runs first and produces a
// clean, logged rejection rather than relying solely on the library's.
func (a *App) originHosts() []string {
	hosts := make([]string, 0, len(a.AppOrigins))
	for _, o := range a.AppOrigins {
		if u, err := url.Parse(o); err == nil && u.Host != "" {
			hosts = append(hosts, u.Host)
		}
	}
	return hosts
}
