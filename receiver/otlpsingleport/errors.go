package otlpsingleport

import (
	"net/http"
	"strconv"
	"time"

	"go.opentelemetry.io/collector/consumer/consumererror"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

// writeHandedStatus is the listener's error handler. confighttp calls it for
// failures before the request reaches the dispatcher — a body the
// decompressor rejects, and later an authenticator refusal — and it writes
// the status it is handed unchanged: as an OTLP rpc.Status in the request's
// encoding when that is an OTLP encoding, as plain text otherwise, so a gRPC
// client reads the HTTP status itself.
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
