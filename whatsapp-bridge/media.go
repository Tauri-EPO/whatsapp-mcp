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
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
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
	client, messageStore := b.Client, b.Store
	// Query the database for the message including timestamp
	var mediaType, url string
	var originalName sql.NullString
	var mediaKey, fileSHA256, fileEncSHA256 []byte
	var fileLength uint64
	var timestamp time.Time
	var err error

	// Get media info AND timestamp from the database
	err = messageStore.db.QueryRow(
		"SELECT media_type, url, media_key, file_sha256, file_enc_sha256, file_length, timestamp, filename FROM messages WHERE id = ? AND chat_jid = ?",
		messageID, chatJID,
	).Scan(&mediaType, &url, &mediaKey, &fileSHA256, &fileEncSHA256, &fileLength, &timestamp, &originalName)

	if err != nil {
		return false, "", "", "", fmt.Errorf("failed to find message: %v", err)
	}

	// Check if this is a media message
	if mediaType == "" {
		return false, "", "", "", fmt.Errorf("not a media message")
	}

	filename := mediaFileName(mediaType, timestamp, messageID, originalName.String)

	// First, check if we already have this file
	chatDir := chatMediaDir(chatJID)

	// Create directory for the chat if it doesn't exist
	if err := os.MkdirAll(chatDir, 0o750); err != nil {
		return false, "", "", "", fmt.Errorf("failed to create chat directory: %v", err)
	}

	// Generate a local path for the file
	localPath := filepath.Join(chatDir, filename)

	// Get absolute path
	absPath, err := filepath.Abs(localPath)
	if err != nil {
		return false, "", "", "", fmt.Errorf("failed to get absolute path: %v", err)
	}

	// Check if file already exists (under the current or the legacy name)
	if cached := cachedMediaPath(chatDir, mediaType, timestamp, messageID, originalName.String); cached != "" {
		if abs, err := filepath.Abs(cached); err == nil {
			absPath = abs
		}
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
	if url == "" || len(mediaKey) == 0 || len(fileSHA256) == 0 || len(fileEncSHA256) == 0 || fileLength == 0 {
		return false, "", "", "", permanentMediaError("incomplete media information for download")
	}

	b.Log.Debugf("Attempting to download media for message %s in chat %s...", messageID, chatJID)

	// Extract direct path from URL
	directPath := extractDirectPathFromURL(url)

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
		FileLength:    fileLength,
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
		written, err := b.transferMedia(ctx, downloader, localPath)
		if isExpiredMediaError(err) {
			// The CDN token in the stored URL has expired (old history, forwards).
			// Ask the sender's phone to re-upload and download from the fresh path.
			// See media_retry.go.
			b.Log.Warnf("Media URL expired for %s (%v); requesting media retry from sender's phone...", messageID, err)
			written, err = downloadViaMediaRetry(ctx, client, messageStore, b.mediaRetry, messageID, chatJID, downloader, localPath)
		}
		if err != nil {
			b.metrics.mediaDownloadFails.Add(1)
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
	return true, mediaType, filename, absPath, nil
}

// transferMedia streams one media file into localPath through its temp file.
// Tests override Bridge.mediaTransfer to run a fake transfer.
func (b *Bridge) transferMedia(ctx context.Context, msg whatsmeow.DownloadableMessage, localPath string) (int64, error) {
	if b.mediaTransfer != nil {
		return b.mediaTransfer(ctx, msg, localPath)
	}
	return downloadToPath(ctx, b.Client, msg, localPath)
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

// cachedMediaPath returns the existing cached file for a row (current name
// first, then the legacy one) or "" when nothing is cached.
func cachedMediaPath(chatDir, mediaType string, timestamp time.Time, messageID, originalName string) string {
	for _, name := range mediaFileNames(mediaType, timestamp, messageID, originalName) {
		p := filepath.Join(chatDir, name)
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	return ""
}

// chatMediaRel is the store-relative directory holding one chat's media: the
// chat JID with ':' (device suffix) mapped to '_'. Callers working through the
// store root (os.Root) need the relative form; chatMediaDir is the same
// directory as a filesystem path.
func chatMediaRel(chatJID string) string {
	return strings.ReplaceAll(chatJID, ":", "_")
}

// chatMediaDir is store/<chat_jid> with ':' (device suffix) mapped to '_'.
func chatMediaDir(chatJID string) string {
	return storePath(chatMediaRel(chatJID))
}

// downloadToPath downloads msg into localPath through a ".part" temp file and
// an atomic rename. Returns the byte count written.
func downloadToPath(ctx context.Context, client *whatsmeow.Client, msg whatsmeow.DownloadableMessage, localPath string) (int64, error) {
	tmpPath := localPath + ".part"
	f, err := os.OpenFile(tmpPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // path is built by downloadMedia under the store directory
	if err != nil {
		return 0, fmt.Errorf("create media file: %w", err)
	}
	if err := client.DownloadToFile(ctx, msg, f); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return 0, err
	}
	info, statErr := f.Stat()
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return 0, fmt.Errorf("close media file: %w", err)
	}
	if err := os.Rename(tmpPath, localPath); err != nil {
		_ = os.Remove(tmpPath)
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
		// Check if connected
		if !b.Connected() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(DownloadMediaResponse{
				Success: false,
				Message: "WhatsApp client is not connected. Please wait for reconnection.",
			})
			return
		}

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
			if errors.Is(err, errMediaUnavailable) {
				code = "media_unavailable"
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
