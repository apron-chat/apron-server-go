package client_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/apron-chat/apron-server-go/apron"
	"github.com/apron-chat/apron-server-go/apron/client"
	"github.com/apron-chat/apron-server-go/apron/server"
)

// Connect opens a connection without signing in, whose server frame says
// which schemes to pick from; Auth then signs it in.
func TestConnectThenAuth(t *testing.T) {
	ctx := timeout(t)
	c := newChat(server.Config{})
	conn, err := client.Connect(ctx, c.serve(t))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if hello := conn.Hello(); !hello.Offers("token") || hello.Offers("email") {
		t.Fatalf("hello %+v", hello)
	}
	if u := conn.User(); u.UserID != "" {
		t.Fatalf("user before auth %+v", u)
	}
	if _, err := conn.Post(ctx, text("hi")); apron.CodeOf(err) != apron.CodeDenied {
		t.Fatalf("post before auth: %v", err)
	}
	// A refused auth changes nothing; the connection stays open.
	if _, err := conn.Auth(ctx, apron.AuthParams{Scheme: "token", Token: "eve"}); apron.CodeOf(err) != apron.CodeDenied {
		t.Fatalf("bad token: %v", err)
	}
	result, err := conn.Auth(ctx, apron.AuthParams{Scheme: "token", Token: "ada"})
	if err != nil || result.You == nil || result.You.UserID != "ada" || conn.User().UserID != "ada" {
		t.Fatalf("auth: %+v, %v; user %+v", result, err, conn.User())
	}
	if _, err := conn.Post(ctx, text("hi")); err != nil {
		t.Fatal(err)
	}
}

// HistoryPages pages forward from after, and back from the newest, with
// the continuations of §4.3.2, until more is false.
func TestHistoryPages(t *testing.T) {
	ctx := timeout(t)
	c := newChat(server.Config{})
	conn := dial(t, c.serve(t), apron.AuthParams{Scheme: "guest"})
	var posted []apron.LogID
	for i := range 7 {
		sent, err := conn.Post(ctx, text(fmt.Sprint(i)))
		if err != nil {
			t.Fatal(err)
		}
		posted = append(posted, sent.MessageID)
	}
	read := func(p apron.HistoryParams, stopAfter int) (ids []apron.LogID, pages int) {
		for page, err := range conn.HistoryPages(ctx, p) {
			if err != nil {
				t.Fatal(err)
			}
			pages++
			for _, m := range page.Messages {
				ids = append(ids, m.MessageID)
			}
			if pages == stopAfter {
				break
			}
		}
		return ids, pages
	}
	zero := apron.LogID(0)
	if ids, pages := read(apron.HistoryParams{After: &zero, Limit: 3}, 0); pages != 3 || !slices.Equal(ids, posted) {
		t.Fatalf("forward: %d pages, %v; want 3, %v", pages, ids, posted)
	}
	ids, pages := read(apron.HistoryParams{Limit: 3}, 0)
	slices.Sort(ids)
	if pages != 3 || !slices.Equal(ids, posted) {
		t.Fatalf("backward: %d pages, %v", pages, ids)
	}
	// Bounded by before, forward stops there.
	if ids, _ := read(apron.HistoryParams{After: &zero, Before: &posted[3], Limit: 2}, 0); !slices.Equal(ids, posted[:4]) {
		t.Fatalf("forward to before: %v", ids)
	}
	// A loop that stops sends no more requests.
	if ids, pages := read(apron.HistoryParams{After: &zero, Limit: 2}, 1); pages != 1 || len(ids) != 2 {
		t.Fatalf("stopped: %d pages, %v", pages, ids)
	}
	// An error ends it.
	conn.Close()
	for page, err := range conn.HistoryPages(ctx, apron.HistoryParams{}) {
		if err == nil || page.More {
			t.Fatalf("after close: %+v, %v", page, err)
		}
	}
}

// The room requests send their params as the protocol names them.
func TestRoomRequests(t *testing.T) {
	ctx := timeout(t)
	c := newChat(server.Config{})
	conn := dial(t, c.serve(t), apron.AuthParams{Scheme: "guest"})
	if _, err := conn.Post(ctx, text("hi")); err != nil {
		t.Fatal(err)
	}
	latest := apron.LogID(5)
	listed, err := conn.RoomList(ctx, apron.RoomListParams{Filter: apron.RoomsJoined, Members: true, LatestLogID: &latest})
	if err != nil || len(listed.Joined) != 1 || listed.Joined[0].RoomID != "general" || listed.Joined[0].HistoryLogID == nil {
		t.Fatalf("room_list: %+v, %v", listed, err)
	}
	if err := conn.RoomJoin(ctx, apron.RoomJoinParams{RoomID: "general", UserID: "bob"}); err != nil {
		t.Fatal(err)
	}
	if err := conn.RoomLeave(ctx, apron.RoomLeaveParams{RoomID: "general"}); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.lists) != 1 || c.lists[0].Filter != apron.RoomsJoined || !c.lists[0].Members || c.lists[0].LatestLogID == nil || *c.lists[0].LatestLogID != 5 {
		t.Errorf("room_list params %+v", c.lists)
	}
	if len(c.joins) != 1 || c.joins[0] != (apron.RoomJoinParams{RoomID: "general", UserID: "bob"}) {
		t.Errorf("room_join params %+v", c.joins)
	}
	if len(c.leaves) != 1 || c.leaves[0] != (apron.RoomLeaveParams{RoomID: "general"}) {
		t.Errorf("room_leave params %+v", c.leaves)
	}
}
