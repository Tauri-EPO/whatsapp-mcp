package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

const profileUnknownEnv = "WHATSAPP_PROFILE_UNKNOWN_PER_HOUR"

const (
	defaultProfileUnknownPerHour = 30
	maxProfileUnknownPerHour     = 1000000
	profilePictureMaxBytes       = 8 << 20
	profilePictureID             = "profile-picture"
	profileUnknownLimitName      = "unknown_profile_lookups"
	profilePushNameSource        = "local_store"
	profileStatusOK              = "ok"
	profileStatusNone            = "none"
	profileStatusHidden          = "hidden_by_privacy"
	profileStatusError           = "error"
	profileRateMessage           = "WhatsApp rate limited this profile lookup; stop and report it to the operator instead of retrying"
	profileQuotaMessage          = "unknown profile lookup limit reached for this hour; stop and report it to the operator instead of retrying"
)

// profileClient is the live WhatsApp surface get_profile needs. Business
// fields go through the existing cache, which already speaks businessClient.
type profileClient interface {
	businessClient
	GetProfilePictureInfo(ctx context.Context, jid types.JID, params *whatsmeow.GetProfilePictureParams) (*types.ProfilePictureInfo, error)
}

type profileDownload func(ctx context.Context, rawURL string, limit int64) ([]byte, error)

// profileResult is the read-only profile of one phone or LID user. Hidden
// photo and about text are statuses, not failures. push_name is only ever
// copied from the local contact store.
type profileResult struct {
	Success           bool     `json:"success"`
	JID               string   `json:"jid"`
	Query             string   `json:"query,omitempty"`
	OnWhatsApp        *bool    `json:"on_whatsapp"`
	About             string   `json:"about,omitempty"`
	AboutStatus       string   `json:"about_status,omitempty"`
	PictureID         string   `json:"picture_id,omitempty"`
	PictureStatus     string   `json:"picture_status,omitempty"`
	PictureChatJID    string   `json:"picture_chat_jid,omitempty"`
	PictureMessageID  string   `json:"picture_message_id,omitempty"`
	PushName          string   `json:"push_name,omitempty"`
	PushNameSource    string   `json:"push_name_source,omitempty"`
	IsBusiness        *bool    `json:"is_business"`
	VerifiedName      string   `json:"verified_name,omitempty"`
	Categories        []string `json:"categories,omitempty"`
	Email             string   `json:"email,omitempty"`
	Address           string   `json:"address,omitempty"`
	Website           string   `json:"website,omitempty"`
	VerifiedNameError string   `json:"verified_name_error,omitempty"`
	ProfileError      string   `json:"profile_error,omitempty"`
	RateLimited       bool     `json:"rate_limited,omitempty"`
	Limit             string   `json:"limit,omitempty"`
	Message           string   `json:"message,omitempty"`
	Cached            bool     `json:"cached,omitempty"`
}

func (r profileResult) complete() bool {
	if r.RateLimited || r.OnWhatsApp == nil {
		return false
	}
	if !*r.OnWhatsApp {
		return true
	}
	return profileStatusSettled(r.PictureStatus) && profileStatusSettled(r.AboutStatus) &&
		r.VerifiedNameError == "" && r.ProfileError == ""
}

func profileStatusSettled(status string) bool {
	switch status {
	case profileStatusOK, profileStatusNone, profileStatusHidden:
		return true
	default:
		return false
	}
}

type profileEntry struct {
	result  profileResult
	expires time.Time
}

// profileCache keeps complete answers for an hour and partial ones for a
// minute. The unknown-number counter is the account's hourly ceiling and is
// not cleared when the WhatsApp client is replaced.
type profileCache struct {
	mu           sync.Mutex
	entries      map[types.JID]profileEntry
	client       profileClient
	now          func() time.Time
	unknownHour  time.Time
	unknownCount int
}

func (c *profileCache) clock() time.Time {
	if c != nil && c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *profileCache) get(client profileClient, jid types.JID) (profileResult, bool) {
	if c == nil {
		return profileResult{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if client != nil && c.client != client {
		c.entries = nil
		c.client = client
	}
	entry, ok := c.entries[jid]
	if !ok || !c.clock().Before(entry.expires) {
		return profileResult{}, false
	}
	entry.result.Cached = true
	return entry.result, true
}

func (c *profileCache) put(client profileClient, jid types.JID, result profileResult) {
	if c == nil || client == nil || jid.IsEmpty() {
		return
	}
	result.Cached = false
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != client {
		c.entries = nil
		c.client = client
	}
	if c.entries == nil {
		c.entries = map[types.JID]profileEntry{}
	}
	ttl := time.Minute
	if result.complete() {
		ttl = time.Hour
	}
	c.entries[jid] = profileEntry{result: result, expires: c.clock().Add(ttl)}
	for len(c.entries) > 256 {
		var oldest types.JID
		var expires time.Time
		for key, value := range c.entries {
			if expires.IsZero() || value.expires.Before(expires) {
				oldest, expires = key, value.expires
			}
		}
		delete(c.entries, oldest)
	}
}

// allowUnknown counts one lookup of a number this account has not seen.
// A non-positive limit disables the ceiling. The counter survives a client
// handoff: reconnecting must not reset it.
func (c *profileCache) allowUnknown(limit int) bool {
	if limit <= 0 {
		return true
	}
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	hour := c.clock().UTC().Truncate(time.Hour)
	if !c.unknownHour.Equal(hour) {
		c.unknownHour = hour
		c.unknownCount = 0
	}
	if c.unknownCount >= limit {
		return false
	}
	c.unknownCount++
	return true
}

type profileDeps struct {
	client     profileClient
	business   *businessCache
	profiles   *profileCache
	policy     chatPolicyResolver
	connected  func() bool
	onWhatsApp isOnWhatsAppFunc
	store      *MessageStore
	root       *os.Root
	account    func() *types.JID
	limit      int
	maxBytes   uint64
	download   profileDownload
}

func handleGetProfile(deps profileDeps) http.HandlerFunc {
	if deps.download == nil {
		deps.download = downloadProfilePicture
	}
	return func(w http.ResponseWriter, r *http.Request) {
		raw, err := normalizePhoneRecipient(strings.TrimSpace(r.URL.Query().Get("jid")))
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		policy := chatPolicy{}
		if deps.policy != nil {
			policy = deps.policy.effective(r.Context())
		}
		jid, ok := authorizeChat(w, policy, raw, true)
		if !ok {
			return
		}
		if jid.Server != types.DefaultUserServer && jid.Server != types.HiddenUserServer {
			writeError(w, http.StatusBadRequest, "jid must be a phone number or a phone/LID user JID")
			return
		}
		if cached, hit := deps.profiles.get(deps.client, jid); hit {
			if cached.JID != "" && cached.JID != jid.String() {
				if _, allowed := authorizeChat(w, policy, cached.JID, false); !allowed {
					return
				}
			}
			writeJSON(w, http.StatusOK, cached)
			return
		}
		if deps.client == nil || deps.connected == nil || !deps.connected() {
			writeError(w, http.StatusServiceUnavailable, "WhatsApp client is not connected")
			return
		}
		ctx, cancel := requestContext(r, actionDeadline)
		defer cancel()
		account := (*types.JID)(nil)
		if deps.account != nil {
			account = deps.account()
		}
		candidates := profileCandidates(ctx, deps.store, jid)
		if !profileKnown(ctx, deps.store, account, candidates) && !deps.profiles.allowUnknown(deps.limit) {
			bridgeLog.Warnf("unknown profile lookup limit reached for this hour")
			result := profileBase(jid)
			result.RateLimited = true
			result.Limit = profileUnknownLimitName
			result.Message = profileQuotaMessage
			result.PushName = localPushName(ctx, deps.store, account, candidates)
			writeJSON(w, http.StatusOK, result)
			return
		}
		resolved := jid
		on := true
		if jid.Server == types.DefaultUserServer {
			answer, asked, askErr := askOnWhatsApp(ctx, deps.onWhatsApp, jid.User)
			if !asked {
				result := profileBase(jid)
				result.AboutStatus = profileStatusError
				result.PictureStatus = profileStatusError
				result.PushName = localPushName(ctx, deps.store, account, candidates)
				if iqRateLimited(askErr) {
					result.RateLimited = true
					result.Message = profileRateMessage
				}
				if ctx.Err() == nil {
					deps.profiles.put(deps.client, jid, result)
				}
				writeJSON(w, http.StatusOK, result)
				return
			}
			if !answer.IsIn {
				off := false
				result := profileBase(jid)
				result.OnWhatsApp = &off
				result.AboutStatus = profileStatusNone
				result.PictureStatus = profileStatusNone
				person := false
				result.IsBusiness = &person
				result.PushName = localPushName(ctx, deps.store, account, candidates)
				if ctx.Err() == nil {
					deps.profiles.put(deps.client, jid, result)
				}
				writeJSON(w, http.StatusOK, result)
				return
			}
			if phone := registeredPhoneJID(answer); !phone.IsEmpty() {
				resolved = phone
			} else if answer.JID.Server == types.HiddenUserServer && answer.JID.User != "" {
				resolved = answer.JID.ToNonAD()
			}
			if resolved.String() != jid.String() {
				if _, allowed := authorizeChat(w, policy, resolved.String(), false); !allowed {
					return
				}
			}
		}
		result := profileBase(jid)
		result.JID = resolved.String()
		result.OnWhatsApp = &on
		info, userErr := deps.client.GetUserInfo(ctx, []types.JID{resolved})
		if iqRateLimited(userErr) {
			result.AboutStatus = profileStatusError
			result.PictureStatus = profileStatusError
			result.RateLimited = true
			result.Message = profileRateMessage
			finishProfile(w, ctx, deps, jid, resolved, account, &result, info, userErr, true)
			return
		}
		applyUserInfo(&result, resolved, info, userErr)
		picture, status, limited := fetchProfilePicture(ctx, deps.client, resolved)
		result.PictureStatus = status
		if limited {
			result.RateLimited = true
			result.Message = profileRateMessage
		}
		if picture != nil {
			if picture.ID != "" {
				result.PictureID = picture.ID
			}
			if status == profileStatusOK && picture.URL != "" && !limited {
				if err := saveProfilePicture(ctx, deps, resolved, picture.URL, &result); err != nil {
					result.PictureStatus = profileStatusError
				}
			}
		}
		stopBusiness := result.RateLimited
		finishProfile(w, ctx, deps, jid, resolved, account, &result, info, userErr, stopBusiness)
	}
}

func finishProfile(w http.ResponseWriter, ctx context.Context, deps profileDeps, query, resolved types.JID, account *types.JID, result *profileResult, info map[types.JID]types.UserInfo, userErr error, stopBusiness bool) {
	if result.RateLimited && result.Message == "" {
		result.Message = profileRateMessage
	}
	if stopBusiness {
		if deps.business != nil {
			if cached, ok := deps.business.fresh(deps.client, resolved); ok {
				applyBusiness(result, cached)
			}
		}
	} else if deps.business != nil {
		applyBusiness(result, deps.business.lookupFromUser(ctx, deps.client, resolved, info, userErr))
		if result.RateLimited && result.Message == "" {
			result.Message = profileRateMessage
		}
	}
	candidates := profileCandidates(ctx, deps.store, resolved)
	if query.String() != resolved.String() {
		candidates = append(profileCandidates(ctx, deps.store, query), candidates...)
	}
	result.PushName = localPushName(ctx, deps.store, account, candidates)
	if ctx.Err() == nil {
		deps.profiles.put(deps.client, query, *result)
		if resolved.String() != query.String() {
			deps.profiles.put(deps.client, resolved, *result)
		}
	}
	writeJSON(w, http.StatusOK, result)
}

func profileBase(jid types.JID) profileResult {
	return profileResult{
		Success:        true,
		JID:            jid.String(),
		Query:          jid.String(),
		Categories:     []string{},
		PushNameSource: profilePushNameSource,
	}
}

func applyUserInfo(result *profileResult, jid types.JID, info map[types.JID]types.UserInfo, userErr error) {
	if userErr != nil {
		result.AboutStatus = profileStatusError
		return
	}
	user, ok := info[jid]
	if !ok {
		for _, value := range info {
			user, ok = value, true
			break
		}
	}
	if !ok {
		result.AboutStatus = profileStatusNone
		return
	}
	result.PictureID = strings.TrimSpace(user.PictureID)
	about := strings.TrimSpace(user.Status)
	if about == "" {
		result.AboutStatus = profileStatusNone
		return
	}
	result.About = about
	result.AboutStatus = profileStatusOK
}

func applyBusiness(result *profileResult, business businessResult) {
	result.IsBusiness = business.IsBusiness
	result.VerifiedName = business.VerifiedName
	if business.Categories != nil {
		result.Categories = business.Categories
	}
	result.Email = business.Email
	result.Address = business.Address
	result.Website = business.Website
	result.VerifiedNameError = business.VerifiedNameError
	result.ProfileError = business.ProfileError
	if business.RateLimited {
		result.RateLimited = true
	}
}

func askOnWhatsApp(ctx context.Context, ask isOnWhatsAppFunc, user string) (types.IsOnWhatsAppResponse, bool, error) {
	if ask == nil {
		return types.IsOnWhatsAppResponse{}, false, errors.New("profile lookup is not configured")
	}
	answers, err := ask(ctx, []string{"+" + user})
	if err != nil && len(answers) == 0 {
		return types.IsOnWhatsAppResponse{}, false, err
	}
	for _, answer := range answers {
		digits := strings.TrimPrefix(strings.TrimSpace(answer.Query), "+")
		if digits == user || answer.Query == "" || len(answers) == 1 {
			return answer, true, err
		}
	}
	if err != nil {
		return types.IsOnWhatsAppResponse{}, false, err
	}
	return types.IsOnWhatsAppResponse{}, false, nil
}

func fetchProfilePicture(ctx context.Context, client profileClient, jid types.JID) (*types.ProfilePictureInfo, string, bool) {
	full, err := client.GetProfilePictureInfo(ctx, jid.ToNonAD(), &whatsmeow.GetProfilePictureParams{})
	status, limited := pictureOutcome(err, full)
	if limited || status == profileStatusHidden || status == profileStatusNone || status == profileStatusOK {
		return full, status, limited
	}
	preview, previewErr := client.GetProfilePictureInfo(ctx, jid.ToNonAD(), &whatsmeow.GetProfilePictureParams{Preview: true})
	previewStatus, previewLimited := pictureOutcome(previewErr, preview)
	if previewLimited {
		return nil, profileStatusError, true
	}
	return preview, previewStatus, false
}

func pictureOutcome(err error, info *types.ProfilePictureInfo) (string, bool) {
	if iqRateLimited(err) {
		return profileStatusError, true
	}
	if errors.Is(err, whatsmeow.ErrProfilePictureUnauthorized) || errors.Is(err, whatsmeow.ErrIQNotAuthorized) {
		return profileStatusHidden, false
	}
	if errors.Is(err, whatsmeow.ErrProfilePictureNotSet) || errors.Is(err, whatsmeow.ErrIQNotFound) {
		return profileStatusNone, false
	}
	if err != nil {
		return profileStatusError, false
	}
	if info == nil || strings.TrimSpace(info.URL) == "" {
		return profileStatusNone, false
	}
	return profileStatusOK, false
}

func iqRateLimited(err error) bool {
	var iq *whatsmeow.IQError
	return errors.As(err, &iq) && iq != nil && iq.Code == 429
}

func saveProfilePicture(ctx context.Context, deps profileDeps, jid types.JID, rawURL string, result *profileResult) error {
	data, err := deps.download(ctx, rawURL, profilePictureLimit(deps.maxBytes))
	if err != nil || len(data) == 0 {
		return errors.New("profile picture download failed")
	}
	if _, err := storeProfilePicture(deps.root, jid, time.Now(), data); err != nil {
		return err
	}
	result.PictureStatus = profileStatusOK
	result.PictureChatJID = jid.String()
	result.PictureMessageID = profilePictureID
	return nil
}

func profilePictureLimit(maxBytes uint64) int64 {
	// A zero ceiling means the deployment set no media cap, so the picture cap
	// applies. Anything that does not fit in int64, or is at least the picture
	// cap, uses that same cap. The range check is what makes the conversion
	// of the remaining value defined.
	if maxBytes == 0 || maxBytes > uint64(math.MaxInt64) || maxBytes >= profilePictureMaxBytes {
		return profilePictureMaxBytes
	}
	return int64(maxBytes)
}

func storeProfilePicture(root *os.Root, chat types.JID, now time.Time, data []byte) (string, error) {
	if root == nil {
		return "", errors.New("profile picture store is not configured")
	}
	dir := chatMediaRel(chat.String())
	if err := checkMediaPathComponents(dir, profilePictureID+".jpg"); err != nil {
		return "", err
	}
	if err := root.MkdirAll(dir, storeDirMode); err != nil {
		return "", err
	}
	if _, err := requireChatMediaDir(root, dir); err != nil {
		return "", err
	}
	name := mediaFileName("image", now, profilePictureID, "")
	rel := path.Join(dir, name)
	if _, err := writeMediaFile(root, rel, func(f *os.File) error {
		_, err := f.Write(data)
		return err
	}); err != nil {
		return "", err
	}
	entries, err := fs.ReadDir(root.FS(), dir)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == name {
			continue
		}
		if cachedProfileID(entry.Name()) == profilePictureID {
			if err := root.Remove(path.Join(dir, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return "", err
			}
		}
	}
	return name, nil
}

func cachedProfileID(name string) string {
	base := path.Base(name)
	stem := strings.TrimSuffix(base, path.Ext(base))
	parts := strings.SplitN(stem, "_", 4)
	if len(parts) != 4 {
		return ""
	}
	return parts[3]
}

func profileCandidates(ctx context.Context, store *MessageStore, jid types.JID) []types.JID {
	seen := map[string]bool{}
	var out []types.JID
	add := func(id types.JID) {
		id = id.ToNonAD()
		if id.User == "" || id.Server == "" || seen[id.String()] {
			return
		}
		seen[id.String()] = true
		out = append(out, id)
	}
	add(jid)
	if jid.Server == types.DefaultUserServer {
		if alt := brMobileAlternate(jid.User); alt != "" {
			add(types.NewJID(alt, types.DefaultUserServer))
		}
	}
	if store == nil || store.waDB == nil {
		return out
	}
	rows, err := store.waDB.QueryContext(ctx, `SELECT lid, pn FROM whatsmeow_lid_map WHERE lid=? OR pn=?`, jid.User, jid.User)
	if err != nil {
		return out
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var lid, pn string
		if rows.Scan(&lid, &pn) != nil {
			continue
		}
		if lid != "" {
			add(types.NewJID(lid, types.HiddenUserServer))
		}
		if pn != "" {
			add(types.NewJID(pn, types.DefaultUserServer))
		}
	}
	if rows.Err() != nil {
		return out
	}
	return out
}

func profileKnown(ctx context.Context, store *MessageStore, account *types.JID, candidates []types.JID) bool {
	if store == nil || store.db == nil {
		return false
	}
	for _, id := range candidates {
		var one int
		if err := store.db.QueryRowContext(ctx, `SELECT 1 FROM chats WHERE jid=? LIMIT 1`, id.String()).Scan(&one); err == nil {
			return true
		}
	}
	if store.waDB == nil {
		return false
	}
	for _, id := range candidates {
		if contactRowExists(ctx, store.waDB, account, id) {
			return true
		}
	}
	return false
}

func localPushName(ctx context.Context, store *MessageStore, account *types.JID, candidates []types.JID) string {
	if store == nil || store.waDB == nil {
		return ""
	}
	for _, id := range candidates {
		name, ok := contactPushName(ctx, store.waDB, account, id)
		if ok && name != "" {
			return name
		}
	}
	return ""
}

func contactRowExists(ctx context.Context, db *sql.DB, account *types.JID, id types.JID) bool {
	var one int
	var err error
	if account != nil && account.String() != "" {
		err = db.QueryRowContext(ctx, `SELECT 1 FROM whatsmeow_contacts WHERE their_jid=? AND our_jid=? LIMIT 1`, id.String(), account.String()).Scan(&one)
	} else {
		err = db.QueryRowContext(ctx, `SELECT 1 FROM whatsmeow_contacts WHERE their_jid=? LIMIT 1`, id.String()).Scan(&one)
	}
	return err == nil
}

func contactPushName(ctx context.Context, db *sql.DB, account *types.JID, id types.JID) (string, bool) {
	var push sql.NullString
	var err error
	if account != nil && account.String() != "" {
		err = db.QueryRowContext(ctx, `SELECT push_name FROM whatsmeow_contacts WHERE their_jid=? AND our_jid=? LIMIT 1`, id.String(), account.String()).Scan(&push)
	} else {
		err = db.QueryRowContext(ctx, `SELECT push_name FROM whatsmeow_contacts WHERE their_jid=? LIMIT 1`, id.String()).Scan(&push)
	}
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(push.String), true
}

func downloadProfilePicture(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, errors.New("profile picture url refused")
	}
	if err := validateProfilePictureURL(parsed); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, errors.New("profile picture size limit refused")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, errors.New("profile picture download failed")
	}
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, Control: refusePrivateProfileDial}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 15 * time.Second,
			DisableKeepAlives:     true,
		},
		CheckRedirect: func(redirect *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("profile picture redirect limit")
			}
			return validateProfilePictureURL(redirect.URL)
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("profile picture download failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("profile picture download failed")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, errors.New("profile picture download failed")
	}
	if int64(len(body)) > limit || len(body) == 0 {
		return nil, errors.New("profile picture exceeds the size limit")
	}
	return body, nil
}

func validateProfilePictureURL(target *url.URL) error {
	if target == nil || target.Scheme != "https" || target.Host == "" || target.User != nil {
		return errors.New("profile picture url refused")
	}
	return nil
}

func refusePrivateProfileDial(network, address string, _ syscall.RawConn) error {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return errors.New("profile picture address refused")
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("profile picture address refused")
	}
	ip := net.ParseIP(host)
	if !profilePictureIPAllowed(ip) {
		return errors.New("profile picture address refused")
	}
	return nil
}

func profilePictureIPAllowed(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsGlobalUnicast() && !ip.IsPrivate()
}

func resolveProfileUnknownPerHour(value string) (int, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return defaultProfileUnknownPerHour, nil
	}
	n, err := strconv.Atoi(trimmed)
	if err != nil || n < 0 || n > maxProfileUnknownPerHour {
		return 0, fmt.Errorf("invalid %s=%q: expected a whole number from 0 to 1000000 (0 disables the hourly ceiling)", profileUnknownEnv, configValue(value))
	}
	return n, nil
}
