package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/beechfuzz/watch-party/internal/dbx"
	"github.com/beechfuzz/watch-party/internal/emby"
	"github.com/beechfuzz/watch-party/internal/idgen"
	"github.com/beechfuzz/watch-party/internal/party"
	"github.com/beechfuzz/watch-party/internal/wsproto"
)

type createPartyRequest struct {
	ItemID string `json:"item_id"`
}

// partySummary is one entry in the GET /api/parties listing — enough for
// the Home page's Active Parties / Your Parties cards without a second
// round-trip per party. There's no per-party access-control list; visibility
// here is simply "every currently active party" (this is a small,
// friends/family self-hosted tool, not a multi-tenant service), same as
// join-by-link already implicitly allowed before this endpoint existed.
type partySummary struct {
	PartyID         string `json:"party_id"`
	ItemID          string `json:"item_id"`
	ItemTitle       string `json:"item_title"`
	HostUserID      string `json:"host_user_id"`
	HostDisplayName string `json:"host_display_name"`
	IsPlaying       bool   `json:"is_playing"`
	PositionTicks   int64  `json:"position_ticks"`
	DurationTicks   int64  `json:"duration_ticks"`
	MemberCount     int    `json:"member_count"`
}

// handleListParties powers the Home page's Active Parties / Your Parties
// sections. Item titles are resolved via the requesting user's own Emby
// token (never a shared one, per the spec's hard requirement elsewhere in
// this codebase) -- a party whose item that specific user can't see (or
// that Emby can't resolve for any other reason) is silently omitted from
// their listing rather than failing the whole request, matching the
// access-control spirit of handleGetParty's join-time check.
func (a *App) handleListParties(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r.Context())
	token, err := a.TokenCipher.Decrypt(user.EncryptedAccessToken)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}

	active := a.Hub.All()
	summaries := make([]partySummary, 0, len(active))
	for _, p := range active {
		item, err := a.Emby.GetItem(r.Context(), token, user.ID, p.ItemID)
		if err != nil {
			continue
		}
		snap := p.Snapshot()

		hostDisplayName := snap.HostUserID
		if host, err := a.Store.GetUser(r.Context(), snap.HostUserID); err == nil && host != nil {
			hostDisplayName = host.DisplayName
		}

		memberCount := 0
		for _, m := range snap.Members {
			if m.ConnectionStatus == "connected" {
				memberCount++
			}
		}

		summaries = append(summaries, partySummary{
			PartyID: p.ID, ItemID: p.ItemID, ItemTitle: item.Name,
			HostUserID: snap.HostUserID, HostDisplayName: hostDisplayName,
			IsPlaying: snap.AuthoritativeState.IsPlaying, PositionTicks: p.CurrentPositionTicks(),
			DurationTicks: p.DurationTicks, MemberCount: memberCount,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"parties": summaries})
}

func (a *App) handleCreateParty(w http.ResponseWriter, r *http.Request) {
	var req createPartyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ItemID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "item_id is required")
		return
	}
	user := userFromContext(r.Context())

	token, err := a.TokenCipher.Decrypt(user.EncryptedAccessToken)
	if err != nil {
		a.Logger.Error("decrypt token failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}

	item, err := a.Emby.GetItem(r.Context(), token, user.ID, req.ItemID)
	if err != nil {
		a.handleEmbyErr(w, r.Context(), user.ID, err, "could not look up that item on Emby")
		return
	}

	partyID, err := idgen.PartyID()
	if err != nil {
		a.Logger.Error("generate party id failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}

	// Parties go straight from creation to 'active': there is no distinct
	// staging step in this design (a party is immediately joinable once
	// created). The 'created' status exists in the schema for conceptual
	// completeness with the spec's stated lifecycle but isn't a state this
	// implementation currently passes through.
	if _, err := a.Hub.CreateParty(r.Context(), partyID, req.ItemID, item.RunTimeTicks, user.ID); err != nil {
		a.Logger.Error("create party failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"party_id":       partyID,
		"item_id":        req.ItemID,
		"duration_ticks": item.RunTimeTicks,
		"host_user_id":   user.ID,
	})
}

func (a *App) handleGetParty(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	user := userFromContext(r.Context())

	row, err := a.Store.GetParty(r.Context(), id)
	if err != nil {
		if errors.Is(err, dbx.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "party not found")
			return
		}
		a.Logger.Error("get party failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	if row.Status == dbx.PartyStatusEnded {
		writeError(w, http.StatusGone, "party_ended", "this party has ended")
		return
	}

	// Media authorization must be re-validated on join, not just at party
	// creation — confirm this specific user (not just the host) can access
	// the item before they can see/join the party.
	token, err := a.TokenCipher.Decrypt(user.EncryptedAccessToken)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	item, err := a.Emby.GetItem(r.Context(), token, user.ID, row.ItemID)
	if err != nil {
		a.handleEmbyErr(w, r.Context(), user.ID, err, "you do not have access to this media item")
		return
	}

	var snap wsproto.SnapshotPayload
	if p, ok := a.Hub.Get(id); ok {
		snap = p.Snapshot()
	} else {
		// Active-in-DB but not in the hub shouldn't normally happen (active
		// parties are recovered at startup), but degrade gracefully rather
		// than 500 if it ever does.
		snap = wsproto.SnapshotPayload{PartyID: row.ID, ItemID: row.ItemID, DurationTicks: row.DurationTicks, HostUserID: row.HostUserID}
	}
	// ItemTitle rides alongside the snapshot rather than joining
	// wsproto.SnapshotPayload itself, since that struct is also the shape of
	// every WS snapshot broadcast — the party actor has no Emby access (by
	// design; see ARCHITECTURE.md §3) and so has no way to populate a title.
	// The access-check GetItem call above already fetched it for free.
	writeJSON(w, http.StatusOK, struct {
		wsproto.SnapshotPayload
		ItemTitle string `json:"item_title"`
	}{SnapshotPayload: snap, ItemTitle: item.Name})
}

func (a *App) handlePlaybackURL(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	user := userFromContext(r.Context())

	row, err := a.Store.GetParty(r.Context(), id)
	if err != nil {
		if errors.Is(err, dbx.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "party not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	if row.Status == dbx.PartyStatusEnded {
		writeError(w, http.StatusGone, "party_ended", "this party has ended")
		return
	}

	token, err := a.TokenCipher.Decrypt(user.EncryptedAccessToken)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}

	// Start the transcode (if any) at the party's current position, not the
	// beginning — critical for anyone joining a party already in progress,
	// which is the common case. Without this, Emby starts encoding from
	// zero and the client's subsequent seek to the real position makes the
	// transcoder work forward through however much has already played
	// before it can produce that segment — see ARCHITECTURE.md §5.4. Falls
	// back to 0 (start of item) in the rare case the party isn't in the hub.
	var startPositionTicks int64
	if p, ok := a.Hub.Get(id); ok {
		startPositionTicks = p.CurrentPositionTicks()
	}

	// Always derived from THIS user's own token — never a shared token —
	// so every participant streams under their own Emby permissions.
	result, err := a.Emby.GetPlaybackURL(r.Context(), token, user.ID, row.ItemID, emby.DeviceID(user.ID), startPositionTicks)
	if err != nil {
		a.handleEmbyErr(w, r.Context(), user.ID, err, "could not get a playback URL from Emby")
		return
	}

	playMethod := "Transcode"
	if !result.IsTranscoded {
		playMethod = "DirectStream"
	}
	if a.Reporter != nil {
		a.Reporter.RecordPlaySession(r.Context(), id, user.ID, result.MediaSourceID, result.PlaySessionID, playMethod)
	}

	writeJSON(w, http.StatusOK, result)
}

func (a *App) handleLeaveParty(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	user := userFromContext(r.Context())
	p, ok := a.Hub.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "party not found or already ended")
		return
	}
	p.Leave(user.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) handleEndParty(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	user := userFromContext(r.Context())
	p, ok := a.Hub.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "party not found or already ended")
		return
	}
	if p.HostUserID() != user.ID {
		writeError(w, http.StatusForbidden, "not_host", "only the host may end the party")
		return
	}
	if err := a.Hub.EndParty(r.Context(), id); err != nil {
		a.Logger.Error("end party failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type hostTransferRequest struct {
	NewHostUserID string `json:"new_host_user_id"`
}

func (a *App) handleHostTransfer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	user := userFromContext(r.Context())
	var req hostTransferRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.NewHostUserID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "new_host_user_id is required")
		return
	}
	p, ok := a.Hub.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "party not found or already ended")
		return
	}
	if err := p.HandleHostTransfer(user.ID, req.NewHostUserID); err != nil {
		switch {
		case errors.Is(err, party.ErrNotHost):
			writeError(w, http.StatusForbidden, "not_host", "only the host may transfer host status")
		case errors.Is(err, party.ErrUnknownTarget):
			writeError(w, http.StatusBadRequest, "bad_request", "target user is not a connected member")
		default:
			a.Logger.Error("host transfer failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleEmbyErr maps an emby package error to an HTTP response. On a
// rejected token it also force-invalidates the user's sessions, per the
// spec's requirement not to silently retry a dead token.
func (a *App) handleEmbyErr(w http.ResponseWriter, ctx context.Context, userID string, err error, forbiddenMsg string) {
	if errors.Is(err, emby.ErrUnauthorized) {
		if delErr := a.Store.DeleteSessionsForUser(ctx, userID); delErr != nil {
			a.Logger.Error("delete sessions for user failed", "error", delErr)
		}
		writeError(w, http.StatusUnauthorized, "unauthorized", "your Emby session is no longer valid, please sign in again")
		return
	}
	a.Logger.Error("emby request failed", "error", err)
	writeError(w, http.StatusBadGateway, "emby_error", forbiddenMsg)
}
