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

// identityProcessors is the processor chain of every client traces and logs
// pipeline, from one list so the two cannot diverge.
func identityProcessors(clientResource KeyValueList) []string {
	chain := []string{"memory_limiter", "attributes/identity"}
	if len(clientResource) > 0 {
		chain = append(chain, "resource/deployment")
	}
	return append(chain, "batch")
}
