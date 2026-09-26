#!/bin/sh
# Smoke-tests a built image the way the README's quick start runs it: nothing
# mounted, only the required variables set. Checks the OCI version label, the
# version subcommand and target_info agree, that HEALTHCHECK passes with no
# upstream and no reachable identity provider, that the configuration was
# rendered into /tmp, and that an export without a token is refused as
# 'no token' over the embedded certificate (curl -k). Then mounts a pair
# generated here and checks curl verifies it (curl --cacert). The embedded
# run uses the hardening the compose example does (uid 10001, read-only root
# with a /tmp tmpfs, no capabilities, no-new-privileges), and the image must
# carry its licence and the bundled modules' notices, Go's own included.
set -eu

image=$1
version=$2

fail() {
	echo "smoke: $*" >&2
	exit 1
}

label=$(docker inspect -f '{{ index .Config.Labels "org.opencontainers.image.version" }}' "$image")
[ "$label" = "$version" ] || fail "OCI version label is '$label', want '$version'"
# The product's licence and the bundled modules' notices ship in the image.
docker run --rm --entrypoint /bin/sh "$image" -c '
	test -s /usr/share/licenses/otlp-collector-oidc/LICENSE &&
	test -s /usr/share/licenses/otlp-collector-oidc/third-party/MODULES &&
	test -s /usr/share/licenses/otlp-collector-oidc/third-party/go.opentelemetry.io/collector/otelcol/LICENSE &&
	test -s /usr/share/licenses/otlp-collector-oidc/third-party/go/LICENSE' ||
	fail "licence or third-party notices missing from the image"
reported=$(docker run --rm "$image" version)
[ "$reported" = "$version" ] || fail "'version' reports '$reported', want '$version'"

# The embedded pair under the hardening examples/compose-behind-traefik uses:
# a read-only root with /tmp for the rendered configuration, no capabilities,
# no privilege gain, the image's own non-root user.
cid=$(docker run -d -p 127.0.0.1:4318:4318 -p 127.0.0.1:8888:8888 \
	--user 10001:10001 --read-only --tmpfs /tmp --cap-drop ALL --security-opt no-new-privileges:true \
	-e OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4317 \
	-e OIDC_ISSUER_URL=https://issuer.invalid -e OIDC_AUDIENCE=smoke -e ALLOWED_SERVICE_NAMES='.*' "$image")
trap 'docker rm -f "$cid" >/dev/null' EXIT

await_healthy() {
	i=0
	until [ "$(docker inspect -f '{{ .State.Health.Status }}' "$1")" = healthy ]; do
		i=$((i + 1))
		[ "$i" -le 60 ] || { docker logs "$1" >&2; fail "HEALTHCHECK never passed"; }
		sleep 1
	done
}

# export_without_token URL CURL_TLS_ARGS... prints the status and the body.
export_without_token() {
	url=$1
	shift
	answer=$(curl -s "$@" -w '\n%{http_code}' "$url/v1/traces" \
		-H 'Content-Type: application/json' \
		-d '{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"5b8efff798038103d269b633813fc60c","spanId":"eee19b7ec3c1b174","name":"smoke","startTimeUnixNano":"1","endTimeUnixNano":"2"}]}]}]}') ||
		fail "curl $* failed"
	code=$(printf '%s\n' "$answer" | tail -n 1)
	# protojson randomly spaces its separators, so drop the optional space after ',' and ':'.
	body=$(printf '%s\n' "$answer" | sed '$d' | sed -E 's/([,:]) /\1/g')
	[ "$code" = 401 ] || fail "export without a token answered $code, want 401"
	[ "$body" = '{"code":16,"message":"no token"}' ] || fail "401 body is '$body', want the 'no token' status"
}

await_healthy "$cid"
docker exec "$cid" grep -q otlpsingleport /tmp/otlp-collector-oidc.yaml ||
	fail "the rendered configuration is not in /tmp/otlp-collector-oidc.yaml"
export_without_token https://localhost:4318 -k || { docker logs "$cid" >&2; exit 1; }

curl -s http://127.0.0.1:8888/metrics | grep -q "^target_info{.*service_version=\"$version\"" ||
	fail "target_info does not carry service_version=$version"

# A mounted pair, generated for this run and never kept: the non-root user
# reads it, and curl verifies the chain against it.
tls=$(mktemp -d)
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 \
	-subj "/CN=smoke" -addext "subjectAltName=DNS:localhost" \
	-keyout "$tls/key.pem" -out "$tls/cert.pem" 2>/dev/null
chmod 0755 "$tls" && chmod 0644 "$tls/cert.pem" "$tls/key.pem"
mounted=$(docker run -d -p 127.0.0.1:4319:4318 -v "$tls:/tls:ro" \
	-e TLS_CERT_FILE=/tls/cert.pem -e TLS_KEY_FILE=/tls/key.pem \
	-e OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4317 \
	-e OIDC_ISSUER_URL=https://issuer.invalid -e OIDC_AUDIENCE=smoke -e ALLOWED_SERVICE_NAMES='.*' "$image")
trap 'docker rm -f "$cid" "$mounted" >/dev/null; rm -rf "$tls"' EXIT
await_healthy "$mounted"
export_without_token https://localhost:4319 --cacert "$tls/cert.pem" || { docker logs "$mounted" >&2; exit 1; }

echo "smoke: $image $version healthy, configuration rendered, tokenless export refused over the embedded and a mounted pair, versions agree"
