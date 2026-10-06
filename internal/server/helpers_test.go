package server

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type testClient struct {
	ws               *websocket.Conn
	passkeyChallenge string
	// requests numbers the request IDs the helpers generate.
	requests int
	// userID is the identity the guest was assigned at auth.
	userID string
	// statuses keeps the bare status announcements of others (§4.5),
	// `user` `{new: {user_id, status}}`, which read otherwise skips: they
	// follow every connection, idle change, and join, and only the status
	// tests look at them.
	statuses bool
}

// nextID returns a request ID not used before on this client.
func (c *testClient) nextID(prefix string) string {
	c.requests++
	return fmt.Sprintf("%s-%d", prefix, c.requests)
}

func newTestServer(t *testing.T, config Config) (*Server, *httptest.Server) {
	t.Helper()
	app := New(config)
	httpServer := httptest.NewServer(app.Handler())
	t.Cleanup(func() {
		// Generous, since -race on a busy machine slows every goroutine.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := app.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
		cancel()
		httpServer.Close()
	})
	return app, httpServer
}

// wsURL is the WebSocket endpoint of a test server.
func wsURL(httpServer *httptest.Server) string {
	return "ws" + httpServer.URL[len("http"):] + "/ws"
}

// dialRaw connects without signing in and returns the server frame.
func dialRaw(t *testing.T, httpServer *httptest.Server) (*testClient, map[string]any) {
	t.Helper()
	return dialOrigin(t, httpServer, "")
}

// dialOrigin connects as a page on origin, if given, and returns the server
// frame.
func dialOrigin(t *testing.T, httpServer *httptest.Server, origin string) (*testClient, map[string]any) {
	t.Helper()
	var options *websocket.DialOptions
	if origin != "" {
		options = &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {origin}}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	ws, _, err := websocket.Dial(ctx, wsURL(httpServer), options)
	cancel()
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	ws.SetReadLimit(1 << 24)
	c := &testClient{ws: ws}
	t.Cleanup(func() { _ = ws.Close(websocket.StatusNormalClosure, "test finished") })
	serverFrame := c.read(t)
	if serverFrame["method"] != "server" {
		t.Fatalf("first frame = %#v, want server announcement", serverFrame)
	}
	return c, serverFrame
}

// guestAuth signs c in as a guest and returns the auth result, which follows
// the membership of the guest's join to general.
func guestAuth(t *testing.T, c *testClient) map[string]any {
	t.Helper()
	before, result := c.request(t, "auth", c.nextID("auth"), map[string]any{"scheme": "guest"})
	c.userID = result["you"].(map[string]any)["user_id"].(string)
	if len(before) != 1 {
		t.Fatalf("frames before the guest auth result: %#v", before)
	}
	checkMembership(t, membershipOnly(t, before[0]), "general", c.userID, true)
	return result
}

// membershipOf returns the one membership record of a room_update frame
// (§4.3.3).
func membershipOf(t *testing.T, frame map[string]any) map[string]any {
	t.Helper()
	update := notificationParams(t, frame, "room_update")
	records, ok := update["memberships"].([]any)
	if !ok || len(records) != 1 {
		t.Fatalf("room_update = %#v, want one membership record", update)
	}
	return records[0].(map[string]any)
}

// membershipOnly returns the membership record of a room_update that
// carries nothing else, as the room's other members receive it.
func membershipOnly(t *testing.T, frame map[string]any) map[string]any {
	t.Helper()
	if update := notificationParams(t, frame, "room_update"); len(update) != 1 {
		t.Fatalf("room_update = %#v, want its membership alone", update)
	}
	return membershipOf(t, frame)
}

// dialTestClient signs in a new guest. The guest's join to general is a
// logged membership, delivered to its connection before the auth result;
// nothing follows the result: clients list their rooms with room_list.
func dialTestClient(t *testing.T, httpServer *httptest.Server) *testClient {
	t.Helper()
	c, _ := dialRaw(t, httpServer)
	guestAuth(t, c)
	c.expectQuiet(t)
	return c
}

// dialGroup signs in n guests, in order. New guests join general, so each
// earlier guest reads the later ones' memberships.
func dialGroup(t *testing.T, httpServer *httptest.Server, n int) []*testClient {
	t.Helper()
	clients := make([]*testClient, 0, n)
	for i := range n {
		c := dialTestClient(t, httpServer)
		if c.userID != fmt.Sprintf("guest_%d", i+1) {
			t.Fatalf("guest %d was assigned %q", i+1, c.userID)
		}
		for _, earlier := range clients {
			expectMembership(t, earlier, "general", c.userID, true)
		}
		clients = append(clients, c)
	}
	return clients
}

// expectMembership reads a room_update carrying only a membership record:
// one entry, for userID in roomID, as the room's other members receive it.
// It returns the record.
func expectMembership(t *testing.T, c *testClient, roomID, userID string, joined bool) map[string]any {
	t.Helper()
	params := membershipOnly(t, c.read(t))
	checkMembership(t, params, roomID, userID, joined)
	return params
}

// expectLeft reads the room_update that tells a user's connection it left or
// was removed from roomID: left and the membership. It returns the record.
func expectLeft(t *testing.T, c *testClient, roomID, userID string) map[string]any {
	t.Helper()
	return checkLeft(t, c.read(t), roomID, userID)
}

// checkLeft checks a room_update with left for roomID and the leave of
// userID, and returns the membership record.
func checkLeft(t *testing.T, frame map[string]any, roomID, userID string) map[string]any {
	t.Helper()
	update := notificationParams(t, frame, "room_update")
	if len(update) != 2 || !reflect.DeepEqual(update["left"], []any{map[string]any{"room_id": roomID}}) {
		t.Fatalf("room_update = %#v, want left %s and its membership", update, roomID)
	}
	membership := membershipOf(t, frame)
	checkMembership(t, membership, roomID, userID, false)
	return membership
}

// checkMembership checks a live membership record's shape.
func checkMembership(t *testing.T, params map[string]any, roomID, userID string, joined bool) {
	t.Helper()
	members, ok := params["members"].([]any)
	if !ok || len(members) != 1 || len(params) != 3 || params["room_id"] != roomID {
		t.Fatalf("membership = %#v, want one entry in %s", params, roomID)
	}
	parseID(t, params["log_id"])
	entry := members[0].(map[string]any)
	user, _ := entry["user"].(map[string]any)
	if len(entry) != 2 || user["user_id"] != userID || entry["joined"] != joined {
		t.Fatalf("membership entry = %#v, want %s joined=%v", entry, userID, joined)
	}
}

// listRooms sends room_list and returns its result.
func listRooms(t *testing.T, c *testClient, params map[string]any) map[string]any {
	t.Helper()
	return c.result(t, "room_list", c.nextID("list"), params)
}

// roomIDs lists the room_id of each room record in a room_list array.
func roomIDs(t *testing.T, value any) []string {
	t.Helper()
	list, ok := value.([]any)
	if !ok {
		t.Fatalf("room list %#v is not an array", value)
	}
	ids := make([]string, len(list))
	for i, entry := range list {
		ids[i] = entry.(map[string]any)["room_id"].(string)
	}
	return ids
}

// drain returns every frame queued before a fence request's reply.
func (c *testClient) drain(t *testing.T) []map[string]any {
	t.Helper()
	id := c.nextID("fence")
	c.write(t, map[string]any{"method": "fence", "id": id})
	frames := make([]map[string]any, 0)
	for {
		frame := c.read(t)
		if frame["id"] == id {
			return frames
		}
		frames = append(frames, frame)
	}
}

func (c *testClient) expectQuiet(t *testing.T) {
	t.Helper()
	if frames := c.drain(t); len(frames) != 0 {
		t.Fatalf("unexpected frames: %#v", frames)
	}
}

func (c *testClient) write(t *testing.T, value any) {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageText, payload); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

func (c *testClient) read(t *testing.T) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, payload, err := c.ws.Read(ctx)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	var frame map[string]any
	if err := json.Unmarshal(payload, &frame); err != nil {
		t.Fatalf("decode frame %q: %v", payload, err)
	}
	if !c.statuses && isStatusAnnouncement(frame) {
		return c.read(t)
	}
	return frame
}

// isStatusAnnouncement reports whether frame is a bare announcement of
// another user's status: `user` with only `new`, which has only user_id and
// status.
func isStatusAnnouncement(frame map[string]any) bool {
	params, _ := frame["params"].(map[string]any)
	object, _ := params["new"].(map[string]any)
	_, hasStatus := object["status"]
	return frame["method"] == "user" && len(params) == 1 && len(object) == 2 && hasStatus
}

// call sends a request and returns its reply frame, which must come next.
func (c *testClient) call(t *testing.T, method, id string, params map[string]any) map[string]any {
	t.Helper()
	c.write(t, map[string]any{"method": method, "id": id, "params": params})
	frame := c.read(t)
	if frame["id"] != id {
		t.Fatalf("%s reply = %#v, want id %q", method, frame, id)
	}
	return frame
}

// request sends a request and returns the notifications that precede its
// reply, then its result.
func (c *testClient) request(t *testing.T, method, id string, params map[string]any) ([]map[string]any, map[string]any) {
	t.Helper()
	c.write(t, map[string]any{"method": method, "id": id, "params": params})
	var before []map[string]any
	for {
		frame := c.read(t)
		if frame["id"] == nil {
			before = append(before, frame)
			continue
		}
		result, ok := frame["result"].(map[string]any)
		if frame["id"] != id || !ok {
			t.Fatalf("%s reply = %#v after %#v, want a result for %q", method, frame, before, id)
		}
		return before, result
	}
}

func (c *testClient) result(t *testing.T, method, id string, params map[string]any) map[string]any {
	t.Helper()
	frame := c.call(t, method, id, params)
	result, ok := frame["result"].(map[string]any)
	if !ok {
		t.Fatalf("%s failed: %#v", method, frame)
	}
	return result
}

func (c *testClient) expectError(t *testing.T, method, id string, params map[string]any, code int) {
	t.Helper()
	frame := c.call(t, method, id, params)
	failure, ok := frame["error"].(map[string]any)
	if !ok || failure["code"] != float64(code) {
		t.Fatalf("%s %s: %#v, want error %d", method, id, frame, code)
	}
}

// notification reads the next frame and requires it to be the named notification.
func (c *testClient) notification(t *testing.T, method string) map[string]any {
	t.Helper()
	frame := c.read(t)
	return notificationParams(t, frame, method)
}

// notificationParams requires a frame to be the named notification and
// returns its params.
func notificationParams(t *testing.T, frame map[string]any, method string) map[string]any {
	t.Helper()
	params, ok := frame["params"].(map[string]any)
	if frame["method"] != method || !ok || frame["id"] != nil {
		t.Fatalf("frame = %#v, want %s notification", frame, method)
	}
	return params
}

// methods names each frame's method, or "reply" for a reply.
func methods(frames []map[string]any) []string {
	names := make([]string, len(frames))
	for i, frame := range frames {
		names[i], _ = frame["method"].(string)
		if names[i] == "" {
			names[i] = "reply"
		}
	}
	return names
}

// save sends a message request and returns the result ID and the sender's
// copy of the broadcast snapshot, which precedes the result (§1).
func save(t *testing.T, c *testClient, requestID string, params map[string]any) (string, map[string]any) {
	t.Helper()
	if _, ok := params["room_id"]; !ok {
		params["room_id"] = "general"
	}
	before, result := c.request(t, "message", requestID, params)
	id, ok := result["message_id"].(string)
	if !ok || len(before) != 1 {
		t.Fatalf("message result %#v after %#v, want one broadcast first", result, before)
	}
	snapshot := notificationParams(t, before[0], "message")
	if snapshot["message_id"] != id || snapshot["room_id"] != params["room_id"] {
		t.Fatalf("result and snapshot disagree: %#v vs %#v", result, snapshot)
	}
	if _, wrapped := snapshot["message"]; wrapped {
		t.Fatalf("snapshot is not flat: %#v", snapshot)
	}
	if len(result) != 1 {
		t.Fatalf("message result: %#v", result)
	}
	return id, snapshot
}

// saveRoom sends room_set and returns the room ID and the record of the
// room_update that precedes the result: joined for a new room, followed by
// the creator's membership, or updated for an edit. The joined record's
// members are checked and removed, so it compares with updated records.
func saveRoom(t *testing.T, c *testClient, requestID string, params map[string]any) (string, map[string]any) {
	t.Helper()
	before, result := c.request(t, "room_set", requestID, params)
	_, editing := params["room_id"]
	if !reflect.DeepEqual(methods(before), []string{"room_update"}) {
		t.Fatalf("room_set frames = %#v, want one room_update", before)
	}
	var record map[string]any
	if editing {
		record = updateRecord(t, before[0], "updated")
	} else {
		// The creator receives the room with its members and the creator's
		// membership in one room_update (§4.3.4).
		record = joinedRecord(t, before[0], c.userID)
		membership := membershipOf(t, before[0])
		checkMembership(t, membership, record["room_id"].(string), c.userID, true)
		if membership["log_id"] != record["latest_log_id"] || parseID(t, membership["log_id"]) <= parseID(t, record["log_id"]) {
			t.Fatalf("creator membership %#v does not follow the room record %#v", membership, record)
		}
		if members := memberIDs(record); !reflect.DeepEqual(members, []string{c.userID}) {
			t.Fatalf("new room members: %v", members)
		}
		delete(record, "members")
	}
	if len(result) != 1 || result["room_id"] != record["room_id"] {
		t.Fatalf("room_set result %#v disagrees with %#v", result, record)
	}
	return record["room_id"].(string), record
}

// joinRoom sends room_join and returns the room record of the room_update
// joined that follows the membership and precedes the result. The record
// keeps its members.
func joinRoom(t *testing.T, c *testClient, roomID string) map[string]any {
	t.Helper()
	before, result := c.request(t, "room_join", c.nextID("join"), map[string]any{"room_id": roomID})
	if !reflect.DeepEqual(methods(before), []string{"room_update"}) || len(result) != 0 {
		t.Fatalf("room_join %s: %#v then %#v", roomID, before, result)
	}
	membership := membershipOf(t, before[0])
	checkMembership(t, membership, roomID, c.userID, true)
	record := joinedRecord(t, before[0], c.userID)
	if record["room_id"] != roomID || record["latest_log_id"] != membership["log_id"] {
		t.Fatalf("room_join %s record: %#v", roomID, record)
	}
	return record
}

// leaveRoom sends room_leave and checks the membership and room_update left
// that precede its result.
func leaveRoom(t *testing.T, c *testClient, roomID string) {
	t.Helper()
	before, result := c.request(t, "room_leave", c.nextID("leave"), map[string]any{"room_id": roomID})
	if !reflect.DeepEqual(methods(before), []string{"room_update"}) || len(result) != 0 {
		t.Fatalf("room_leave %s: %#v then %#v", roomID, before, result)
	}
	checkLeft(t, before[0], roomID, c.userID)
}

// roomUpdated reads a room_update notification and returns its one record
// in field.
func roomUpdated(t *testing.T, c *testClient, field string) map[string]any {
	t.Helper()
	return updateRecord(t, c.read(t), field)
}

// updateRecord requires a room_update carrying one record in field alone.
func updateRecord(t *testing.T, frame map[string]any, field string) map[string]any {
	t.Helper()
	update := notificationParams(t, frame, "room_update")
	records, ok := update[field].([]any)
	if !ok || len(records) != 1 || len(update) != 1 {
		t.Fatalf("room_update = %#v, want one %s record", update, field)
	}
	return records[0].(map[string]any)
}

// joinedRecord requires a room_update joined carrying one record with its
// members, and users holding each member's current object once. A non-empty
// member must be among them.
func joinedRecord(t *testing.T, frame map[string]any, member string) map[string]any {
	t.Helper()
	update := notificationParams(t, frame, "room_update")
	records, ok := update["joined"].([]any)
	_, withMembership := update["memberships"]
	if !ok || len(records) != 1 || (len(update) != 2 && !(withMembership && len(update) == 3)) {
		t.Fatalf("room_update = %#v, want one joined record and users", update)
	}
	record := records[0].(map[string]any)
	members := memberIDs(record)
	var users []string
	for _, user := range update["users"].([]any) {
		users = append(users, user.(map[string]any)["user_id"].(string))
	}
	if !reflect.DeepEqual(members, users) || (member != "" && !slices.Contains(members, member)) {
		t.Fatalf("joined members %v with users %v, want %q among them", members, users, member)
	}
	return record
}

// memberIDs lists the user_ids of a room record's members.
func memberIDs(entry map[string]any) []string {
	ids := []string{}
	for _, member := range entry["members"].([]any) {
		ids = append(ids, member.(map[string]any)["user_id"].(string))
	}
	return ids
}

// react sets reactions and returns the broadcast that precedes the result.
func react(t *testing.T, c *testClient, requestID, messageID string, emojis ...string) map[string]any {
	t.Helper()
	list := make([]any, len(emojis))
	for i, emoji := range emojis {
		list[i] = emoji
	}
	before, result := c.request(t, "reactions", requestID, map[string]any{"message_id": messageID, "emojis": list})
	if len(result) != 0 || len(before) != 1 {
		t.Fatalf("reactions result %#v after %#v, want one broadcast first", result, before)
	}
	return notificationParams(t, before[0], "reactions")
}

func historyPage(t *testing.T, c *testClient, roomID string, params map[string]any) map[string]any {
	t.Helper()
	params["room_id"] = roomID
	return c.result(t, "history", c.nextID("history"), params)
}

// logIDs lists the log_ids of a history array, empty when it is omitted.
func logIDs(t *testing.T, page map[string]any, key string) []string {
	t.Helper()
	raw, present := page[key]
	if !present {
		return []string{}
	}
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		t.Fatalf("history %s is not a non-empty array: %#v", key, page)
	}
	ids := make([]string, len(list))
	for i, value := range list {
		ids[i] = value.(map[string]any)["log_id"].(string)
	}
	return ids
}

// records returns a history array, empty when it is omitted.
func records(t *testing.T, page map[string]any, key string) []any {
	t.Helper()
	logIDs(t, page, key)
	list, _ := page[key].([]any)
	return list
}

func parseID(t *testing.T, value any) int64 {
	t.Helper()
	text, ok := value.(string)
	if !ok {
		t.Fatalf("log id %#v is not a string", value)
	}
	id, err := strconv.ParseInt(text, 10, 64)
	if err != nil || id <= 0 {
		t.Fatalf("log id %q is not a positive integer", text)
	}
	return id
}

// httpDo sends one HTTP request and returns the status, headers, and body.
func httpDo(t *testing.T, method, url string, body io.Reader, contentType string) (int, http.Header, []byte) {
	t.Helper()
	request, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, response.Header, data
}

func testPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 3, 2))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func embedsOf(t *testing.T, snapshot map[string]any) []map[string]any {
	t.Helper()
	body, _ := snapshot["body"].(map[string]any)
	list, _ := body["embeds"].([]any)
	embeds := make([]map[string]any, len(list))
	for i, value := range list {
		embeds[i] = value.(map[string]any)
	}
	return embeds
}

// postEmbeds posts a message with new upload or stream embeds and returns
// its result, which must follow the sender's copy of the pending snapshot
// (§1); the snapshot is added to the result as "snapshot".
func postEmbeds(t *testing.T, c *testClient, id string, params map[string]any) map[string]any {
	t.Helper()
	before, result := c.request(t, "message", id, params)
	if len(before) != 1 || len(result["embeds"].([]any)) == 0 {
		t.Fatalf("message with embeds: %#v then %#v", before, result)
	}
	result["snapshot"] = notificationParams(t, before[0], "message")
	return result
}
