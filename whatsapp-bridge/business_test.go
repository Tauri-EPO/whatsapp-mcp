package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waVnameCert"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

type fakeBusinessClient struct {
	user                    types.UserInfo
	profile                 *types.BusinessProfile
	userErr, profileErr     error
	userCalls, profileCalls int
}

func (f *fakeBusinessClient) GetUserInfo(_ context.Context, jids []types.JID) (map[types.JID]types.UserInfo, error) {
	f.userCalls++
	return map[types.JID]types.UserInfo{jids[0]: f.user}, f.userErr
}
func (f *fakeBusinessClient) GetBusinessProfile(context.Context, types.JID) (*types.BusinessProfile, error) {
	f.profileCalls++
	return f.profile, f.profileErr
}

func TestBusinessAnswersAndCache(t *testing.T) {
	for _, tc := range []struct {
		name     string
		client   fakeBusinessClient
		business *bool
		limited  bool
	}{
		{"business", fakeBusinessClient{user: types.UserInfo{VerifiedName: &types.VerifiedName{Details: &waVnameCert.VerifiedNameCertificate_Details{VerifiedName: proto.String("Alice")}}}, profile: &types.BusinessProfile{Email: "alice@example.test", Address: "Alice Street", ProfileOptions: map[string]string{"website": "https://example.test"}, Categories: []types.Category{{Name: "Shop"}}}}, proto.Bool(true), false},
		{"person", fakeBusinessClient{profileErr: whatsmeow.ErrIQNotFound}, proto.Bool(false), false},
		{"empty personal profile", fakeBusinessClient{profile: &types.BusinessProfile{}}, proto.Bool(false), false},
		{"429 with profile", fakeBusinessClient{userErr: fmt.Errorf("lookup: %w", &whatsmeow.IQError{Code: 429}), profile: &types.BusinessProfile{Email: "alice@example.test"}}, proto.Bool(true), true},
		{"unknown", fakeBusinessClient{userErr: errors.New("offline"), profileErr: errors.New("offline")}, nil, false},
		{"profile unavailable", fakeBusinessClient{profileErr: errors.New("offline")}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := businessCache{}
			jid := types.NewJID("5511999999999", types.DefaultUserServer)
			now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
			cache.now = func() time.Time { return now }
			got := cache.lookup(context.Background(), &tc.client, jid)
			if (got.IsBusiness == nil) != (tc.business == nil) || (got.IsBusiness != nil && *got.IsBusiness != *tc.business) || got.RateLimited != tc.limited {
				t.Fatalf("%+v", got)
			}
			if tc.client.profileCalls != 1 || tc.client.userCalls != 1 {
				t.Fatal("independent query missing")
			}
			if tc.name == "business" && (got.VerifiedName != "Alice" || got.Email != "alice@example.test" || got.Website != "https://example.test" || len(got.Categories) != 1) {
				t.Fatalf("%+v", got)
			}
			again := cache.lookup(context.Background(), &tc.client, jid)
			if tc.business != nil {
				if !again.Cached || again.VerifiedNameError != got.VerifiedNameError || tc.client.userCalls != 1 {
					t.Fatal(again)
				}
				now = now.Add(time.Hour)
				cache.lookup(context.Background(), &tc.client, jid)
				if tc.client.userCalls != 2 {
					t.Fatal("expired entry reused")
				}
			} else if again.Cached {
				t.Fatal("cached outage")
			}
		})
	}
}

func TestBusinessCacheBoundAndAuthorization(t *testing.T) {
	cache := businessCache{}
	client := &fakeBusinessClient{}
	for i := 0; i < 260; i++ {
		cache.lookup(context.Background(), client, types.NewJID(fmt.Sprintf("100000000%06d", i), types.HiddenUserServer))
	}
	if len(cache.entries) != 256 {
		t.Fatal(len(cache.entries))
	}
	jid := "5511999999999@s.whatsapp.net"
	policy := chatPolicy{}
	resolver := chatPolicyHandle{resolve: func() chatPolicy { return policy }}
	h := handleCheckBusiness(client, &cache, resolver, func() bool { return true })
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest("GET", "/api/business?jid="+jid, nil))
	var answer businessResult
	if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || answer.IsBusiness == nil || *answer.IsBusiness {
		t.Fatal(w.Code, answer)
	}
	before := client.userCalls
	policy = parseChatPolicy("5511888888888")
	w = httptest.NewRecorder()
	h(w, httptest.NewRequest("GET", "/api/business?jid="+jid, nil))
	if w.Code != 403 || client.userCalls != before {
		t.Fatal("cached answer bypassed new policy")
	}
}

func TestBusinessPartialCacheExpiresQuickly(t *testing.T) {
	for _, scenario := range []string{"complete", "rate-limited", "profile-failure", "user-failure"} {
		t.Run(scenario, func(t *testing.T) {
			client := &fakeBusinessClient{profile: &types.BusinessProfile{Email: "alice@example.test"}}
			if scenario == "rate-limited" {
				client.userErr = &whatsmeow.IQError{Code: 429}
			}
			if scenario == "user-failure" {
				client.userErr = errors.New("offline")
			}
			if scenario == "profile-failure" {
				client.user = types.UserInfo{VerifiedName: &types.VerifiedName{Details: &waVnameCert.VerifiedNameCertificate_Details{VerifiedName: proto.String("Alice")}}}
				client.profileErr = errors.New("offline")
			}
			now := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
			cache := businessCache{now: func() time.Time { return now }}
			jid := types.NewJID("5511999999999", types.DefaultUserServer)
			cache.lookup(context.Background(), client, jid)
			now = now.Add(time.Minute + time.Second)
			answer := cache.lookup(context.Background(), client, jid)
			if scenario != "complete" && (answer.Cached || client.userCalls != 2) {
				t.Fatalf("rate limit remained cached: %+v calls=%d", answer, client.userCalls)
			}
			if scenario == "complete" && (!answer.Cached || client.userCalls != 1) {
				t.Fatal("complete answer did not retain its hour cache")
			}
		})
	}
}
