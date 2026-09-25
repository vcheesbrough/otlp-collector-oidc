package oidcclientauth

import "strings"

// bearerToken is the token of the request's one Authorization: Bearer
// header. Nothing else is read — no cookie, no query parameter — and a
// request with several Authorization values has no token, since which one
// the client meant is not ours to guess.
func bearerToken(sources map[string][]string) (string, bool) {
	var values []string
	for k, v := range sources {
		// Headers arrive canonicalised over HTTP and lower-case as gRPC
		// metadata; match either.
		if strings.EqualFold(k, "authorization") {
			values = append(values, v...)
		}
	}
	if len(values) != 1 {
		return "", false
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}
