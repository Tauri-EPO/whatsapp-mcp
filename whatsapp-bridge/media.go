package main

// Inbound media download: the whatsmeow DownloadableMessage adapter, the
// store/<chat>/ file layout and the media-retry fallback (media_retry.go).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"go.mau.fi/whatsmeow"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// DownloadMediaRequest represents the request body for the download media API
type DownloadMediaRequest struct {
	MessageID string `json:"message_id"`
	ChatJID   string `json:"chat_jid"`
}

// DownloadMediaResponse represents the response for the download media API
type DownloadMediaResponse struct {
	Success  bool   `json:"success"`
	Message  string `json:"message"`
	Filename string `json:"filename,omitempty"`
	Path     string `json:"path,omitempty"`
}

// MediaDownloader implements the whatsmeow.DownloadableMessage interface
type MediaDownloader struct {
	URL           string
	DirectPath    string
	MediaKey      []byte
	FileLength    uint64
	FileSHA256    []byte
	FileEncSHA256 []byte
	MediaType     whatsmeow.MediaType
}

// GetDirectPath implements the DownloadableMessage interface
func (d *MediaDownloader) GetDirectPath() string {
	return d.DirectPath
}

// GetURL implements the DownloadableMessage interface
func (d *MediaDownloader) GetURL() string {
	return d.URL
}

// GetMediaKey implements the DownloadableMessage interface
func (d *MediaDownloader) GetMediaKey() []byte {
	return d.MediaKey
}

// GetFileLength implements the DownloadableMessage interface
func (d *MediaDownloader) GetFileLength() uint64 {
	return d.FileLength
}

// GetFileSHA256 implements the DownloadableMessage interface
func (d *MediaDownloader) GetFileSHA256() []byte {
	return d.FileSHA256
}

// GetFileEncSHA256 implements the DownloadableMessage interface
func (d *MediaDownloader) GetFileEncSHA256() []byte {
	return d.FileEncSHA256
}

// GetMediaType implements the DownloadableMessage interface
func (d *MediaDownloader) GetMediaType() whatsmeow.MediaType {
	return d.MediaType
}

// errMediaUnavailable marks a download that no later request can turn into
// bytes, whatever the cause: the sender's phone was asked to re-upload and said
// it no longer has the file (media_retry.go, issue #378), or the row never
// carried the CDN fields a download needs (issue #392). A phone that is merely
// offline, a disconnected bridge and a CDN that timed out are not this — those
// are worth retrying. Callers (handleDownload, and through it the MCP server's
// ingest worker) use it to record the miss once instead of asking for the same
// dead file on every pass over the archive.
var errMediaUnavailable = errors.New("the sender's phone no longer has this media")

// errMediaRefused is an immutable row identity that cannot safely name a file.
// Unlike errMediaUnavailable this says nothing about another copy's bytes.
var errMediaRefused = errors.New("media path permanently refused")

func permanentMediaCode(err error) string {
	if errors.Is(err, errMediaRefused) {
		return "media_refused"
	}
	if errors.Is(err, errMediaUnavailable) {
		return "media_unavailable"
	}
	return ""
}

// permanentMediaError is an errMediaUnavailable that keeps its own wording.
// The two causes are equally final but not interchangeable to whoever reads the
// answer — "the phone declined the retry" is a file that once existed, "the
// media information is incomplete" is a row this bridge never captured the keys
// for — so the message stays the cause and errors.Is still sees the sentinel.
type permanentMediaError string

func (e permanentMediaError) Error() string { return string(e) }
func (permanentMediaError) Unwrap() error   { return errMediaUnavailable }

// Function to download media from a message
func (b *Bridge) downloadMedia(ctx context.Context, messageID, chatJID string) (bool, string, string, string, error) {
	for {
		ok, kind, name, path, err := b.downloadMediaAttempt(ctx, messageID, chatJID)
		if mediaLimit(ctx) != 0 || !errors.Is(err, errAutoMediaLimit) || ctx.Err() != nil {
			return ok, kind, name, path, err
		}
		// An uncapped waiter retries after the capped starter cleans up.
		// Re-read metadata too: a CDN retry may have refreshed its credentials.
	}
}

func (b *Bridge) downloadMediaAttempt(ctx context.Context, messageID, chatJID string) (bool, string, string, string, error) {
	messageStore := b.Store
	// Query the database for the message including timestamp
	var mediaType, url string
	var originalName, storedDirectPath sql.NullString
	var mediaKey, fileSHA256, fileEncSHA256 []byte
	var fileLength sql.NullInt64
	var timestamp time.Time
	var err error

	// Get media info AND timestamp from the database
	err = messageStore.db.QueryRow(
		"SELECT COALESCE(media_type, ''), COALESCE(url, ''), media_key, file_sha256, file_enc_sha256, file_length, timestamp, filename, direct_path FROM messages WHERE id = ? AND chat_jid = ?",
		messageID, chatJID,
	).Scan(&mediaType, &url, &mediaKey, &fileSHA256, &fileEncSHA256, &fileLength, &timestamp, &originalName, &storedDirectPath)

	if err != nil {
		return false, "", "", "", fmt.Errorf("failed to find message: %v", err)
	}

	length, lengthErr := mediaLengthValue(fileLength)
	if lengthErr != nil {
		return false, "", "", "", lengthErr
	}

	// Check if this is a media message
	if mediaType == "" || mediaType == "location" {
		return false, "", "", "", fmt.Errorf("not a media message")
	}
	if mediaType != "image" && mediaType != "video" && mediaType != "audio" && mediaType != "document" && mediaType != "sticker" {
		return false, "", "", "", fmt.Errorf("unsupported media type: %s", mediaType)
	}

	filename := mediaFileName(mediaType, timestamp, messageID, originalName.String)
	chatDir := chatMediaRel(chatJID)

	// The chat JID and the message ID are whatever the sending client put in
	// the stanza, and both end up in the path. Refuse one that is more than a
	// single path component before anything touches the disk (issue #453).
	if err := checkMediaPathComponents(chatDir, filename); err != nil {
		b.Log.Warnf("Refusing to cache media for message %q in chat %q: %v", messageID, chatJID, err)
		b.metrics.mediaRefusals.Add(1)
		return false, "", "", "", fmt.Errorf("%w: %w", errMediaRefused, err)
	}
	// Everything below — the directory, the cache lookup, the temp file and the
	// rename — goes through the store root, so the kernel keeps it inside the
	// store even when a path component was replaced by a symlink.
	root := b.StoreRoot
	if root == nil {
		return false, "", "", "", errors.New("store directory unavailable")
	}

	// Create directory for the chat if it doesn't exist. A chat directory that
	// is a symlink out of the store fails here instead of receiving the file;
	// one that links to another directory of the store passes MkdirAll, and a
	// chat's media belongs in a directory of its own, so that is refused next.
	if err := root.MkdirAll(chatDir, storeDirMode); err != nil {
		return false, "", "", "", fmt.Errorf("failed to create chat directory inside the store: %v", err)
	}
	if _, err := requireChatMediaDir(root, chatDir); err != nil {
		return false, "", "", "", errChatDirNotReal
	}

	// The file's path relative to the store root, and the absolute one callers get
	relPath := path.Join(chatDir, filename)
	absPath, err := filepath.Abs(storePath(chatDir, filename))
	if err != nil {
		return false, "", "", "", fmt.Errorf("failed to get absolute path: %v", err)
	}

	// Check if file already exists (under the current or the legacy name)
	cached, lookupErr := cachedMediaPath(root, chatDir, mediaType, timestamp, messageID, originalName.String)
	if lookupErr != nil {
		// Something stands under the cached name, or in front of the directory,
		// that the lookup will not go through. Say so: the fetch that follows
		// would otherwise read as an ordinary cache miss.
		b.Log.Warnf("Cache lookup for message %q in chat %q was refused (%v); downloading the file again", messageID, chatJID, lookupErr)
	}
	if cached != "" {
		if err := checkCachedMediaLimit(ctx, root, cached); err != nil {
			return false, "", "", "", err
		}
		absPath = filepath.Join(filepath.Dir(absPath), path.Base(cached))
		b.Log.Debugf("📁 File already exists: %s", absPath)
		return true, mediaType, filepath.Base(absPath), absPath, nil
	}

	// If we don't have all the media info we need, we can't download, and no
	// request can change that: the media-retry protocol hands back a fresh
	// path, never the key the file is decrypted with. Report it as permanent
	// (errMediaUnavailable) so the caller records the miss once instead of
	// asking on every pass over the archive (issue #392). What *can* change it
	// is a later history sync storing the same message with its media info; the
	// caller clears its note to ask again then, exactly as it does for a phone
	// that restored a backup.
	if !mediaComplete(url, storedDirectPath.String, mediaKey, fileSHA256, fileEncSHA256) {
		return false, "", "", "", permanentMediaError("incomplete media information for download")
	}

	b.Log.Debugf("Attempting to download media for message %s in chat %s...", messageID, chatJID)

	// whatsmeow downloads by direct path alone. The message's own one is used
	// when the row has it; a row written before the column existed, or for a
	// message that carried none, keeps the path cut out of the URL.
	directPath, pathSource := storedDirectPath.String, pathFromMessage
	if directPath == "" {
		directPath, pathSource = extractDirectPathFromURL(url), pathFromURL
	} else if !strings.HasPrefix(directPath, "/") {
		directPath = "/" + directPath
	}

	// Create a downloader that implements DownloadableMessage
	var waMediaType whatsmeow.MediaType
	switch mediaType {
	case "image":
		waMediaType = whatsmeow.MediaImage
	case "video":
		waMediaType = whatsmeow.MediaVideo
	case "audio":
		waMediaType = whatsmeow.MediaAudio
	case "document":
		waMediaType = whatsmeow.MediaDocument
	case "sticker":
		// whatsmeow derives sticker decryption keys from the image HKDF info string
		// (see download.go: classToMediaType maps "StickerMessage" -> MediaImage).
		waMediaType = whatsmeow.MediaImage
	default:
		return false, "", "", "", fmt.Errorf("unsupported media type: %s", mediaType)
	}

	downloader := &MediaDownloader{
		URL:           url,
		DirectPath:    directPath,
		MediaKey:      mediaKey,
		FileLength:    length,
		FileSHA256:    fileSHA256,
		FileEncSHA256: fileEncSHA256,
		MediaType:     waMediaType,
	}

	// Stream straight to a temp file next to the target (whatsmeow decrypts
	// and verifies in place), then rename: no full copy of the media in RAM
	// and no half-written file ever appears under the final name.
	//
	// One transfer per destination file: REST, auto-download and an MCP fetch
	// can miss the cache for the same message at once and would otherwise
	// share (and truncate) the same "<file>.part". The first caller starts the
	// transfer, every caller waits for its result (media_inflight.go). Counters
	// and the success log therefore count transfers, not callers.
	if _, err := b.mediaTransfers.do(ctx, absPath, func() (int64, error) {
		// Deliberately shadowed: everything below runs under the detached
		// transfer context, never under the context of one of the callers.
		ctx, cancel := transferContext(b.ctx, ctx)
		defer cancel()
		written, err := b.transferMedia(ctx, downloader, relPath)
		if status := cdnRefusalStatus(err); status != 0 {
			// What the next report has to be read from: the status, how old
			// the message is and the shape of what was asked, never a token.
			age := time.Since(timestamp)
			b.Log.Warnf("CDN refused media for message %s with HTTP %d: message age %s, %s", messageID, status,
				age.Round(time.Second), mediaRequestShape(url, directPath, pathSource, mediaKey, fileSHA256, fileEncSHA256))
			// The path this bridge asked for before it kept the message's own
			// one. Tried once when it names something else, so nothing that
			// downloaded then fails now, and the log says which of the two works.
			if alt := extractDirectPathFromURL(url); pathSource == pathFromMessage && alt != directPath && strings.HasPrefix(alt, "/") {
				viaURL := *downloader
				viaURL.DirectPath = alt
				if n, altErr := b.transferMedia(ctx, &viaURL, relPath); altErr == nil {
					b.Log.Warnf("Message %s was downloaded through the path cut out of its url after its direct path was refused", messageID)
					written, err = n, nil
				} else if errors.Is(altErr, errAutoMediaLimit) {
					err = altErr
				}
			}
			switch {
			case err == nil || errors.Is(err, errAutoMediaLimit):
			case age < cdnFreshWindow && !linkExpired(directPath, time.Now()):
				// Too young for its link to have expired, and the link does
				// not say it has: the request itself is what the CDN refuses,
				// so the sender's phone is not asked for a new upload and
				// nothing is concluded about the file.
				b.metrics.mediaDownloadFails.Add(1)
				return 0, &cdnRefusedError{status: status, age: age}
			default:
				// Old enough for the link to have expired (old history, a file
				// that sat unread), or a link stamped as expired: ask the
				// sender's phone to re-upload and download from the fresh
				// path. See media_retry.go.
				b.Log.Warnf("Requesting a media retry from the sender's phone for message %s...", messageID)
				written, err = b.retryMedia(ctx, messageID, chatJID, downloader, root, relPath)
			}
		}
		if err != nil {
			if !errors.Is(err, errAutoMediaLimit) {
				b.metrics.mediaDownloadFails.Add(1)
			}
			return 0, err
		}
		b.metrics.mediaDownloads.Add(1)
		b.Log.Infof("Successfully downloaded %s media to %s (%d bytes)", mediaType, absPath, written)
		return written, nil
	}); err != nil {
		// %w, not %v: handleDownload asks errors.Is whether the sender's phone
		// answered "gone" (errMediaUnavailable), and %v would cut that chain.
		return false, "", "", "", fmt.Errorf("failed to download media: %w", err)
	}
	if err := checkCachedMediaLimit(ctx, root, relPath); err != nil {
		return false, "", "", "", err
	}
	return true, mediaType, filename, absPath, nil
}

// transferMedia streams one media file into relPath (relative to the store
// root) through its temp file. Tests override Bridge.mediaTransfer to run a
// fake transfer.
func (b *Bridge) transferMedia(ctx context.Context, msg whatsmeow.DownloadableMessage, relPath string) (int64, error) {
	if b.mediaTransfer != nil {
		return b.mediaTransfer(ctx, msg, relPath)
	}
	return downloadToPath(ctx, b.StoreRoot, b.currentClient(), msg, relPath)
}

// mediaComplete reports whether a message carries what a download needs:
// somewhere to ask (its direct path, or a url to cut one out of), the media key
// and the two hashes. Not the length: an empty file is a file, and its length
// is 0 (issue #474). The download gate and the automatic download on arrival
// both decide on this, so they cannot disagree about a row.
func mediaComplete(url, directPath string, mediaKey, fileSHA256, fileEncSHA256 []byte) bool {
	return (url != "" || directPath != "") && len(mediaKey) > 0 && len(fileSHA256) > 0 && len(fileEncSHA256) > 0
}

// retryMedia asks the sender's phone to re-upload the file and downloads it
// from the fresh path. Tests override Bridge.mediaRetryDownload to record the
// call instead.
func (b *Bridge) retryMedia(ctx context.Context, messageID, chatJID string, downloader *MediaDownloader, root *os.Root, relPath string) (int64, error) {
	if b.mediaRetryDownload != nil {
		return b.mediaRetryDownload(ctx, messageID, chatJID, downloader, root, relPath)
	}
	return downloadViaMediaRetry(ctx, b.currentClient(), b.Store, b.mediaRetry, messageID, chatJID, downloader, root, relPath)
}

// checkMediaPathComponents refuses a chat directory or a media file name that
// is not exactly one path component: empty, or carrying a separator, a ".." or
// a control character (NUL, a line break). The store root already keeps a write
// inside the store; this keeps it inside the one directory of its chat, under
// the one name of its message, which containment alone cannot express. It is
// the same rule for every reader of the cache (media_cache_path.go), and every
// refusal wraps errMediaPath so a caller can tell it from the others. The names
// of ordinary messages never trip it.
var errMediaPath = errors.New("refusing media path")

func checkMediaPathComponents(chatDir, filename string) error {
	for _, c := range []struct{ what, name string }{{"chat JID", chatDir}, {"message ID", filename}} {
		if c.name == "" || c.name == "." || strings.Contains(c.name, "..") || strings.ContainsAny(c.name, `/\`) || strings.ContainsFunc(c.name, unicode.IsControl) {
			return fmt.Errorf("%w: the %s does not name a single file inside the store directory", errMediaPath, c.what)
		}
	}
	return nil
}

// mediaFileName is the cached file name for a media row:
// <type>_<yyyymmdd_hhmmss>_<message id><ext>. The message ID disambiguates
// two messages that arrive in the same second. Documents take a sanitised
// extension from the sender's filename (messages.filename) so the cached file
// opens with the right application; anything else is fixed per type.
//
// The wall clock is the local one. On arrival the timestamp is a Local
// time.Time from whatsmeow; read back out of SQLite it is UTC (store_time.go),
// and the same instant has to yield the same name or a re-download misses the
// cache and a purge never finds the file.
func mediaFileName(mediaType string, timestamp time.Time, messageID, originalName string) string {
	var ext string
	switch mediaType {
	case "image":
		ext = ".jpg"
	case "video":
		ext = ".mp4"
	case "audio":
		ext = ".ogg"
	case "sticker":
		ext = ".webp"
	case "document":
		ext = documentExt(originalName)
	}
	return fmt.Sprintf("%s_%s_%s%s", mediaType, timestamp.Local().Format("20060102_150405"), messageID, ext)
}

// legacyMediaFileName is the name used before documents kept an extension;
// readers try it after mediaFileName so files cached earlier stay reachable.
func legacyMediaFileName(mediaType string, timestamp time.Time, messageID string) string {
	return mediaFileName(mediaType, timestamp, messageID, "")
}

// documentExt returns the lower-cased extension of a sender-supplied file
// name when it is plain ASCII letters/digits of 1..10 characters, "" otherwise.
// Only the extension is reused: the rest of the name never reaches the disk.
func documentExt(name string) string {
	ext := strings.ToLower(filepath.Ext(strings.TrimSpace(name)))
	if len(ext) < 2 || len(ext) > 11 {
		return ""
	}
	for _, r := range ext[1:] {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return ""
		}
	}
	// Reserve .part for writeMediaFile's unfinished downloads. A completed
	// document must never share a name with another file's temporary download.
	if ext == ".part" {
		return ".part.bin"
	}
	return ext
}

// mediaFileNames lists the names a row's cached file may carry, newest naming
// scheme first. Readers try them in order so files cached before documents kept
// an extension stay reachable; keeping the list in one place is what stops a
// reader (download, purge) from looking for a name another one never writes.
func mediaFileNames(mediaType string, timestamp time.Time, messageID, originalName string) []string {
	return []string{
		mediaFileName(mediaType, timestamp, messageID, originalName),
		legacyMediaFileName(mediaType, timestamp, messageID),
	}
}

// chatMediaRel is the store-relative directory holding one chat's media: the
// chat JID with ':' (device suffix) mapped to '_'. Every file operation on a
// chat's media goes through the store root (os.Root) with this relative form.
// The one place that joins it onto the store path is downloadMedia, to build
// the absolute path it hands back to its callers, which the bridge itself never
// opens.
func chatMediaRel(chatJID string) string {
	return strings.ReplaceAll(chatJID, ":", "_")
}

// downloadToPath downloads msg into relPath, a path relative to the store root.
// Returns the byte count written.
func downloadToPath(ctx context.Context, root *os.Root, client *whatsmeow.Client, msg whatsmeow.DownloadableMessage, relPath string) (int64, error) {
	return writeMediaDownload(ctx, root, relPath, func(downloadCtx context.Context, f whatsmeow.File) error {
		return client.DownloadToFile(downloadCtx, msg, f)
	})
}

// writeMediaFile creates relPath inside the store through a ".part" temp file
// that fill writes and an atomic rename. Create, cleanup and rename all go
// through the store root, so a directory that is a symlink out of the store is
// refused by the kernel instead of being written through.
//
// The bytes only ever land in a file this call created: whatever sits under the
// temp name is removed first (a leftover from a crash, or a symlink, which
// Remove unlinks without following) and O_EXCL refuses anything that reappears
// there, where O_TRUNC would follow a link and truncate its target. Nobody else
// owns that name: one transfer per destination runs at a time (media_inflight.go).
func writeMediaFile(root *os.Root, relPath string, fill func(*os.File) error) (int64, error) {
	if root == nil {
		return 0, errors.New("create media file: store directory unavailable")
	}
	tmpPath := relPath + ".part"
	if err := root.Remove(tmpPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, fmt.Errorf("create media file: %w", err)
	}
	f, err := root.OpenFile(tmpPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, fmt.Errorf("create media file: %w", err)
	}
	if err := fill(f); err != nil {
		_ = f.Close()
		_ = root.Remove(tmpPath)
		return 0, err
	}
	info, statErr := f.Stat()
	if err := f.Close(); err != nil {
		_ = root.Remove(tmpPath)
		return 0, fmt.Errorf("close media file: %w", err)
	}
	if err := root.Rename(tmpPath, relPath); err != nil {
		_ = root.Remove(tmpPath)
		return 0, fmt.Errorf("finalise media file: %w", err)
	}
	if statErr != nil {
		return 0, nil
	}
	return info.Size(), nil
}

// Extract direct path from a WhatsApp media URL
func extractDirectPathFromURL(url string) string {
	// The direct path is typically in the URL, we need to extract it
	// Example URL: https://mmg.whatsapp.net/v/t62.7118-24/13812002_698058036224062_3424455886509161511_n.enc?ccb=11-4&oh=...

	// Find the path part after the domain
	parts := strings.SplitN(url, ".net/", 2)
	if len(parts) < 2 {
		return url // Return original URL if parsing fails
	}

	// Keep the query string: it carries the CDN auth tokens (oh=/oe=).
	// whatsmeow's Download rebuilds the URL as host + directPath + "&hash=..."
	// and the CDN returns 403 if the auth params are missing.
	return "/" + parts[1]
}

// handleDownload serves POST /api/download.
func (b *Bridge) handleDownload() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Parse the request body
		var req DownloadMediaRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "Invalid request format")
			return
		}

		// Validate request
		if req.MessageID == "" || req.ChatJID == "" {
			writeError(w, http.StatusBadRequest, "Message ID and Chat JID are required")
			return
		}
		chat, ok := authorizeChat(w, b.Policy, req.ChatJID, false)
		if !ok {
			return
		}
		req.ChatJID = chat.String()
		if !b.Connected() {
			writeJSON(w, http.StatusServiceUnavailable, DownloadMediaResponse{
				Success: false,
				Message: "WhatsApp client is not connected. Please wait for reconnection.",
			})
			return
		}

		// Log download request for debugging
		b.Log.Debugf("📥 Download request: message_id=%s chat_jid=%s", req.MessageID, req.ChatJID)

		// Download the media
		ctx, cancel := requestContext(r, downloadDeadline)
		defer cancel()
		success, mediaType, filename, path, err := b.DownloadMedia(ctx, req.MessageID, req.ChatJID)

		// Set response headers
		w.Header().Set("Content-Type", "application/json")

		// Handle download result
		if !success || err != nil {
			errMsg := "Unknown error"
			if err != nil {
				errMsg = err.Error()
			}
			// A file that can never arrive — the phone answered "I no longer
			// have this", or the row has no media key — is not a failure to
			// retry: name it so the caller records the miss once instead of
			// asking again on every pass over the archive (issues #378, #392).
			code := errorCode(http.StatusInternalServerError)
			if named := permanentMediaCode(err); named != "" {
				code = named
			}
			// The CDN turning down a recent message is the other end failing,
			// and worth another try: 502, which the MCP server reads as
			// bridge_unavailable (issue #452).
			var refused *cdnRefusedError
			if errors.As(err, &refused) {
				writeError(w, http.StatusBadGateway, "Failed to download media: "+errMsg)
				return
			}
			writeErrorCode(w, http.StatusInternalServerError, code, "Failed to download media: "+errMsg)
			return
		}

		// Send successful response
		_ = json.NewEncoder(w).Encode(DownloadMediaResponse{
			Success:  true,
			Message:  fmt.Sprintf("Successfully downloaded %s media", mediaType),
			Filename: filename,
			Path:     path,
		})
	}
}
