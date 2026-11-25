package main

// The paired account's own profile: photo, about text and push name.
//
// SetGroupPhoto with an empty JID addresses this account. On the pinned
// whatsmeow, sendIQ omits the target attribute when the JID is empty, and the
// w:profile:picture set then updates the account picture. Nil avatar bytes
// remove it (the library returns the id "remove"). A non-nil body must be a
// JPEG; other formats come back as ErrInvalidImageFormat, so callers here
// always send a square 640 JPEG produced from a JPEG or PNG in
// WHATSAPP_MEDIA_ROOTS.
//
// About text is SetStatusMessage (the profile "About" field, not a status
// broadcast). The push name is the app-state critical_block patch
// BuildSettingPushName, the same single writer the bridge uses for archive
// and labels. That patch is the device's push name: it is not the name saved
// in someone else's contacts and not a business verified name. It needs
// app-state keys the phone has already synced; the phone can overwrite it
// later. A 429 is reported once and never retried.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	_ "image/png" // register the PNG decoder; profile photos accept JPEG and PNG
	"io"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/types"
)

const (
	profilePhotoEdge           = 640
	profilePhotoMaxEdge        = 4096
	profilePhotoMaxInputBytes  = 8 << 20
	profilePhotoMaxOutputBytes = 512 << 10
	profileAboutMaxRunes       = 139
	profileNameMaxRunes        = 25
	profileCallTimeout         = 20 * time.Second
)

var (
	errProfileImageInvalid  = errors.New("image must be a JPEG or PNG")
	errProfileImageTooLarge = errors.New("image is too large")
)

// ownProfileAPI is the slice of *whatsmeow.Client these handlers call.
// Tests substitute a fake; production passes the live client.
type ownProfileAPI interface {
	SetGroupPhoto(context.Context, types.JID, []byte) (string, error)
	SetStatusMessage(context.Context, types.SetStatusInput) error
}

type ownProfileOps struct {
	connected func() bool
	loggedIn  func() bool
	setPhoto  func(context.Context, []byte) (string, error)
	setAbout  func(context.Context, string) error
	setName   func(context.Context, string) error
}

func bindOwnProfile(client ownProfileAPI, connected, loggedIn func() bool, send appStateSendFunc, remember func(context.Context, string) error) ownProfileOps {
	if sdk, ok := client.(*whatsmeow.Client); ok && sdk == nil {
		client = nil
	}
	if connected == nil {
		connected = func() bool { return false }
	}
	if loggedIn == nil {
		loggedIn = func() bool { return false }
	}
	ops := ownProfileOps{
		connected: func() bool { return client != nil && connected() },
		loggedIn:  func() bool { return client != nil && loggedIn() },
	}
	unavailable := func(context.Context, []byte) (string, error) {
		return "", whatsmeow.ErrClientIsNil
	}
	if client == nil || send == nil {
		ops.setName = func(context.Context, string) error { return whatsmeow.ErrClientIsNil }
	}
	if client == nil {
		ops.setPhoto = unavailable
		ops.setAbout = func(context.Context, string) error { return whatsmeow.ErrClientIsNil }
		return ops
	}
	ops.setPhoto = func(ctx context.Context, avatar []byte) (string, error) {
		// Empty JID: sendIQ drops the target attribute, so the set applies to
		// this account's picture rather than a group's.
		return client.SetGroupPhoto(ctx, types.EmptyJID, avatar)
	}
	ops.setAbout = func(ctx context.Context, text string) error {
		return client.SetStatusMessage(ctx, types.SetStatusInput{Text: &text})
	}
	if send != nil {
		ops.setName = func(ctx context.Context, name string) error {
			if err := send(ctx, appstate.BuildSettingPushName(name)); err != nil {
				return err
			}
			if remember != nil {
				// The name is already on WhatsApp. A local save failure must
				// not look like a failed send: the caller would retry it.
				_ = remember(ctx, name)
			}
			return nil
		}
	}
	return ops
}

func (ops ownProfileOps) refuseOffline(w http.ResponseWriter) bool {
	if !ops.connected() {
		writeError(w, http.StatusServiceUnavailable, "WhatsApp client is not connected")
		return true
	}
	if !ops.loggedIn() {
		writeError(w, http.StatusServiceUnavailable, "WhatsApp client is not logged in")
		return true
	}
	return false
}

func handleSetProfilePhoto(ops ownProfileOps, roots []string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Path string `json:"path"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		if ops.refuseOffline(w) {
			return
		}
		jpegBytes, err := prepareProfileJPEG(req.Path, roots)
		if err != nil {
			writeProfileImageError(w, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), profileCallTimeout)
		defer cancel()
		id, err := ops.setPhoto(ctx, jpegBytes)
		if profileCallFailed(w, err) {
			return
		}
		if !acceptablePictureID(id) {
			writeError(w, http.StatusBadGateway, "WhatsApp did not return a picture id")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "picture_id": id})
	}
}

func handleRemoveProfilePhoto(ops ownProfileOps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if ops.refuseOffline(w) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), profileCallTimeout)
		defer cancel()
		// Nil, not an empty slice: the library treats only nil as removal.
		if _, err := ops.setPhoto(ctx, nil); profileCallFailed(w, err) {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "removed": true})
	}
}

func handleSetAbout(ops ownProfileOps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Text *string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Text == nil {
			writeError(w, http.StatusBadRequest, "text is required")
			return
		}
		if ops.refuseOffline(w) {
			return
		}
		text, err := cleanProfileText(*req.Text, "text", profileAboutMaxRunes, true)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), profileCallTimeout)
		defer cancel()
		if err := ops.setAbout(ctx, text); profileCallFailed(w, err) {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "text": text})
	}
}

func handleSetDisplayName(ops ownProfileOps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name *string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == nil {
			writeError(w, http.StatusBadRequest, "name is required")
			return
		}
		if ops.refuseOffline(w) {
			return
		}
		name, err := cleanProfileText(*req.Name, "name", profileNameMaxRunes, false)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), profileCallTimeout)
		defer cancel()
		if err := ops.setName(ctx, name); profileCallFailed(w, err) {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "name": name})
	}
}

func writeProfileImageError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errProfileImageTooLarge):
		writeError(w, http.StatusBadRequest, "image is too large")
	case errors.Is(err, errProfileImageInvalid):
		writeError(w, http.StatusBadRequest, "image must be a JPEG or PNG")
	default:
		writeError(w, http.StatusBadRequest, err.Error())
	}
}

// profileCallFailed writes the HTTP error for one WhatsApp failure and reports
// whether it did. A 429 is returned as rate_limited with no further attempt.
func profileCallFailed(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	var iq *whatsmeow.IQError
	if errors.As(err, &iq) && iq != nil && iq.Code == http.StatusTooManyRequests {
		writeErrorCode(w, http.StatusTooManyRequests, "rate_limited", "WhatsApp rate limited this profile change; report it instead of retrying")
		return true
	}
	if errors.Is(err, whatsmeow.ErrNotLoggedIn) || errors.Is(err, whatsmeow.ErrClientIsNil) {
		writeError(w, http.StatusServiceUnavailable, "WhatsApp client is not logged in")
		return true
	}
	if errors.Is(err, whatsmeow.ErrInvalidImageFormat) {
		writeError(w, http.StatusBadRequest, "WhatsApp rejected the profile photo")
		return true
	}
	var notSent appStateNotSentError
	if errors.As(err, &notSent) {
		writeError(w, http.StatusServiceUnavailable, "profile change was not sent")
		return true
	}
	writeError(w, http.StatusBadGateway, "profile change failed: "+err.Error())
	return true
}

func acceptablePictureID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 0x20 || id[i] > 0x7e {
			return false
		}
	}
	return true
}

func cleanProfileText(raw, label string, max int, allowEmpty bool) (string, error) {
	text := strings.TrimSpace(raw)
	if text == "" && !allowEmpty {
		return "", fmt.Errorf("%s is required", label)
	}
	if utf8.RuneCountInString(text) > max {
		return "", fmt.Errorf("%s is longer than %d characters", label, max)
	}
	for _, r := range text {
		if rejectedProfileRune(r) {
			return "", fmt.Errorf("%s contains a control character", label)
		}
	}
	return text, nil
}

func rejectedProfileRune(r rune) bool {
	if unicode.IsControl(r) {
		return true
	}
	switch r {
	case '\u202A', '\u202B', '\u202C', '\u202D', '\u202E',
		'\u2066', '\u2067', '\u2068', '\u2069',
		'\u200E', '\u200F', '\u2028', '\u2029', '\uFEFF':
		return true
	default:
		return false
	}
}

func prepareProfileJPEG(path string, roots []string) ([]byte, error) {
	confined, err := validateMediaPath(path, roots)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(confined)
	if err != nil {
		return nil, fmt.Errorf("stat media_path: %w", err)
	}
	if info.Size() > profilePhotoMaxInputBytes {
		return nil, errProfileImageTooLarge
	}
	f, err := os.Open(confined) //nolint:gosec // mediaPath was canonicalised and confined to WHATSAPP_MEDIA_ROOTS by validateMediaPath
	if err != nil {
		return nil, fmt.Errorf("read media_path: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, profilePhotoMaxInputBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read media_path: %w", err)
	}
	if int64(len(data)) > profilePhotoMaxInputBytes {
		return nil, errProfileImageTooLarge
	}
	return normalizeProfileJPEG(data)
}

func normalizeProfileJPEG(data []byte) ([]byte, error) {
	if !looksLikeJPEG(data) && !looksLikePNG(data) {
		return nil, errProfileImageInvalid
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, errProfileImageInvalid
	}
	if cfg.Width > profilePhotoMaxEdge || cfg.Height > profilePhotoMaxEdge {
		return nil, errProfileImageTooLarge
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, errProfileImageInvalid
	}
	return encodeProfileJPEG(scaleSquare(cropSquare(img), profilePhotoEdge))
}

func looksLikeJPEG(data []byte) bool {
	return len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff
}

func looksLikePNG(data []byte) bool {
	return len(data) >= 8 && bytes.Equal(data[:8], []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
}

func cropSquare(src image.Image) *image.NRGBA {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	side := w
	if h < side {
		side = h
	}
	dst := image.NewNRGBA(image.Rect(0, 0, side, side))
	draw.Draw(dst, dst.Bounds(), image.White, image.Point{}, draw.Src)
	draw.Draw(dst, dst.Bounds(), src, image.Pt(b.Min.X+(w-side)/2, b.Min.Y+(h-side)/2), draw.Over)
	return dst
}

func scaleSquare(src *image.NRGBA, edge int) *image.NRGBA {
	side := src.Bounds().Dx()
	if side == edge {
		return src
	}
	dst := image.NewNRGBA(image.Rect(0, 0, edge, edge))
	if side > edge {
		boxDownscale(dst, src)
	} else {
		nearestUpscale(dst, src)
	}
	return dst
}

func boxDownscale(dst, src *image.NRGBA) {
	edge := dst.Bounds().Dx()
	side := src.Bounds().Dx()
	for y := 0; y < edge; y++ {
		y0, y1 := sampleSpan(y, edge, side)
		for x := 0; x < edge; x++ {
			x0, x1 := sampleSpan(x, edge, side)
			var r, g, b, n uint32
			for yy := y0; yy < y1; yy++ {
				row := src.Pix[yy*src.Stride+x0*4:]
				for xx := x0; xx < x1; xx++ {
					r += uint32(row[0])
					g += uint32(row[1])
					b += uint32(row[2])
					n++
					row = row[4:]
				}
			}
			i := y*dst.Stride + x*4
			dst.Pix[i] = averageByte(r, n)
			dst.Pix[i+1] = averageByte(g, n)
			dst.Pix[i+2] = averageByte(b, n)
			dst.Pix[i+3] = 255
		}
	}
}

func sampleSpan(i, edge, side int) (int, int) {
	start := i * side / edge
	end := (i + 1) * side / edge
	if end > side {
		end = side
	}
	if end <= start {
		if start >= side {
			start = side - 1
		}
		end = start + 1
	}
	return start, end
}

func averageByte(sum, n uint32) uint8 {
	if n == 0 {
		return 0
	}
	v := sum / n
	if v > 255 {
		return 255
	}
	return uint8(v)
}

func nearestUpscale(dst, src *image.NRGBA) {
	edge := dst.Bounds().Dx()
	side := src.Bounds().Dx()
	if side <= 0 {
		return
	}
	for y := 0; y < edge; y++ {
		sy := y * side / edge
		if sy >= side {
			sy = side - 1
		}
		for x := 0; x < edge; x++ {
			sx := x * side / edge
			if sx >= side {
				sx = side - 1
			}
			si := sy*src.Stride + sx*4
			di := y*dst.Stride + x*4
			copy(dst.Pix[di:di+3], src.Pix[si:si+3])
			dst.Pix[di+3] = 255
		}
	}
}

func encodeProfileJPEG(img image.Image) ([]byte, error) {
	for _, quality := range []int{85, 70, 50} {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
			return nil, errProfileImageInvalid
		}
		if buf.Len() <= profilePhotoMaxOutputBytes {
			return buf.Bytes(), nil
		}
	}
	return nil, errProfileImageTooLarge
}
