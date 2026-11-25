package apron

// HistoryParams are the params of history (§4.3): an empty RoomID is every
// room the connection receives; After and Before are inclusive log_id
// bounds, nil when left out, which also sets the direction: from the oldest
// with After, and from the newest without; Limit is positive, 0 for the
// backend's default. They encode as a client sends them.
type HistoryParams struct {
	RoomID string `json:"room_id,omitempty"`
	After  *LogID `json:"after,omitempty"`
	Before *LogID `json:"before,omitempty"`
	Limit  int    `json:"limit,omitzero"`
}

// HistoryResult is the result of history (§4.3). Its rooms are logged
// records, without the delivery fields of a RoomView (§4.3.3).
type HistoryResult struct {
	Rooms       []Room       `json:"rooms,omitempty"`
	Messages    []Message    `json:"messages,omitempty"`
	Reactions   []Reactions  `json:"reactions,omitempty"`
	Memberships []Membership `json:"memberships,omitempty"`
	FirstLogID  LogID        `json:"first_log_id,omitzero"`
	LastLogID   LogID        `json:"last_log_id,omitzero"`
	More        bool         `json:"more"`
	LatestLogID LogID        `json:"latest_log_id,omitzero"`
	// HistoryLogID is the inclusive lower bound of what is retrievable, or
	// 0 for nothing, which is sent as null: every result has it (§4.3.3).
	HistoryLogID LogID `json:"history_log_id"`
}

// Record is one logged record, for Page: exactly one of Room, Message,
// Membership, and Reactions is set, and LogID is its log_id.
type Record struct {
	LogID      LogID
	Room       *Room
	Message    *Message
	Membership *Membership
	Reactions  *Reactions
}
