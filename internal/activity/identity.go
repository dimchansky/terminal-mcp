package activity

import "encoding/json"

// clientInfoKey is the MCP per-request client metadata key recorded in
// request_start.call_meta.
const clientInfoKey = "io.modelcontextprotocol/clientInfo"

// conversationSources lists the documented per-conversation metadata fields
// that may supply a conversation hint. Supporting a new provider means adding
// its field here; the Identity shape does not change.
var conversationSources = []string{"openai/session"}

// Identity is the normalized, read-only projection of client-supplied
// caller metadata (docs/DASHBOARD_DESIGN.md section 3; the API exposes the
// first four fields as clientName, conversationId, identitySource and
// identityVerified). Every value is client-claimed and unauthenticated.
// ConversationID is the exact complete value recorded in
// request_start.call_meta, never hashed, shortened or invented; grouping uses
// the pair (IdentitySource, ConversationID). Nil means the value is absent.
//
// MetadataInvalid reports that a recognized identity field was present but
// could not be used: duplicated, null or of another non-string type, an empty
// conversation hint, two recognized providers at once, or a string that
// cannot be represented exactly. Only the affected value is nil; a call with an
// unusable conversation hint is unattributed rather than grouped under a
// guessed identity. Conflicts between journal copies are reported separately.
type Identity struct {
	ClientName       *string
	ConversationID   *string
	IdentitySource   *string
	IdentityVerified bool
	MetadataInvalid  bool
}

// ConversationKey groups actions into a lane. It deliberately excludes the
// server ID so a conversation can span server restarts. The zero value means
// unattributed; unattributed calls are never grouped into a conversation.
type ConversationKey struct {
	IdentitySource string
	ConversationID string
}

// Key returns the lane key and whether the identity names a conversation.
func (id Identity) Key() (ConversationKey, bool) {
	if id.ConversationID == nil || id.IdentitySource == nil {
		return ConversationKey{}, false
	}
	return ConversationKey{IdentitySource: *id.IdentitySource, ConversationID: *id.ConversationID}, true
}

// identityFromCallMeta normalizes recorded call_meta without modifying it.
// identityVerified is always false: no server-side verification exists.
func identityFromCallMeta(meta json.RawMessage) Identity {
	var id Identity
	if len(meta) == 0 {
		return id
	}
	fields, duplicates, err := objectFields(meta, func(key string) bool {
		return key == clientInfoKey || isConversationSource(key)
	})
	if err != nil {
		id.MetadataInvalid = true
		return id
	}
	duplicated := make(map[string]bool, len(duplicates))
	for _, key := range duplicates {
		duplicated[key] = true
		id.MetadataInvalid = true
	}

	var present []string
	for _, source := range conversationSources {
		if _, ok := fields[source]; ok {
			present = append(present, source)
		}
	}
	switch {
	case len(present) > 1:
		id.MetadataInvalid = true // Two providers claim the call; choose neither.
	case len(present) == 1 && !duplicated[present[0]]:
		value, ok := exactString(fields[present[0]])
		if ok && value != "" {
			source := present[0]
			id.ConversationID, id.IdentitySource = &value, &source
		} else {
			id.MetadataInvalid = true
		}
	}

	if raw, ok := fields[clientInfoKey]; ok && !duplicated[clientInfoKey] {
		name, ambiguous := clientName(raw)
		id.ClientName = name
		id.MetadataInvalid = id.MetadataInvalid || ambiguous
	}
	return id
}

func isConversationSource(key string) bool {
	for _, source := range conversationSources {
		if key == source {
			return true
		}
	}
	return false
}

// clientName extracts clientInfo.name exactly. An empty name is preserved as
// recorded; a missing name is nil without ambiguity.
func clientName(raw json.RawMessage) (*string, bool) {
	fields, duplicates, err := objectFields(raw, func(key string) bool { return key == "name" })
	if err != nil || len(duplicates) > 0 {
		return nil, true
	}
	value, ok := fields["name"]
	if !ok {
		return nil, false
	}
	name, exact := exactString(value)
	if !exact {
		return nil, true
	}
	return &name, false
}
