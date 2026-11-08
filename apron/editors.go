package apron

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"maps"
	"slices"
)

// ext:editors (apron's EXTENSIONS.md) lets a message's sender share the
// editing of its text with the users it mentions. The sender sets
// ext.editors {"body.text": "mentioned"}; each user in the current
// body.mentions may then save body.text, and only that. The server keeps
// every other field of the current snapshot, ext included, and marks such a
// snapshot with ext.editors.edited_by.

const (
	editorsKey = "editors"
	// sharedText is the one field a grant may share, and mentionedEditors
	// the one value that shares it. Any other key or value shares nothing.
	sharedText       = "body.text"
	mentionedEditors = "mentioned"
	editedBy         = "edited_by"
)

// sharesText reports whether userID, who is not its sender, may save the
// body.text of the message whose current snapshot is snapshot.
func sharesText(snapshot map[string]any, userID string) bool {
	if snapshot["deleted"] == true {
		return false
	}
	raw, ok := extOf(snapshot["ext"])[editorsKey]
	if !ok {
		return false
	}
	var editors map[string]jsontext.Value
	if json.Unmarshal(raw, &editors) != nil {
		return false
	}
	var grant string
	if json.Unmarshal(editors[sharedText], &grant) != nil || grant != mentionedEditors {
		return false
	}
	body, _ := snapshot["body"].(map[string]any)
	return slices.Contains(mentions(body), userID)
}

// sharedTextBody is the body of a save of only body.text: the current
// snapshot's body, with the text that the request carries, or none when it
// carries none.
func sharedTextBody(current map[string]any, request map[string]any) map[string]any {
	body, _ := current["body"].(map[string]any)
	body = maps.Clone(body)
	if body == nil {
		body = make(map[string]any)
	}
	if text, ok := request["text"]; ok {
		body["text"] = text
	} else {
		delete(body, "text")
	}
	return body
}

// withEditedBy returns ext with ext.editors.edited_by set to userID, or
// removed when userID is "". The server sets it on a save by a user other
// than the sender, removes it on the sender's saves, and ignores it in
// requests. It leaves an ext without an editors object as it is.
func withEditedBy(ext extObject, userID string) extObject {
	raw, ok := ext[editorsKey]
	if !ok {
		return ext
	}
	var editors map[string]jsontext.Value
	if json.Unmarshal(raw, &editors) != nil || editors == nil {
		return ext
	}
	if _, had := editors[editedBy]; !had && userID == "" {
		return ext
	}
	delete(editors, editedBy)
	if userID != "" {
		editors[editedBy], _ = json.Marshal(userID)
	}
	ext = maps.Clone(ext)
	if len(editors) == 0 {
		delete(ext, editorsKey)
		if len(ext) == 0 {
			return nil
		}
		return ext
	}
	encoded, err := json.Marshal(editors, json.Deterministic(true))
	if err != nil {
		return ext
	}
	ext[editorsKey] = encoded
	return ext
}
