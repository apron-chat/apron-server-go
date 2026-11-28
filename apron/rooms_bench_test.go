package apron_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strconv"
	"testing"

	"github.com/apron-chat/apron-server-go/apron"
)

// roomList is a room_list result of 1,000 rooms, each with 10 members
// when members is set.
func roomList(members bool) apron.RoomListResult {
	var r apron.RoomListResult
	for i := range 1000 {
		h := apron.LogID(1_700_000_000_000)
		v := apron.RoomView{Room: apron.Room{RoomID: "room" + strconv.Itoa(i), LogID: h + apron.LogID(i), Title: "Room number " + strconv.Itoa(i), Description: "A room for talking about things, number " + strconv.Itoa(i)}, LatestLogID: h + 5000, HistoryLogID: &h}
		if members {
			for j := range 10 {
				v.Members = append(v.Members, apron.CurrentUser{User: apron.User{UserID: "guest_" + strconv.Itoa(j), Name: "Guest " + strconv.Itoa(j)}, Cleared: []string{"status"}})
			}
			v.MemberCount = 10
		}
		r.Joined = append(r.Joined, v)
	}
	return r
}

// BenchmarkRoomListEncode encodes a room_list result of 1,000 rooms,
// without members (rooms) and with 10 each (members), as a server answers
// room_list: each room typed (typed), each decoded first from the record
// the server keeps encoded (decoded), as a server that keeps its rooms so
// does without RoomView.Raw, or each sent as kept (raw, RoomView.Raw).
func BenchmarkRoomListEncode(b *testing.B) {
	for _, members := range []bool{false, true} {
		name := "rooms"
		if members {
			name = "members"
		}
		r := roomList(members)
		raws := make([]jsontext.Value, len(r.Joined))
		for i, v := range r.Joined {
			raws[i], _ = apron.Marshal(v.Room)
		}
		for _, how := range []string{"typed", "decoded", "raw"} {
			b.Run(name+"/"+how, func(b *testing.B) {
				for b.Loop() {
					result := r
					switch how {
					case "decoded":
						result.Joined = make([]apron.RoomView, len(r.Joined))
						for i, v := range r.Joined {
							v.Room = apron.Room{}
							if err := json.Unmarshal(raws[i], &v.Room); err != nil {
								b.Fatal(err)
							}
							result.Joined[i] = v
						}
					case "raw":
						result.Joined = make([]apron.RoomView, len(r.Joined))
						for i, v := range r.Joined {
							v.Room, v.Raw = apron.Room{}, raws[i]
							result.Joined[i] = v
						}
					}
					if _, err := apron.Marshal(&result); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
