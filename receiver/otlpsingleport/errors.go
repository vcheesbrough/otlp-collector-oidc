package otlpsingleport

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/collector/consumer/consumererror"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/vcheesbrough/otlp-collector-oidc/extension/oidcclientauth"
)

// statusFromConsumerError maps a consumer error to the gRPC status the stock
// OTLP receiver answers with. A status carried by the error keeps its code (a
// permanent upstream InvalidArgument stays InvalidArgument); a permanent error
// with none is this collector's own fault (Internal); anything else is
// retryable (Unavailable).
func statusFromConsumerError(err error) error {
	s, ok := status.FromError(err)
	if !ok {
		code := codes.Unavailable
		if consumererror.IsPermanent(err) {
			code = codes.Internal
		}
		s = status.New(code, err.Error())
	}
	return s.Err()
}

// httpStatusFromStatus is the OTLP specification's gRPC → HTTP mapping, as
// the stock receiver applies it.
func httpStatusFromStatus(s *status.Status) int {
	switch s.Code() {
	case codes.Canceled, codes.DeadlineExceeded, codes.Aborted, codes.OutOfRange, codes.Unavailable, codes.DataLoss:
		return http.StatusServiceUnavailable
	case codes.ResourceExhausted:
		return http.StatusTooManyRequests
	case codes.InvalidArgument:
		return http.StatusBadRequest
	case codes.Unauthenticated:
		return http.StatusUnauthorized
	case codes.PermissionDenied:
		return http.StatusForbidden
	case codes.Unimplemented:
		return http.StatusNotFound
	case codes.OK, codes.Unknown, codes.NotFound, codes.AlreadyExists, codes.FailedPrecondition, codes.Internal:
		return http.StatusInternalServerError
	default:
		return http.StatusInternalServerError
	}
}

// statusFromHTTP wraps an HTTP status and message in the rpc.Status an
// OTLP/HTTP error body carries.
func statusFromHTTP(msg string, httpStatus int) *status.Status {
	var c codes.Code
	switch httpStatus {
	case http.StatusBadRequest:
		c = codes.InvalidArgument
	case http.StatusUnauthorized:
		c = codes.Unauthenticated
	case http.StatusForbidden:
		c = codes.PermissionDenied
	case http.StatusNotFound:
		c = codes.Unimplemented
	case http.StatusTooManyRequests, http.StatusRequestEntityTooLarge:
		c = codes.ResourceExhausted
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		c = codes.Unavailable
	default:
		c = codes.Unknown
	}
	return status.New(c, msg)
}

// notReadyRetryAfter is how long a client is asked to wait while the
// authenticator's provider has not loaded.
const notReadyRetryAfter = 5 * time.Second

// newErrorHandler returns the listener's error handler. confighttp calls it
// for failures before the request reaches the dispatcher: an authenticator
// refusal, always handed over as 401 with the error's text, and a body the
// decompressor rejects. The authenticator's not-ready error becomes 503 with
// Retry-After; every other status is written unchanged. Authenticator
// refusals are counted by the authenticator, the rest here.
func newErrorHandler(refused *refusals) func(w http.ResponseWriter, r *http.Request, msg string, httpStatus int) {
	return func(w http.ResponseWriter, r *http.Request, msg string, httpStatus int) {
		switch {
		// confighttp hands the handler the error's text, not the error, so
		// the not-ready refusal is told apart by its text; the extension
		// exports it as ErrNotReady so the two cannot drift.
		case httpStatus == http.StatusUnauthorized && msg == oidcclientauth.ErrNotReady.Error():
			writeNotReady(w, r, msg)
		case httpStatus == http.StatusUnauthorized:
			writeUnauthenticated(w, r, msg)
		default:
			t := transportHTTP
			if isGRPC(r) {
				t = transportGRPC
			}
			refused.count(r.Context(), t, refusalDecompress)
			writeHandedStatus(w, r, msg, httpStatus)
		}
	}
}

// writeUnauthenticated answers an authenticator refusal: 401 with the
// refusal's text over HTTP, Unauthenticated with the same text over gRPC.
func writeUnauthenticated(w http.ResponseWriter, r *http.Request, msg string) {
	if isGRPC(r) {
		writeGRPCStatus(w, status.New(codes.Unauthenticated, msg))
		return
	}
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeHandedStatus(w, r, msg, http.StatusUnauthorized)
}

// writeNotReady answers the authenticator's not-ready error as retryable,
// with a retry delay: 503 with Retry-After over HTTP, whose OTLP body carries
// it as RetryInfo, and Unavailable with the RetryInfo over gRPC.
func writeNotReady(w http.ResponseWriter, r *http.Request, msg string) {
	st := status.New(codes.Unavailable, msg)
	if withDelay, err := st.WithDetails(&errdetails.RetryInfo{RetryDelay: durationpb.New(notReadyRetryAfter)}); err == nil {
		st = withDelay
	}
	if isGRPC(r) {
		writeGRPCStatus(w, st)
		return
	}
	if enc, ok := lookupEncoding(r.Header.Get("Content-Type")); ok {
		writeStatus(w, enc, http.StatusServiceUnavailable, st)
		return
	}
	w.Header().Set("Retry-After", strconv.FormatInt(int64(notReadyRetryAfter/time.Second), 10))
	http.Error(w, msg, http.StatusServiceUnavailable)
}

// writeGRPCStatus answers a gRPC request with st as a trailers-only
// response, so a gRPC client reads the code and the text themselves rather
// than an HTTP status it can only guess a code from.
func writeGRPCStatus(w http.ResponseWriter, st *status.Status) {
	h := w.Header()
	h.Set("Content-Type", "application/grpc")
	h.Set("Grpc-Status", strconv.Itoa(int(st.Code())))
	h.Set("Grpc-Message", encodeGRPCMessage(st.Message()))
	if len(st.Details()) > 0 {
		if b, err := proto.Marshal(st.Proto()); err == nil {
			h.Set("Grpc-Status-Details-Bin", base64.RawStdEncoding.EncodeToString(b))
		}
	}
	w.WriteHeader(http.StatusOK)
}

// encodeGRPCMessage percent-encodes msg as the gRPC wire protocol requires
// of grpc-message: every byte outside printable ASCII, and '%'.
func encodeGRPCMessage(msg string) string {
	var b strings.Builder
	for i := range len(msg) {
		c := msg[i]
		if c < ' ' || c > '~' || c == '%' {
			fmt.Fprintf(&b, "%%%02X", c)
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// writeHandedStatus writes a status the listener hands over unchanged: as an
// OTLP rpc.Status in the request's encoding when that is an OTLP encoding, as
// plain text otherwise, so a gRPC client reads the HTTP status itself.
func writeHandedStatus(w http.ResponseWriter, r *http.Request, msg string, httpStatus int) {
	if enc, ok := lookupEncoding(r.Header.Get("Content-Type")); ok {
		writeStatus(w, enc, httpStatus, statusFromHTTP(msg, httpStatus))
		return
	}
	http.Error(w, msg, httpStatus)
}

// writeStatus writes st as the OTLP/HTTP error body, with Retry-After when a
// retryable answer carries the upstream's retry delay.
func writeStatus(w http.ResponseWriter, enc encoding, httpStatus int, st *status.Status) {
	if httpStatus == http.StatusTooManyRequests || httpStatus == http.StatusServiceUnavailable {
		if d, ok := retryDelay(st); ok {
			w.Header().Set("Retry-After", strconv.FormatInt(int64(d/time.Second), 10))
		}
	}
	body, err := enc.marshalStatus(st.Proto())
	if err != nil {
		http.Error(w, "failed to marshal error status", http.StatusInternalServerError)
		return
	}
	writeBody(w, enc.mediaType, httpStatus, body)
}

func retryDelay(st *status.Status) (time.Duration, bool) {
	for _, d := range st.Details() {
		if ri, ok := d.(*errdetails.RetryInfo); ok && ri.GetRetryDelay() != nil {
			return ri.GetRetryDelay().AsDuration(), true
		}
	}
	return 0, false
}
