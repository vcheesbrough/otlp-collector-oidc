package otlpsingleport

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"

	spb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// otlpMessage is what pdata's OTLP export requests and responses share.
type otlpMessage interface {
	MarshalProto() ([]byte, error)
	UnmarshalProto(data []byte) error
	MarshalJSON() ([]byte, error)
	UnmarshalJSON(data []byte) error
}

// encoding is one OTLP/HTTP body encoding.
type encoding struct {
	mediaType     string
	unmarshal     func(m otlpMessage, body []byte) error
	marshal       func(m otlpMessage) ([]byte, error)
	marshalStatus func(s *spb.Status) ([]byte, error)
}

// lookupEncoding finds the encoding for a Content-Type. The table is the only
// place an encoding is named: a new one is a new entry.
func lookupEncoding(contentType string) (encoding, bool) {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return encoding{}, false
	}
	table := [...]encoding{
		{
			mediaType:     "application/x-protobuf",
			unmarshal:     func(m otlpMessage, b []byte) error { return m.UnmarshalProto(b) },
			marshal:       func(m otlpMessage) ([]byte, error) { return m.MarshalProto() },
			marshalStatus: func(s *spb.Status) ([]byte, error) { return proto.Marshal(s) },
		},
		{
			mediaType:     "application/json",
			unmarshal:     func(m otlpMessage, b []byte) error { return m.UnmarshalJSON(b) },
			marshal:       func(m otlpMessage) ([]byte, error) { return m.MarshalJSON() },
			marshalStatus: func(s *spb.Status) ([]byte, error) { return protojson.Marshal(s) },
		},
	}
	for _, e := range table {
		if e.mediaType == mediaType {
			return e, true
		}
	}
	return encoding{}, false
}

// exportHandler serves one signal's OTLP/HTTP endpoint. It is parametrised by
// the signal's request and response types, so the three signals share one
// implementation.
func exportHandler[Req, Resp otlpMessage](newRequest func() Req, export func(context.Context, Req) (Resp, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "405 method not allowed, supported: [POST]", http.StatusMethodNotAllowed)
			return
		}
		enc, ok := lookupEncoding(r.Header.Get("Content-Type"))
		if !ok {
			http.Error(w, "415 unsupported media type, supported: [application/json, application/x-protobuf]", http.StatusUnsupportedMediaType)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			code := http.StatusBadRequest
			if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
				code = http.StatusRequestEntityTooLarge
			}
			writeStatus(w, enc, code, statusFromHTTP(err.Error(), code))
			return
		}

		req := newRequest()
		if err = enc.unmarshal(req, body); err != nil {
			writeStatus(w, enc, http.StatusBadRequest, statusFromHTTP(err.Error(), http.StatusBadRequest))
			return
		}

		resp, err := export(r.Context(), req)
		if err != nil {
			st := status.Convert(err)
			writeStatus(w, enc, httpStatusFromStatus(st), st)
			return
		}

		out, err := enc.marshal(resp)
		if err != nil {
			writeStatus(w, enc, http.StatusInternalServerError, statusFromHTTP(err.Error(), http.StatusInternalServerError))
			return
		}
		writeBody(w, enc.mediaType, http.StatusOK, out)
	}
}

func writeBody(w http.ResponseWriter, mediaType string, httpStatus int, body []byte) {
	w.Header().Set("Content-Type", mediaType)
	w.WriteHeader(httpStatus)
	// The client has gone if this fails; there is no one left to tell.
	_, _ = w.Write(body)
}
