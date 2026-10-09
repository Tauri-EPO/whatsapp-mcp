package main

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

// Embed only to satisfy the unused SDK operations; an accidental call to one
// of those methods fails the test rather than pretending it worked.
type topicSDKProbe struct {
	groupManagementClient
	ctx                   context.Context
	jid                   types.JID
	previous, next, topic string
	fetches, writes       int
	info                  *types.GroupInfo
	fail                  error
}

func (p *topicSDKProbe) GetGroupInfo(ctx context.Context, jid types.JID) (*types.GroupInfo, error) {
	p.ctx, p.jid = ctx, jid
	p.fetches++
	return p.info, p.fail
}

func (p *topicSDKProbe) SetGroupTopic(ctx context.Context, jid types.JID, previousID, newID, topic string) error {
	p.ctx, p.jid = ctx, jid
	p.previous, p.next, p.topic = previousID, newID, topic
	p.writes++
	return p.fail
}

func TestGroupTopicSDKAdapterArgumentPlacement(t *testing.T) {
	for _, previous := range []string{"previous-topic", ""} {
		for _, description := range []string{"new description", ""} {
			t.Run(previous+"/"+description, func(t *testing.T) {
				jid := types.NewJID("120363000000000001", types.GroupServer)
				probe := &topicSDKProbe{info: &types.GroupInfo{JID: jid, GroupTopic: types.GroupTopic{TopicID: previous}}}
				ops := liveGroupOps(probe, func() bool { return true })
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				info, err := ops.getInfo(ctx, jid)
				if err != nil || info != probe.info || probe.ctx != ctx || probe.jid != jid || probe.fetches != 1 {
					t.Fatalf("SDK fetch: %+v, %v", probe, err)
				}
				if err := ops.setTopic(ctx, jid, groupTopicUpdate{PreviousID: info.TopicID, Description: description}); err != nil {
					t.Fatal(err)
				}
				if probe.previous != previous || probe.next != "" || probe.topic != description || probe.writes != 1 || probe.ctx != ctx || probe.jid != jid {
					t.Fatalf("SetGroupTopic SDK arguments: %+v", probe)
				}
				probe.fail = errors.New("synthetic SDK failure")
				if err := ops.setTopic(ctx, jid, groupTopicUpdate{}); !errors.Is(err, probe.fail) {
					t.Fatalf("SDK error lost: %v", err)
				}
			})
		}
	}
}

func TestGroupTopicSDKAdapterOffline(t *testing.T) {
	probe := &topicSDKProbe{}
	ops := liveGroupOps(probe, func() bool { return false })
	if _, err := ops.getInfo(context.Background(), types.EmptyJID); err == nil {
		t.Fatal("offline fetch succeeded")
	}
	if err := ops.setTopic(context.Background(), types.EmptyJID, groupTopicUpdate{}); err == nil {
		t.Fatal("offline topic update succeeded")
	}
	if probe.fetches != 0 || probe.writes != 0 {
		t.Fatalf("offline adapter reached SDK: %+v", probe)
	}
	var sdk *whatsmeow.Client
	ops = liveGroupOps(sdk, func() bool { return true })
	if _, err := ops.getInfo(context.Background(), types.EmptyJID); err == nil {
		t.Fatal("nil SDK client fetch succeeded")
	}
	if err := ops.setTopic(context.Background(), types.EmptyJID, groupTopicUpdate{}); err == nil {
		t.Fatal("nil SDK client update succeeded")
	}
}

func TestGroupSubjectTopicAndCanonicalRoster(t *testing.T) {
	for _, previous := range []string{"previous-topic", ""} {
		for _, description := range []string{"  new description  ", "  "} {
			t.Run(previous+"/"+description, func(t *testing.T) {
				store := newTestMessageStore(t)
				b := testBridge(t, nil, store, testLogger())
				oldAt := time.Now().Add(-time.Hour)
				oldRow, _ := newGroupMemberRow("888", "", "888")
				if err := store.AddGroupMembers(mgGroup, []groupMemberRow{oldRow}, oldAt); err != nil {
					t.Fatal(err)
				}
				fetches, topics, records := 0, 0, 0
				var fetchStarted time.Time
				ops := groupOps{
					getInfo: func(_ context.Context, jid types.JID) (*types.GroupInfo, error) {
						fetchStarted = time.Now()
						fetches++
						if jid.String() != mgGroup {
							t.Fatalf("fetch group = %s", jid)
						}
						// A join newer than the request must survive the snapshot sweep.
						joined, _ := newGroupMemberRow("5511888888888", "5511888888888", "777")
						if err := store.AddGroupMembers(mgGroup, []groupMemberRow{joined}, fetchStarted.Add(2*time.Second)); err != nil {
							t.Fatal(err)
						}
						return &types.GroupInfo{JID: jid, GroupTopic: types.GroupTopic{TopicID: previous}, Participants: []types.GroupParticipant{
							{JID: types.NewJID("5511999999999", types.DefaultUserServer), IsSuperAdmin: true, DisplayName: "Alice"},
						}}, nil
					},
					setTopic: func(_ context.Context, jid types.JID, topic groupTopicUpdate) error {
						topics++
						if jid.String() != mgGroup || topic.PreviousID != previous || topic.Description != strings.TrimSpace(description) {
							t.Fatalf("topic arguments: %s %+v", jid, topic)
						}
						if records != 1 || readMembers(t, store, mgGroup)["5511999999999"].source != "roster" {
							t.Fatal("roster was not cached before the topic update")
						}
						return nil
					},
				}
				var stamp time.Time
				record := func(jid string, members []GroupMember, at time.Time) {
					records++
					stamp = at
					if at.After(fetchStarted) {
						t.Fatal("roster stamp was captured after the fetch started")
					}
					b.recordGroupRoster(jid, members, at)
				}
				body := `{"group_jid":" ` + mgGroup + ` ","description":"` + description + `"}`
				code, resp := groupPost(t, handleGroupSubject(ops, chatPolicy{}, record), body)
				if code != http.StatusOK || !resp.Success || !reflect.DeepEqual(resp.Changed, []string{"description"}) || fetches != 1 || topics != 1 || records != 1 {
					t.Fatalf("description: %d %+v fetch=%d topic=%d record=%d", code, resp, fetches, topics, records)
				}
				members := readMembers(t, store, mgGroup)
				alice := members["5511999999999"]
				if len(members) != 2 || alice.phone != "5511999999999" || alice.name != "Alice" || !alice.isAdmin || !alice.isSuper || alice.lastSeen != dbTime(stamp) {
					t.Fatalf("canonical SQLite roster: %+v", members)
				}
				if _, ok := members["5511888888888"]; !ok {
					t.Fatal("join during fetch was removed")
				}
			})
		}
	}
}

func TestGroupSubjectPartialFailures(t *testing.T) {
	for _, stage := range []string{"fetch", "nil-info", "topic"} {
		for _, rename := range []bool{false, true} {
			t.Run(stage+"/rename="+strconv.FormatBool(rename), func(t *testing.T) {
				store := newTestMessageStore(t)
				b := testBridge(t, nil, store, testLogger())
				names, topics, fetches := 0, 0, 0
				ops := groupOps{
					setName: func(_ context.Context, _ types.JID, name string) error { names++; return nil },
					getInfo: func(_ context.Context, jid types.JID) (*types.GroupInfo, error) {
						fetches++
						if stage == "fetch" {
							return nil, errors.New("synthetic fetch failure")
						}
						if stage == "nil-info" {
							return nil, nil
						}
						return &types.GroupInfo{JID: jid, Participants: []types.GroupParticipant{{JID: types.NewJID("5511999999999", types.DefaultUserServer)}}}, nil
					},
					setTopic: func(_ context.Context, _ types.JID, _ groupTopicUpdate) error {
						topics++
						return errors.New("synthetic topic failure")
					},
				}
				name := ""
				if rename {
					name = `,"name":"New group"`
				}
				code, resp := groupPost(t, handleGroupSubject(ops, chatPolicy{}, b.recordGroupRoster), `{"group_jid":"`+mgGroup+`","description":"new"`+name+`}`)
				if code != http.StatusBadGateway || resp.Success || fetches != 1 || names != map[bool]int{false: 0, true: 1}[rename] {
					t.Fatalf("failure: %d %+v names=%d fetches=%d", code, resp, names, fetches)
				}
				if rename {
					if !reflect.DeepEqual(resp.Changed, []string{"name"}) || !strings.Contains(resp.Message, "group was renamed") || !strings.Contains(resp.Message, "retry only description") {
						t.Fatalf("partial result lost: %+v", resp)
					}
				} else if len(resp.Changed) != 0 || strings.Contains(resp.Message, "renamed") {
					t.Fatalf("false partial result: %+v", resp)
				}
				if stage == "topic" {
					if topics != 1 || len(readMembers(t, store, mgGroup)) != 1 || !strings.Contains(resp.Message, "set description failed") {
						t.Fatalf("topic failure: %+v topics=%d", resp, topics)
					}
				} else if topics != 0 || len(readMembers(t, store, mgGroup)) != 0 || !strings.Contains(resp.Message, "could not read group info") {
					t.Fatalf("fetch failure: %+v topics=%d", resp, topics)
				}
			})
		}
	}
}

func TestGroupSubjectNameOnlyAndNameFailure(t *testing.T) {
	for _, fail := range []error{nil, errors.New("synthetic name failure")} {
		ops := groupOps{setName: func(_ context.Context, _ types.JID, name string) error {
			if name != "New group" {
				t.Fatalf("name = %q", name)
			}
			return fail
		}}
		body := `{"group_jid":"` + mgGroup + `","name":" New group "}`
		if fail != nil {
			body = `{"group_jid":"` + mgGroup + `","name":" New group ","description":"new"}`
		}
		code, resp := groupPost(t, handleGroupSubject(ops, chatPolicy{}, nil), body)
		if fail == nil {
			if code != http.StatusOK || !reflect.DeepEqual(resp.Changed, []string{"name"}) {
				t.Fatalf("name-only: %d %+v", code, resp)
			}
		} else if code != http.StatusBadGateway || len(resp.Changed) != 0 || strings.Contains(resp.Message, "renamed") {
			t.Fatalf("name failure: %d %+v", code, resp)
		}
	}
}

func TestGroupSubjectDenyBeforeEffects(t *testing.T) {
	// Nil operations prove authorization precedes both the new fetch and writes.
	code, _ := groupPost(t, handleGroupSubject(groupOps{}, parseChatPolicy("5511999999999"), nil), `{"group_jid":"`+mgGroup+`","name":"New group","description":"new"}`)
	if code != http.StatusForbidden {
		t.Fatalf("chat policy: %d", code)
	}
	for _, tc := range []struct {
		name        string
		readOnly    bool
		allow, deny string
	}{
		{"read-only", true, "update_group", ""},
		{"allow-list", false, "send_reaction", ""},
		{"deny-list", false, "update_group", "update_group"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := toolPolicyRequest(t, tc.readOnly, mustToolPolicy(t, tc.allow, tc.deny), "/api/group/subject", `{"group_jid":"`+mgGroup+`","name":"New group","description":"new"}`)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("REST deny: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}
