package server

import (
	"context"
	"slices"

	"github.com/apron-chat/apron-server-go/apron"
)

// RoomHandler handles room_list, room_join, room_leave, and room_set, and
// adds capability rooms (§4.2). A backend sends room_update itself. A
// join's or leave's UserID is empty unless the client names another user
// to add or remove; a backend that does not support that returns an
// Unsupported error. A backend that lists rooms but takes no other room
// request embeds ReadOnlyRooms.
type RoomHandler interface {
	// RoomList answers with the rooms, which Send makes what the filter
	// asks for (RoomListResult).
	RoomList(ctx context.Context, c *Conn, p apron.RoomListParams, reply Reply[apron.RoomListResult]) error
	RoomJoin(ctx context.Context, c *Conn, p apron.RoomJoinParams, reply Reply[apron.OK]) error
	RoomLeave(ctx context.Context, c *Conn, p apron.RoomLeaveParams, reply Reply[apron.OK]) error
	RoomSet(ctx context.Context, c *Conn, p apron.RoomSetParams, reply Reply[apron.RoomSetResult]) error
}

// ReadOnlyRooms refuses room_join, room_leave, and room_set as
// error/unsupported, for a RoomHandler that embeds it and implements only
// RoomList, such as a gateway to a chat system whose rooms its users
// cannot change.
type ReadOnlyRooms struct{}

// RoomJoin refuses a join.
func (ReadOnlyRooms) RoomJoin(context.Context, *Conn, apron.RoomJoinParams, Reply[apron.OK]) error {
	return apron.Unsupported("This server does not join rooms on request")
}

// RoomLeave refuses a leave.
func (ReadOnlyRooms) RoomLeave(context.Context, *Conn, apron.RoomLeaveParams, Reply[apron.OK]) error {
	return apron.Unsupported("This server does not leave rooms on request")
}

// RoomSet refuses to make or edit a room.
func (ReadOnlyRooms) RoomSet(context.Context, *Conn, apron.RoomSetParams, Reply[apron.RoomSetResult]) error {
	return apron.Unsupported("This server does not make or edit rooms on request")
}

// normalizeRoomList makes a room_list result what its params ask for: the
// arrays the filter asks for are present even when empty (§4.2.1), and the
// others absent, and without members, no room has its members, nor the
// result users.
func normalizeRoomList(p apron.RoomListParams, result apron.RoomListResult) apron.RoomListResult {
	if p.Filter != apron.FilterNotJoined && result.Joined == nil {
		result.Joined = []apron.RoomView{}
	}
	if p.Filter != apron.FilterJoined && result.NotJoined == nil {
		result.NotJoined = []apron.RoomView{}
	}
	if p.Filter == apron.FilterJoined {
		result.NotJoined = nil
	}
	if p.Filter == apron.FilterNotJoined {
		result.Joined = nil
	}
	if !p.Members {
		result.Users = nil
		// Copies, so that the backend's own slices stay as they are.
		for _, list := range []*[]apron.RoomView{&result.Joined, &result.NotJoined} {
			if *list == nil {
				continue
			}
			*list = slices.Clone(*list)
			for i := range *list {
				(*list)[i].Members, (*list)[i].MemberCount = nil, 0
			}
		}
	}
	return result
}
