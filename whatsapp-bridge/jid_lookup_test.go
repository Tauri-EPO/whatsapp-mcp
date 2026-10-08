package main

import (
	"context"
	"errors"
	"testing"

	"go.mau.fi/whatsmeow/types"
)

func TestSharedLIDLookupDirectionsAndMissingBackend(t *testing.T) {
	phone := types.NewJID("12025551234", types.DefaultUserServer)
	lid := types.NewJID("111222333444555", types.HiddenUserServer)
	client := newTestClientWithSelf(&mockLIDStore{lidByPN: map[types.JID]types.JID{phone: lid}, pnByLID: map[types.JID]types.JID{lid: phone}}, phone)
	for _, pair := range [][2]types.JID{{phone, lid}, {lid, phone}, {types.NewJID("120363000000000001", types.GroupServer), types.EmptyJID}} {
		got, err := lookupAltJID(context.Background(), client, pair[0])
		if err != nil || got != pair[1] {
			t.Fatalf("lookup %s=%s %v, want %s", pair[0], got, err, pair[1])
		}
	}
	client.Store.LIDs = nil
	if got, err := lookupAltJID(context.Background(), client, phone); !got.IsEmpty() || !errors.Is(err, errLIDStoreUnavailable) {
		t.Fatalf("missing map=%s %v", got, err)
	}
	if gotPhone, gotLID := clientIdentity(client)(context.Background()); gotPhone != phone || !gotLID.IsEmpty() {
		t.Fatalf("own identity=%s %s", gotPhone, gotLID)
	}
}
