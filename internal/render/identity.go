package render

// The traces and logs pipelines stamp identity onto every span and record
// (DESIGN §5). An attributes processor copies each mapped claim from the
// authenticated context, where the authenticator put it (auth.<attribute>);
// a resource processor stamps the deployment's static attributes. Identity
// is materialised before batch, so the batcher needs no metadata_keys and
// two tokens batched together keep their own.

// attributeAction is one action of the attributes processor.
type attributeAction struct {
	Key         string
	Action      string
	FromContext string
}

// identityActions is, for each mapped claim, a delete of whatever the client
// sent under its attribute and an upsert from the context. The processor
// skips an upsert whose context key is absent (collector v0.161.0), so a
// claim the token lacks leaves the attribute absent, not forged.
func identityActions(claims KeyValueList) []attributeAction {
	out := make([]attributeAction, 0, 2*len(claims))
	for _, kv := range claims {
		out = append(out,
			attributeAction{Key: kv.Value, Action: "delete"},
			attributeAction{Key: kv.Value, Action: "upsert", FromContext: "auth." + kv.Value},
		)
	}
	return out
}

// resourceAction is one action of the resource processor.
type resourceAction struct {
	Key    string
	Action string
	Value  string
}

// resourceActions deletes every claim target from the resource, where a
// client could otherwise forge identity a backend reads like the span's, and
// upserts each of the deployment's attributes over the client's.
func resourceActions(claims, clientResource KeyValueList) []resourceAction {
	out := make([]resourceAction, 0, len(claims)+len(clientResource))
	for _, kv := range claims {
		out = append(out, resourceAction{Key: kv.Value, Action: "delete"})
	}
	for _, kv := range clientResource {
		out = append(out, resourceAction{Key: kv.Key, Action: "upsert", Value: kv.Value})
	}
	return out
}

// identityProcessors is the processor chain of every client traces and logs
// pipeline, from one list so the two cannot diverge: the bounds first, so
// what is dropped costs nothing further, then identity, then batch.
func identityProcessors() []string {
	return []string{"memory_limiter", "filter/bounds", "transform/bounds", "attributes/identity", "resource/identity", "batch"}
}
