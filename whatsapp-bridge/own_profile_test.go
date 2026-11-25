package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/types"
)

type fakeOwnProfile struct {
	photoCalls int
	photoJID   types.JID
	photoBytes []byte
	photoErr   error
	photoID    string

	aboutCalls int
	aboutText  string
	aboutErr   error
}

func (f *fakeOwnProfile) SetGroupPhoto(_ context.Context, jid types.JID, avatar []byte) (string, error) {
	f.photoCalls++
	f.photoJID = jid
	if avatar == nil {
		f.photoBytes = nil
	} else {
		f.photoBytes = append([]byte(nil), avatar...)
	}
	if f.photoErr != nil {
		return "", f.photoErr
	}
	if avatar == nil {
		return "remove", nil
	}
	if f.photoID == "" {
		return "pic1", nil
	}
	return f.photoID, nil
}

func (f *fakeOwnProfile) SetStatusMessage(_ context.Context, status types.SetStatusInput) error {
	f.aboutCalls++
	if status.Text != nil {
		f.aboutText = *status.Text
	}
	return f.aboutErr
}

func postProfile(h http.Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeProfileBody(t *testing.T, rec *httptest.ResponseRecorder) apiError {
	t.Helper()
	var body apiError
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %s: %v", rec.Body.String(), err)
	}
	return body
}

func writeProfileFixture(t *testing.T, dir, name string, w, h int, kind string) string {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	fill := color.NRGBA{R: 20, G: 40, B: 80, A: 255}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, fill)
		}
	}
	var buf bytes.Buffer
	var err error
	if kind == "png" {
		err = png.Encode(&buf, img)
	} else {
		err = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90})
	}
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func loggedInProfile(api *fakeOwnProfile) ownProfileOps {
	return bindOwnProfile(api, func() bool { return true }, func() bool { return true }, nil, nil)
}

func TestSetProfilePhotoNormalizesAJPEG(t *testing.T) {
	root := t.TempDir()
	path := writeProfileFixture(t, root, "square.jpg", 1024, 1024, "jpeg")
	api := &fakeOwnProfile{photoID: "pic-640"}
	rec := postProfile(handleSetProfilePhoto(loggedInProfile(api), []string{root}), `{"path":"`+path+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Success   bool   `json:"success"`
		PictureID string `json:"picture_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Success || body.PictureID != "pic-640" {
		t.Fatalf("body %+v", body)
	}
	if api.photoCalls != 1 {
		t.Fatalf("SetGroupPhoto calls = %d, want 1", api.photoCalls)
	}
	if !api.photoJID.IsEmpty() {
		t.Fatalf("target JID = %s, want empty", api.photoJID)
	}
	cfg, format, err := image.DecodeConfig(strings.NewReader(string(api.photoBytes)))
	if err != nil {
		t.Fatal(err)
	}
	if format != "jpeg" || cfg.Width != profilePhotoEdge || cfg.Height != profilePhotoEdge {
		t.Fatalf("sent image %s %dx%d, want jpeg %dx%d", format, cfg.Width, cfg.Height, profilePhotoEdge, profilePhotoEdge)
	}
	if !looksLikeJPEG(api.photoBytes) || len(api.photoBytes) > profilePhotoMaxOutputBytes {
		t.Fatalf("sent %d bytes, jpeg=%v", len(api.photoBytes), looksLikeJPEG(api.photoBytes))
	}
}

func TestSetProfilePhotoAcceptsAPNG(t *testing.T) {
	root := t.TempDir()
	path := writeProfileFixture(t, root, "wide.png", 800, 200, "png")
	api := &fakeOwnProfile{}
	rec := postProfile(handleSetProfilePhoto(loggedInProfile(api), []string{root}), `{"path":"`+path+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	cfg, format, err := image.DecodeConfig(strings.NewReader(string(api.photoBytes)))
	if err != nil {
		t.Fatal(err)
	}
	if format != "jpeg" || cfg.Width != profilePhotoEdge || cfg.Height != profilePhotoEdge || api.photoCalls != 1 {
		t.Fatalf("sent %s %dx%d calls %d", format, cfg.Width, cfg.Height, api.photoCalls)
	}
}

func TestSetProfilePhotoRejectsBeforeWhatsApp(t *testing.T) {
	root := t.TempDir()
	textPath := filepath.Join(root, "note.txt")
	if err := os.WriteFile(textPath, []byte("not an image"), 0o600); err != nil {
		t.Fatal(err)
	}
	wide := writeProfileFixture(t, root, "wide.png", profilePhotoMaxEdge+1, 1, "png")
	huge := filepath.Join(root, "huge.jpg")
	if err := os.WriteFile(huge, []byte{0xff, 0xd8, 0xff}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(huge, profilePhotoMaxInputBytes+1); err != nil {
		t.Fatal(err)
	}
	outside := writeProfileFixture(t, t.TempDir(), "out.jpg", 32, 32, "jpeg")

	api := &fakeOwnProfile{}
	ops := loggedInProfile(api)
	h := handleSetProfilePhoto(ops, []string{root})
	cases := []struct {
		name string
		path string
		want string
	}{
		{"invalid", textPath, "JPEG or PNG"},
		{"edge", wide, "too large"},
		{"bytes", huge, "too large"},
		{"outside", outside, "outside"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := api.photoCalls
			rec := postProfile(h, `{"path":"`+tc.path+`"}`)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("body %s, want %q", rec.Body.String(), tc.want)
			}
			if api.photoCalls != before {
				t.Fatalf("SetGroupPhoto calls = %d, want %d", api.photoCalls, before)
			}
		})
	}
}

func TestSetProfilePhotoRateLimitIsOneCall(t *testing.T) {
	root := t.TempDir()
	path := writeProfileFixture(t, root, "square.jpg", 64, 64, "jpeg")
	api := &fakeOwnProfile{photoErr: whatsmeow.ErrIQRateOverLimit}
	rec := postProfile(handleSetProfilePhoto(loggedInProfile(api), []string{root}), `{"path":"`+path+`"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	body := decodeProfileBody(t, rec)
	if body.Error.Code != "rate_limited" || api.photoCalls != 1 {
		t.Fatalf("code %q calls %d body %s", body.Error.Code, api.photoCalls, rec.Body.String())
	}
	if strings.Contains(strings.ToLower(body.Message), "retry after") {
		t.Fatalf("message invites a retry: %s", body.Message)
	}
}

func TestSetProfilePhotoNotLoggedIn(t *testing.T) {
	api := &fakeOwnProfile{}
	ops := bindOwnProfile(api, func() bool { return true }, func() bool { return false }, nil, nil)
	rec := postProfile(handleSetProfilePhoto(ops, []string{t.TempDir()}), `{"path":"/no/such/photo.jpg"}`)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "not logged in") {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if api.photoCalls != 0 {
		t.Fatalf("SetGroupPhoto calls = %d, want 0", api.photoCalls)
	}

	root := t.TempDir()
	path := writeProfileFixture(t, root, "square.jpg", 32, 32, "jpeg")
	api.photoErr = whatsmeow.ErrNotLoggedIn
	ops = loggedInProfile(api)
	rec = postProfile(handleSetProfilePhoto(ops, []string{root}), `{"path":"`+path+`"}`)
	if rec.Code != http.StatusServiceUnavailable || api.photoCalls != 1 {
		t.Fatalf("status %d calls %d body %s", rec.Code, api.photoCalls, rec.Body.String())
	}
}

func TestRemoveProfilePhoto(t *testing.T) {
	api := &fakeOwnProfile{}
	rec := postProfile(handleRemoveProfilePhoto(loggedInProfile(api)), `{}`)
	if rec.Code != http.StatusOK || api.photoCalls != 1 || api.photoBytes != nil || !api.photoJID.IsEmpty() {
		t.Fatalf("status %d calls %d bytes %d jid %s body %s", rec.Code, api.photoCalls, len(api.photoBytes), api.photoJID, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"removed":true`) {
		t.Fatalf("body %s", rec.Body.String())
	}

	api.photoErr = whatsmeow.ErrIQRateOverLimit
	rec = postProfile(handleRemoveProfilePhoto(loggedInProfile(api)), `{}`)
	if rec.Code != http.StatusTooManyRequests || api.photoCalls != 2 {
		t.Fatalf("status %d calls %d body %s", rec.Code, api.photoCalls, rec.Body.String())
	}
}

func TestSetAbout(t *testing.T) {
	api := &fakeOwnProfile{}
	h := handleSetAbout(loggedInProfile(api))
	rec := postProfile(h, `{"text":"  out of office  "}`)
	if rec.Code != http.StatusOK || api.aboutText != "out of office" || api.aboutCalls != 1 {
		t.Fatalf("status %d text %q calls %d body %s", rec.Code, api.aboutText, api.aboutCalls, rec.Body.String())
	}
	rec = postProfile(h, `{"text":""}`)
	if rec.Code != http.StatusOK || api.aboutText != "" || api.aboutCalls != 2 {
		t.Fatalf("clear status %d text %q calls %d", rec.Code, api.aboutText, api.aboutCalls)
	}
	rec = postProfile(h, `{"text":"`+strings.Repeat("a", profileAboutMaxRunes+1)+`"}`)
	if rec.Code != http.StatusBadRequest || api.aboutCalls != 2 {
		t.Fatalf("long status %d calls %d body %s", rec.Code, api.aboutCalls, rec.Body.String())
	}
	rec = postProfile(h, `{"text":"hel\nlo"}`)
	if rec.Code != http.StatusBadRequest || api.aboutCalls != 2 {
		t.Fatalf("control status %d calls %d body %s", rec.Code, api.aboutCalls, rec.Body.String())
	}

	api.aboutErr = whatsmeow.ErrIQRateOverLimit
	rec = postProfile(h, `{"text":"later"}`)
	if rec.Code != http.StatusTooManyRequests || api.aboutCalls != 3 {
		t.Fatalf("rate status %d calls %d", rec.Code, api.aboutCalls)
	}

	offline := bindOwnProfile(api, func() bool { return true }, func() bool { return false }, nil, nil)
	rec = postProfile(handleSetAbout(offline), `{"text":"later"}`)
	if rec.Code != http.StatusServiceUnavailable || api.aboutCalls != 3 {
		t.Fatalf("offline status %d calls %d body %s", rec.Code, api.aboutCalls, rec.Body.String())
	}
}

func TestSetDisplayName(t *testing.T) {
	var (
		sends      int
		patch      appstate.PatchInfo
		sendErr    error
		remembered int
	)
	send := func(_ context.Context, got appstate.PatchInfo) error {
		sends++
		patch = got
		return sendErr
	}
	remember := func(context.Context, string) error {
		remembered++
		return nil
	}
	api := &fakeOwnProfile{}
	ops := bindOwnProfile(api, func() bool { return true }, func() bool { return true }, send, remember)
	h := handleSetDisplayName(ops)
	rec := postProfile(h, `{"name":" Alice "}`)
	if rec.Code != http.StatusOK || sends != 1 || remembered != 1 {
		t.Fatalf("status %d sends %d remembered %d body %s", rec.Code, sends, remembered, rec.Body.String())
	}
	if patch.Type != appstate.WAPatchCriticalBlock || len(patch.Mutations) != 1 || patch.Mutations[0].Index[0] != appstate.IndexSettingPushName {
		t.Fatalf("patch type %q index %v", patch.Type, patch.Mutations)
	}
	if patch.Mutations[0].Value.GetPushNameSetting().GetName() != "Alice" {
		t.Fatalf("push name %q", patch.Mutations[0].Value.GetPushNameSetting().GetName())
	}

	rec = postProfile(h, `{"name":"  "}`)
	if rec.Code != http.StatusBadRequest || sends != 1 {
		t.Fatalf("empty status %d sends %d body %s", rec.Code, sends, rec.Body.String())
	}
	rec = postProfile(h, `{"name":"`+strings.Repeat("a", profileNameMaxRunes+1)+`"}`)
	if rec.Code != http.StatusBadRequest || sends != 1 {
		t.Fatalf("long status %d sends %d", rec.Code, sends)
	}

	sendErr = whatsmeow.ErrIQRateOverLimit
	rec = postProfile(h, `{"name":"Alice"}`)
	if rec.Code != http.StatusTooManyRequests || sends != 2 || remembered != 1 {
		t.Fatalf("rate status %d sends %d remembered %d body %s", rec.Code, sends, remembered, rec.Body.String())
	}
	if decodeProfileBody(t, rec).Error.Code != "rate_limited" {
		t.Fatalf("body %s", rec.Body.String())
	}

	offline := bindOwnProfile(api, func() bool { return true }, func() bool { return false }, send, remember)
	rec = postProfile(handleSetDisplayName(offline), `{"name":"Alice"}`)
	if rec.Code != http.StatusServiceUnavailable || sends != 2 {
		t.Fatalf("offline status %d sends %d body %s", rec.Code, sends, rec.Body.String())
	}
}
