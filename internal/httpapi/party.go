package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/beechfuzz/watch-party/internal/dbx"
	"github.com/beechfuzz/watch-party/internal/emby"
	"github.com/beechfuzz/watch-party/internal/idgen"
	"github.com/beechfuzz/watch-party/internal/party"
	"github.com/beechfuzz/watch-party/internal/wsproto"
)

// settingsRequest carries the Party Settings form's four end-of-media
// fields. Pointer fields so handleCreateParty can tell "omitted" (fall back
// to the default matching the form's own default state) from "explicitly
// false/0". handleUpdatePartySettings, by contrast, treats every field as
// required — that request is always a full form submission, not a partial
// patch.
type settingsRequest struct {
	AutoAdvance          *bool `json:"auto_advance"`
	ShowNextDialog       *bool `json:"show_next_dialog"`
	AutoplayEnabled      *bool `json:"autoplay_enabled"`
	AutoplayDelaySeconds *int  `json:"autoplay_delay_seconds"`
}

// defaultSettings matches the Party Settings form's own default state (and
// the migration's SQL column defaults) — all three checkboxes checked, a
// 5-second delay.
func defaultSettings() party.Settings {
	return party.Settings{AutoAdvance: true, ShowNextDialog: true, AutoplayEnabled: true, AutoplayDelaySeconds: 5}
}

func (r settingsRequest) toSettings() party.Settings {
	s := defaultSettings()
	if r.AutoAdvance != nil {
		s.AutoAdvance = *r.AutoAdvance
	}
	if r.ShowNextDialog != nil {
		s.ShowNextDialog = *r.ShowNextDialog
	}
	if r.AutoplayEnabled != nil {
		s.AutoplayEnabled = *r.AutoplayEnabled
	}
	if r.AutoplayDelaySeconds != nil {
		s.AutoplayDelaySeconds = *r.AutoplayDelaySeconds
	}
	return s
}

type createPartyRequest struct {
	Name string `json:"name"`
	settingsRequest
}

// partySummary is one entry in the GET /api/parties listing — enough for
// the Home page's Active Parties / Your Parties cards without a second
// round-trip per party. There's no per-party access-control list; visibility
// here is simply "every currently active party" (this is a small,
// friends/family self-hosted tool, not a multi-tenant service), same as
// join-by-link already implicitly allowed before this endpoint existed.
type partySummary struct {
	PartyID         string `json:"party_id"`
	Name            string `json:"name"`
	ItemID          string `json:"item_id,omitempty"`
	ItemTitle       string `json:"item_title,omitempty"`
	HostUserID      string `json:"host_user_id"`
	HostDisplayName string `json:"host_display_name"`
	IsPlaying       bool   `json:"is_playing"`
	PositionTicks   int64  `json:"position_ticks"`
	DurationTicks   int64  `json:"duration_ticks"`
	MemberCount     int    `json:"member_count"`
}

// handleListParties powers the Home page's Active Parties / Your Parties
// sections. A party's display name is now user-chosen at creation (it no
// longer has a mandatory Emby item to derive a title from) — the current
// item's title, when there is one, is still resolved via the requesting
// user's own Emby token (never a shared one). Unlike the old single-item
// design, a viewer who can't access the current item no longer causes the
// whole party row to be omitted (the party itself isn't Emby-gated content;
// only playback of a specific item is) — it's shown with a generic
// "Restricted item" placeholder instead, matching the playlist listing's
// per-viewer resolution below.
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
		snap := p.Snapshot()

		itemTitle := ""
		if snap.ItemID != "" {
			if item, err := a.Emby.GetItem(r.Context(), token, user.ID, snap.ItemID); err == nil {
				itemTitle = item.Name
			} else {
				itemTitle = "Restricted item"
			}
		}

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
			PartyID: p.ID, Name: p.Name, ItemID: snap.ItemID, ItemTitle: itemTitle,
			HostUserID: snap.HostUserID, HostDisplayName: hostDisplayName,
			IsPlaying: snap.AuthoritativeState.IsPlaying, PositionTicks: p.CurrentPositionTicks(),
			DurationTicks: snap.DurationTicks, MemberCount: memberCount,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"parties": summaries})
}

// handleCreateParty creates a party with a user-chosen name and an empty
// playlist — no Emby item is required (or even possible) up front anymore;
// media is added afterward through the playlist endpoints below.
func (a *App) handleCreateParty(w http.ResponseWriter, r *http.Request) {
	var req createPartyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "name is required")
		return
	}
	settings := req.settingsRequest.toSettings()
	if settings.AutoplayDelaySeconds <= 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "autoplay_delay_seconds must be positive")
		return
	}
	user := userFromContext(r.Context())

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
	if _, err := a.Hub.CreateParty(r.Context(), partyID, req.Name, settings, user.ID); err != nil {
		a.Logger.Error("create party failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"party_id":     partyID,
		"name":         req.Name,
		"host_user_id": user.ID,
	})
}

type updatePartySettingsRequest struct {
	Name                 string `json:"name"`
	AutoAdvance          bool   `json:"auto_advance"`
	ShowNextDialog       bool   `json:"show_next_dialog"`
	AutoplayEnabled      bool   `json:"autoplay_enabled"`
	AutoplayDelaySeconds int    `json:"autoplay_delay_seconds"`
}

// handleUpdatePartySettings is the Party Settings form's post-creation save
// path (the cog button on the party page) — host-only, like every other
// party mutation. Unlike handleAddPlaylistItem there's no Emby round trip to
// avoid here, so the host check lives entirely in Party.UpdateSettings
// (still fast/IO-free before its own SQLite write — see its doc comment).
func (a *App) handleUpdatePartySettings(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	user := userFromContext(r.Context())
	var req updatePartySettingsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "name is required")
		return
	}
	if req.AutoplayDelaySeconds <= 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "autoplay_delay_seconds must be positive")
		return
	}

	p, ok := a.Hub.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "party not found or already ended")
		return
	}
	settings := party.Settings{
		AutoAdvance: req.AutoAdvance, ShowNextDialog: req.ShowNextDialog,
		AutoplayEnabled: req.AutoplayEnabled, AutoplayDelaySeconds: req.AutoplayDelaySeconds,
	}
	if err := p.UpdateSettings(r.Context(), user.ID, req.Name, settings); err != nil {
		a.writeHostActionErr(w, err) // ErrNotHost/ErrPartyEnded mapping already covers this case
		return
	}
	w.WriteHeader(http.StatusNoContent)
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

	var snap wsproto.SnapshotPayload
	if p, ok := a.Hub.Get(id); ok {
		snap = p.Snapshot()
	} else {
		// Active-in-DB but not in the hub shouldn't normally happen (active
		// parties are recovered at startup), but degrade gracefully rather
		// than 500 if it ever does.
		snap = wsproto.SnapshotPayload{PartyID: row.ID, Name: row.Name, HostUserID: row.HostUserID}
	}

	// Media authorization must be re-validated on join, not just at party
	// creation — confirm this specific user (not just the host) can access
	// the CURRENT item before they can see/join the party. With a playlist,
	// the current item can change after join (see ARCHITECTURE.md's
	// Playlist section) — an idle party (no current item yet) has nothing
	// to authorize against, so this check only runs once there is one; a
	// per-participant re-check against whatever becomes current later
	// happens naturally at handlePlaybackURL, which every client calls
	// again whenever the current item changes.
	itemTitle := ""
	if snap.ItemID != "" {
		token, err := a.TokenCipher.Decrypt(user.EncryptedAccessToken)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
		item, err := a.Emby.GetItem(r.Context(), token, user.ID, snap.ItemID)
		if err != nil {
			a.handleEmbyErr(w, r.Context(), user.ID, err, "you do not have access to this media item")
			return
		}
		itemTitle = item.Name
	}

	// ItemTitle rides alongside the snapshot rather than joining
	// wsproto.SnapshotPayload itself — the party actor has no Emby access
	// (by design; see ARCHITECTURE.md §3) and so has no way to populate a
	// title. Name, unlike ItemTitle, IS part of SnapshotPayload (so a host
	// rename propagates live to already-connected clients via the ordinary
	// snapshot broadcast, not just this HTTP response).
	writeJSON(w, http.StatusOK, struct {
		wsproto.SnapshotPayload
		ItemTitle string `json:"item_title"`
	}{SnapshotPayload: snap, ItemTitle: itemTitle})
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
	if row.CurrentPlaylistItemID == nil {
		writeError(w, http.StatusConflict, "no_current_item", "no media is currently loaded for this party")
		return
	}

	// This is also the point every client re-fetches whenever the current
	// item changes (playlist advance, or a host selecting a different
	// item), which makes it the per-participant, per-item re-validation
	// point the spec's authorization rule requires now that a party's
	// current item can change after join — see ARCHITECTURE.md's Playlist
	// section. Emby's own PlaybackInfo call, below, does the actual
	// rejection: a user who can't see this specific item gets an error
	// here (surfaced to only this client), not a party-wide side effect.
	current, err := a.Store.GetPlaylistItem(r.Context(), *row.CurrentPlaylistItemID)
	if err != nil {
		if errors.Is(err, dbx.ErrNotFound) {
			writeError(w, http.StatusConflict, "no_current_item", "no media is currently loaded for this party")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
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
	result, err := a.Emby.GetPlaybackURL(r.Context(), token, user.ID, current.ItemID, emby.DeviceID(user.ID), startPositionTicks)
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

// --- playlist ---

// playlistItemView is one entry in GET /api/parties/{id}/playlist — the raw
// playlist_items row plus Emby-resolved metadata under the REQUESTING
// viewer's own token (never a shared one). An item that viewer can't access
// is shown as a generic "Restricted item" placeholder rather than omitted
// outright (an omission would leave an unexplained gap in the order) or
// shown with its real title (which would leak metadata about content that
// viewer isn't authorized to know exists) — see ARCHITECTURE.md's Playlist
// section.
type playlistItemView struct {
	ID            int64  `json:"id"`
	ItemID        string `json:"item_id"`
	Title         string `json:"title"`
	Restricted    bool   `json:"restricted"`
	PosterURL     string `json:"poster_url,omitempty"`
	DurationTicks int64  `json:"duration_ticks"`
	Position      int    `json:"position"`
	AddedByUserID string `json:"added_by_user_id"`
	IsCurrent     bool   `json:"is_current"`
}

func (a *App) handleGetPlaylist(w http.ResponseWriter, r *http.Request) {
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

	items, err := a.Store.ListPlaylistItems(r.Context(), id)
	if err != nil {
		a.Logger.Error("list playlist items failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	token, err := a.TokenCipher.Decrypt(user.EncryptedAccessToken)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}

	out := make([]playlistItemView, 0, len(items))
	for _, it := range items {
		view := playlistItemView{
			ID: it.ID, ItemID: it.ItemID, DurationTicks: it.DurationTicks,
			Position: it.Position, AddedByUserID: it.AddedByUserID,
			IsCurrent: row.CurrentPlaylistItemID != nil && *row.CurrentPlaylistItemID == it.ID,
		}
		if embyItem, err := a.Emby.GetItem(r.Context(), token, user.ID, it.ItemID); err == nil {
			view.Title = embyItem.Name
			view.PosterURL = a.Emby.ImageURL(it.ItemID, token)
		} else {
			view.Restricted = true
			view.Title = "Restricted item"
		}
		out = append(out, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

type addPlaylistItemRequest struct {
	ItemID string `json:"item_id"`
}

// handleAddPlaylistItem is host-only (enforced by Party.AddPlaylistItem).
// The GetItem call below serves double duty: it's how duration_ticks gets
// fetched and persisted (the party actor has no Emby access of its own),
// and it's the adding host's own authorization check for this item —
// consistent with "being host never implicitly grants anyone else access":
// it only proves the HOST can see this item, not that every other
// participant can, which is why the real per-participant check happens
// again at handlePlaybackURL whenever this item becomes current.
func (a *App) handleAddPlaylistItem(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	user := userFromContext(r.Context())
	var req addPlaylistItemRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ItemID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "item_id is required")
		return
	}

	p, ok := a.Hub.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "party not found or already ended")
		return
	}
	// Fail fast on host authorization before spending a token decrypt and an
	// Emby round trip on a request that's going to be rejected regardless —
	// Party.AddPlaylistItem is still the authoritative check (this is a
	// cheap up-front read, not a replacement for it).
	if p.HostUserID() != user.ID {
		writeError(w, http.StatusForbidden, "not_host", "only the host may edit the playlist")
		return
	}

	token, err := a.TokenCipher.Decrypt(user.EncryptedAccessToken)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	item, err := a.Emby.GetItem(r.Context(), token, user.ID, req.ItemID)
	if err != nil {
		a.handleEmbyErr(w, r.Context(), user.ID, err, "could not look up that item on Emby")
		return
	}

	added, err := p.AddPlaylistItem(r.Context(), user.ID, req.ItemID, item.RunTimeTicks)
	if err != nil {
		a.writeHostActionErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": added.ID, "item_id": added.ItemID, "position": added.Position, "duration_ticks": added.DurationTicks,
	})
}

func (a *App) handleRemovePlaylistItem(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	playlistItemID, err := strconv.ParseInt(r.PathValue("itemId"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid playlist item id")
		return
	}
	user := userFromContext(r.Context())
	p, ok := a.Hub.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "party not found or already ended")
		return
	}
	if err := p.RemovePlaylistItem(r.Context(), user.ID, playlistItemID); err != nil {
		a.writeHostActionErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type reorderPlaylistRequest struct {
	OrderedIDs []int64 `json:"ordered_ids"`
}

func (a *App) handleReorderPlaylist(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	user := userFromContext(r.Context())
	var req reorderPlaylistRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "ordered_ids is required")
		return
	}
	p, ok := a.Hub.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "party not found or already ended")
		return
	}
	if err := p.ReorderPlaylist(r.Context(), user.ID, req.OrderedIDs); err != nil {
		a.writeHostActionErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSelectPlaylistItem is the host-only "play this now" action —
// Party.SelectCurrentItem both loads and starts the item in one step (see
// its doc comment for why there's no separate "load paused" step).
func (a *App) handleSelectPlaylistItem(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	playlistItemID, err := strconv.ParseInt(r.PathValue("itemId"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid playlist item id")
		return
	}
	user := userFromContext(r.Context())
	p, ok := a.Hub.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "party not found or already ended")
		return
	}
	if err := p.SelectCurrentItem(user.ID, playlistItemID); err != nil {
		a.writeHostActionErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) writeHostActionErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, party.ErrNotHost):
		writeError(w, http.StatusForbidden, "not_host", "only the host may edit the playlist")
	case errors.Is(err, party.ErrPartyEnded):
		writeError(w, http.StatusGone, "party_ended", "this party has ended")
	case errors.Is(err, party.ErrCannotRemoveCurrentItem):
		writeError(w, http.StatusConflict, "cannot_remove_current_item", "cannot remove the currently-playing item")
	case errors.Is(err, party.ErrPlaylistItemNotFound):
		writeError(w, http.StatusNotFound, "not_found", "playlist item not found")
	case errors.Is(err, party.ErrInvalidReorder):
		writeError(w, http.StatusBadRequest, "bad_request", "ordered_ids must match the current playlist exactly")
	default:
		a.Logger.Error("playlist operation failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
	}
}

// handleBrowseEmbyItems is the Playlist library browser's search/browse
// endpoint. Deliberately routed through this backend (using the requester's
// own token) rather than direct browser-to-Emby, unlike the media/playback
// URL — see ARCHITECTURE.md's Playlist section for why the two cases are
// architecturally different.
func (a *App) handleBrowseEmbyItems(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r.Context())
	token, err := a.TokenCipher.Decrypt(user.EncryptedAccessToken)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}

	q := emby.ItemsQuery{Search: r.URL.Query().Get("search"), Tab: r.URL.Query().Get("tab")}
	if start := r.URL.Query().Get("start"); start != "" {
		if n, err := strconv.Atoi(start); err == nil {
			q.Start = n
		}
	}

	items, err := a.Emby.ListItems(r.Context(), token, user.ID, q)
	if err != nil {
		a.handleEmbyErr(w, r.Context(), user.ID, err, "could not browse the Emby library")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
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
