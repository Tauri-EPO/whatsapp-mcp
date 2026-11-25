package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waVnameCert"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

type fakeProfileClient struct {
	fakeBusinessClient
	picture, preview           *types.ProfilePictureInfo
	pictureErr, previewErr     error
	pictureCalls, previewCalls int
	answers                    []types.IsOnWhatsAppResponse
	onErr                      error
	onCalls, downloads         int
	download                   []byte
	downloadErr                error
}

func (f *fakeProfileClient) GetProfilePictureInfo(_ context.Context, _ types.JID, params *whatsmeow.GetProfilePictureParams) (*types.ProfilePictureInfo, error) {
	if params != nil && params.Preview {
		f.previewCalls++
		return f.preview, f.previewErr
	}
	f.pictureCalls++
	return f.picture, f.pictureErr
}

type profileFixture struct {
	h         http.HandlerFunc
	cache     *profileCache
	business  *businessCache
	root      *os.Root
	client    *fakeProfileClient
	now       time.Time
	connected bool
	store     *MessageStore
	policy    chatPolicy
	limit     int
}

func newProfileFixture(t *testing.T, client *fakeProfileClient) *profileFixture {
	t.Helper()
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	f := &profileFixture{
		cache: &profileCache{}, business: &businessCache{}, root: root, client: client,
		now: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC), connected: true, limit: 30,
		policy: chatPolicy{},
	}
	f.cache.now = func() time.Time { return f.now }
	f.business.now = func() time.Time { return f.now }
	if client.picture == nil && client.pictureErr == nil {
		client.picture = &types.ProfilePictureInfo{URL: "https://cdn.example.test/photo", ID: "pic-1"}
	}
	if client.download == nil {
		client.download = []byte("profile-photo")
	}
	f.h = handleGetProfile(profileDeps{
		client: client, business: f.business, profiles: f.cache, root: f.root,
		policy:    chatPolicyHandle{resolve: func() chatPolicy { return f.policy }},
		connected: func() bool { return f.connected },
		onWhatsApp: func(_ context.Context, phones []string) ([]types.IsOnWhatsAppResponse, error) {
			client.onCalls++
			if client.answers != nil || client.onErr != nil {
				return client.answers, client.onErr
			}
			user := strings.TrimPrefix(phones[0], "+")
			return []types.IsOnWhatsAppResponse{{
				Query: phones[0], IsIn: true, JID: types.NewJID(user, types.DefaultUserServer),
			}}, nil
		},
		store:   f.store,
		account: func() *types.JID { return nil },
		limit:   0,
		download: func(context.Context, string, int64) ([]byte, error) {
			client.downloads++
			return client.download, client.downloadErr
		},
	})
	// limit is read from the closure's deps value, so rebuild when tests change it.
	f.rebuild(t)
	return f
}

func (f *profileFixture) rebuild(t *testing.T) {
	t.Helper()
	client := f.client
	f.h = handleGetProfile(profileDeps{
		client: client, business: f.business, profiles: f.cache, root: f.root, store: f.store,
		policy:    chatPolicyHandle{resolve: func() chatPolicy { return f.policy }},
		connected: func() bool { return f.connected },
		onWhatsApp: func(_ context.Context, phones []string) ([]types.IsOnWhatsAppResponse, error) {
			client.onCalls++
			if client.answers != nil || client.onErr != nil {
				return client.answers, client.onErr
			}
			user := strings.TrimPrefix(phones[0], "+")
			return []types.IsOnWhatsAppResponse{{
				Query: phones[0], IsIn: true, JID: types.NewJID(user, types.DefaultUserServer),
			}}, nil
		},
		limit: f.limit,
		download: func(context.Context, string, int64) ([]byte, error) {
			client.downloads++
			return client.download, client.downloadErr
		},
	})
}

func (f *profileFixture) get(raw string) (int, profileResult) {
	w := httptest.NewRecorder()
	f.h(w, httptest.NewRequest(http.MethodGet, "/api/profile?jid="+raw, nil))
	var got profileResult
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	return w.Code, got
}

func TestProfilePictureStatuses(t *testing.T) {
	alice := &types.VerifiedName{Details: &waVnameCert.VerifiedNameCertificate_Details{VerifiedName: proto.String("Alice")}}
	phone := "5511999999999"
	cases := []struct {
		name    string
		client  fakeProfileClient
		status  string
		about   string
		limited bool
		network bool
	}{
		{
			name: "ok", status: profileStatusOK, about: "hello", network: true,
			client: fakeProfileClient{
				// GetUserInfo leaves VerifiedName empty. IsOnWhatsApp fills it.
				fakeBusinessClient: fakeBusinessClient{user: types.UserInfo{Status: "hello", PictureID: "pic-1"}, profile: &types.BusinessProfile{Email: "alice@example.test"}},
				answers: []types.IsOnWhatsAppResponse{{
					Query: "+" + phone, IsIn: true, JID: types.NewJID(phone, types.DefaultUserServer), VerifiedName: alice,
				}},
			},
		},
		{
			name: "hidden", status: profileStatusHidden, about: "hello", network: true,
			client: fakeProfileClient{
				fakeBusinessClient: fakeBusinessClient{user: types.UserInfo{Status: "hello"}},
				pictureErr:         whatsmeow.ErrProfilePictureUnauthorized,
			},
		},
		{
			name: "none", status: profileStatusNone, network: true,
			client: fakeProfileClient{pictureErr: whatsmeow.ErrProfilePictureNotSet},
		},
		{
			name: "user 429", status: profileStatusError, limited: true,
			client: fakeProfileClient{fakeBusinessClient: fakeBusinessClient{userErr: fmt.Errorf("lookup: %w", &whatsmeow.IQError{Code: 429})}},
		},
		{
			name: "picture 429", status: profileStatusError, limited: true,
			client: fakeProfileClient{pictureErr: fmt.Errorf("picture: %w", &whatsmeow.IQError{Code: 429})},
		},
		{
			name: "not on whatsapp", status: profileStatusNone,
			client: fakeProfileClient{answers: []types.IsOnWhatsAppResponse{{Query: "+" + phone, IsIn: false}}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := tc.client
			f := newProfileFixture(t, &client)
			code, got := f.get(phone)
			if code != 200 || got.PictureStatus != tc.status || got.RateLimited != tc.limited || got.About != tc.about {
				t.Fatalf("code=%d body=%+v", code, got)
			}
			if tc.name == "ok" {
				if client.user.VerifiedName != nil || got.VerifiedName != "Alice" || got.PictureMessageID != profilePictureID || got.PictureChatJID != phone+"@s.whatsapp.net" || got.PushNameSource != profilePushNameSource || got.OnWhatsApp == nil || !*got.OnWhatsApp {
					t.Fatalf("%+v", got)
				}
				entries, err := fs.ReadDir(f.root.FS(), phone+"@s.whatsapp.net")
				if err != nil || len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), "_"+profilePictureID+".jpg") {
					t.Fatalf("cached picture: %v %v", entries, err)
				}
				f.now = f.now.Add(time.Minute)
				_, again := f.get(phone)
				if !again.Cached || client.pictureCalls != 1 || client.userCalls != 1 || client.profileCalls != 1 || client.downloads != 1 {
					t.Fatalf("cache miss on a complete answer: %+v calls=%d", again, client.pictureCalls)
				}
			}
			if tc.name == "user 429" && (client.pictureCalls != 0 || client.profileCalls != 0) {
				t.Fatal("429 retried picture or business")
			}
			if tc.name == "picture 429" && (client.previewCalls != 0 || client.profileCalls != 0 || client.downloads != 0) {
				t.Fatal("picture 429 continued")
			}
			if tc.name == "not on whatsapp" && (got.OnWhatsApp == nil || *got.OnWhatsApp || client.userCalls != 0 || client.pictureCalls != 0) {
				t.Fatalf("%+v calls=%d", got, client.userCalls)
			}
			if tc.name == "hidden" && client.downloads != 0 {
				t.Fatal("hidden photo was downloaded")
			}
			if tc.name == "none" && (got.AboutStatus != profileStatusNone || client.downloads != 0) {
				t.Fatalf("%+v", got)
			}
			if tc.network && client.onCalls == 0 {
				t.Fatal("phone was not checked")
			}
		})
	}
}

func TestProfilePreviewFallsBackOnce(t *testing.T) {
	client := &fakeProfileClient{
		pictureErr: errors.New("full size unavailable"),
		preview:    &types.ProfilePictureInfo{URL: "https://cdn.example.test/preview", ID: "prev"},
	}
	f := newProfileFixture(t, client)
	code, got := f.get("5511999999999")
	if code != 200 || got.PictureStatus != profileStatusOK || client.pictureCalls != 1 || client.previewCalls != 1 || client.downloads != 1 {
		t.Fatalf("code=%d %+v calls=%d/%d", code, got, client.pictureCalls, client.previewCalls)
	}
}

func TestProfileOnWhatsApp429Stops(t *testing.T) {
	client := &fakeProfileClient{onErr: fmt.Errorf("check: %w", &whatsmeow.IQError{Code: 429})}
	f := newProfileFixture(t, client)
	code, got := f.get("5511999999999")
	if code != 200 || !got.RateLimited || got.OnWhatsApp != nil || client.userCalls != 0 || client.pictureCalls != 0 {
		t.Fatalf("code=%d %+v user=%d", code, got, client.userCalls)
	}
}

func TestProfileAllowListBeforeAndAfterLookup(t *testing.T) {
	client := &fakeProfileClient{}
	f := newProfileFixture(t, client)
	f.policy = parseChatPolicy("5511888888888")
	f.rebuild(t)
	if code, _ := f.get("5511999999999"); code != 403 || client.onCalls != 0 {
		t.Fatalf("typed number reached WhatsApp: %d calls=%d", code, client.onCalls)
	}
	f.policy = chatPolicy{}
	f.rebuild(t)
	client.answers = []types.IsOnWhatsAppResponse{{
		Query: "+5511999999999", IsIn: true,
		JID: types.NewJID("5511888888888", types.DefaultUserServer), PhoneNumber: types.NewJID("5511888888888", types.DefaultUserServer),
	}}
	f.policy = parseChatPolicy("5511999999999")
	f.rebuild(t)
	if code, _ := f.get("5511999999999"); code != 403 || client.userCalls != 0 {
		t.Fatalf("registered number skipped the allow-list: %d user=%d", code, client.userCalls)
	}
	if _, ok := f.cache.get(client, types.NewJID("5511999999999", types.DefaultUserServer)); ok {
		t.Fatal("denial was cached")
	}
}

func TestProfileCacheDoesNotBypassANewPolicy(t *testing.T) {
	client := &fakeProfileClient{}
	f := newProfileFixture(t, client)
	if code, got := f.get("5511999999999"); code != 200 || got.PictureStatus != profileStatusOK {
		t.Fatal(code, got)
	}
	before := client.userCalls
	f.policy = parseChatPolicy("5511888888888")
	f.rebuild(t)
	if code, _ := f.get("5511999999999"); code != 403 || client.userCalls != before {
		t.Fatal("cached profile bypassed the new policy")
	}
}

func TestProfileUnknownHourlyLimit(t *testing.T) {
	client := &fakeProfileClient{}
	f := newProfileFixture(t, client)
	f.limit = 1
	f.rebuild(t)
	if code, first := f.get("5511999999999"); code != 200 || first.RateLimited || client.onCalls != 1 {
		t.Fatalf("first lookup: %d %+v calls=%d", code, first, client.onCalls)
	}
	code, second := f.get("5511888888888")
	if code != 200 || !second.RateLimited || second.Limit != profileUnknownLimitName || second.OnWhatsApp != nil || client.onCalls != 1 || !strings.Contains(second.Message, "operator") {
		t.Fatalf("limit: %d %+v calls=%d", code, second, client.onCalls)
	}
}

func TestProfileKnownChatDoesNotConsumeTheLimit(t *testing.T) {
	client := &fakeProfileClient{}
	f := newProfileFixture(t, client)
	f.store = newTestMessageStore(t)
	known := "5511999999999@s.whatsapp.net"
	if _, err := f.store.db.Exec(`INSERT INTO chats (jid, name) VALUES (?, 'Alice')`, known); err != nil {
		t.Fatal(err)
	}
	f.limit = 1
	f.rebuild(t)
	if code, got := f.get("5511999999999"); code != 200 || got.RateLimited {
		t.Fatal(code, got)
	}
	if code, got := f.get("5511888888888"); code != 200 || got.RateLimited || got.Limit != "" {
		t.Fatalf("known chat consumed the unknown ceiling: %+v", got)
	}
}

func TestProfilePushNameComesFromTheLocalStore(t *testing.T) {
	client := &fakeProfileClient{}
	f := newProfileFixture(t, client)
	f.store = newTestMessageStore(t)
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "contacts.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE whatsmeow_contacts(our_jid TEXT, their_jid TEXT, push_name TEXT)`); err != nil {
		t.Fatal(err)
	}
	jid := "5511999999999@s.whatsapp.net"
	if _, err := db.Exec(`INSERT INTO whatsmeow_contacts VALUES('self', ?, 'Alice')`, jid); err != nil {
		t.Fatal(err)
	}
	f.store.waDB = db
	f.rebuild(t)
	code, got := f.get("5511999999999")
	if code != 200 || got.PushName != "Alice" || got.PushNameSource != profilePushNameSource {
		t.Fatalf("%d %+v", code, got)
	}
}

func TestProfileDisconnectedMissDoesNotBurnQuota(t *testing.T) {
	client := &fakeProfileClient{}
	f := newProfileFixture(t, client)
	f.connected = false
	f.limit = 1
	f.rebuild(t)
	if code, _ := f.get("5511999999999"); code != 503 || client.onCalls != 0 || f.cache.unknownCount != 0 {
		t.Fatalf("disconnected miss burned quota: code=%d count=%d", code, f.cache.unknownCount)
	}
	f.connected = true
	f.rebuild(t)
	if code, got := f.get("5511999999999"); code != 200 || got.RateLimited {
		t.Fatalf("quota was consumed while disconnected: %d %+v", code, got)
	}
}

func TestProfilePartialCacheExpiresQuickly(t *testing.T) {
	client := &fakeProfileClient{pictureErr: errors.New("cdn down"), previewErr: errors.New("cdn down")}
	f := newProfileFixture(t, client)
	if _, got := f.get("5511999999999"); got.PictureStatus != profileStatusError {
		t.Fatal(got)
	}
	f.now = f.now.Add(time.Minute + time.Second)
	if _, got := f.get("5511999999999"); got.Cached || client.pictureCalls < 2 {
		t.Fatalf("partial answer stayed cached: %+v calls=%d", got, client.pictureCalls)
	}
}

func TestProfileRefusesPrivatePictureURLs(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("secret"))
	}))
	defer server.Close()
	if _, err := downloadProfilePicture(context.Background(), server.URL, 1024, nil); err == nil {
		t.Fatal("loopback picture download was accepted")
	}
	for _, raw := range []string{"http://cdn.example.test/photo", "https://user:pass@cdn.example.test/photo", "https:///photo"} {
		if _, err := downloadProfilePicture(context.Background(), raw, 1024, nil); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if !profilePictureIPAllowed(net.ParseIP("8.8.8.8")) || profilePictureIPAllowed(net.ParseIP("10.1.2.3")) || profilePictureIPAllowed(net.ParseIP("127.0.0.1")) || profilePictureIPAllowed(net.ParseIP("169.254.1.1")) {
		t.Fatal("address classification changed")
	}
}

func TestProfileVerifiedNameUsesTheContactStoreForALID(t *testing.T) {
	client := &fakeProfileClient{fakeBusinessClient: fakeBusinessClient{user: types.UserInfo{Status: "hello"}}}
	if client.user.VerifiedName != nil {
		t.Fatal("fake filled VerifiedName, which GetUserInfo does not")
	}
	f := newProfileFixture(t, client)
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "contacts.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE whatsmeow_contacts(our_jid TEXT, their_jid TEXT, push_name TEXT, business_name TEXT)`); err != nil {
		t.Fatal(err)
	}
	lid := types.NewJID("100000000000001", types.HiddenUserServer)
	if _, err := db.Exec(`INSERT INTO whatsmeow_contacts VALUES('self', ?, 'Bob', 'Bob')`, lid.String()); err != nil {
		t.Fatal(err)
	}
	f.store = newTestMessageStore(t)
	f.store.waDB = db
	f.rebuild(t)
	code, got := f.get(lid.String())
	if code != 200 || got.VerifiedName != "Bob" || client.onCalls != 0 || client.user.VerifiedName != nil {
		t.Fatalf("%d %+v calls=%d", code, got, client.onCalls)
	}
}

func TestProfileHiddenOrMissingPictureRemovesTheCachedFile(t *testing.T) {
	phone := "5511999999999"
	registered := "5511888888888"
	for _, tc := range []struct {
		name   string
		err    error
		status string
	}{
		{"hidden", whatsmeow.ErrProfilePictureUnauthorized, profileStatusHidden},
		{"none", whatsmeow.ErrProfilePictureNotSet, profileStatusNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &fakeProfileClient{answers: []types.IsOnWhatsAppResponse{{
				Query: "+" + phone, IsIn: true,
				JID: types.NewJID(registered, types.DefaultUserServer), PhoneNumber: types.NewJID(registered, types.DefaultUserServer),
			}}}
			f := newProfileFixture(t, client)
			code, got := f.get(phone)
			if code != 200 || got.PictureStatus != profileStatusOK || got.PictureMessageID != profilePictureID {
				t.Fatalf("first: %d %+v", code, got)
			}
			if !profilePictureCached(t, f.root, registered+"@s.whatsapp.net") {
				t.Fatal("photo was not stored")
			}
			f.now = f.now.Add(time.Hour + time.Second)
			client.picture = nil
			client.pictureErr = tc.err
			code, got = f.get(phone)
			if code != 200 || got.PictureStatus != tc.status || got.PictureMessageID != "" || client.downloads != 1 {
				t.Fatalf("second: %d %+v downloads=%d", code, got, client.downloads)
			}
			if profilePictureCached(t, f.root, registered+"@s.whatsapp.net") || profilePictureCached(t, f.root, phone+"@s.whatsapp.net") {
				t.Fatal("previous photo stayed readable")
			}
		})
	}
}

func profilePictureCached(t *testing.T, root *os.Root, chat string) bool {
	t.Helper()
	entries, err := fs.ReadDir(root.FS(), chat)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if cachedProfileID(entry.Name()) == profilePictureID {
			return true
		}
	}
	return false
}

func TestProfilePictureDownloadUsesTheProxyMediaClient(t *testing.T) {
	var hits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Method != http.MethodConnect {
			t.Errorf("method %s host %s", r.Method, r.Host)
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer proxy.Close()
	u, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != u.Host {
			t.Errorf("profile picture dialed %s instead of the media proxy", address)
			return nil, errors.New("direct dial")
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	var media atomic.Pointer[http.Client]
	factory := proxyClientFactoryTransport(nil, u, transport, &media)
	if factory(newTestClient(&mockLIDStore{}).Store, waLog.Noop) == nil || media.Load() == nil {
		t.Fatal("proxy did not install the media client")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := downloadProfilePicture(ctx, "http://cdn.example.test/photo.jpg", 1024, media.Load()); err == nil {
		t.Fatal("plain http photo was accepted")
	}
	if hits.Load() != 0 {
		t.Fatal("refused url still reached the proxy")
	}
	_, err = downloadProfilePicture(ctx, "https://cdn.example.test/photo.jpg", 1024, media.Load())
	if err == nil || hits.Load() == 0 {
		t.Fatalf("proxy hits=%d err=%v", hits.Load(), err)
	}
}
