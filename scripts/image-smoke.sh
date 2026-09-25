#!/bin/sh
# Smoke-tests a built image the way the README's quick start runs it: nothing
# mounted, only the required variables set. Checks the OCI version label, the
# version subcommand and target_info agree, that HEALTHCHECK passes with no
# upstream and no reachable identity provider, and that an export without a
# token is refused as 'no token' over the embedded certificate.
set -eu

image=$1
version=$2

fail() {
	echo "smoke: $*" >&2
	exit 1
}

label=$(docker inspect -f '{{ index .Config.Labels "org.opencontainers.image.version" }}' "$image")
[ "$label" = "$version" ] || fail "OCI version label is '$label', want '$version'"
reported=$(docker run --rm "$image" version)
[ "$reported" = "$version" ] || fail "'version' reports '$reported', want '$version'"

cid=$(docker run -d -p 127.0.0.1:4318:4318 -p 127.0.0.1:8888:8888 \
	-e OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4317 \
	-e OIDC_ISSUER_URL=https://issuer.invalid -e OIDC_AUDIENCE=smoke "$image")
trap 'docker rm -f "$cid" >/dev/null' EXIT

i=0
until [ "$(docker inspect -f '{{ .State.Health.Status }}' "$cid")" = healthy ]; do
	i=$((i + 1))
	[ "$i" -le 60 ] || { docker logs "$cid" >&2; fail "HEALTHCHECK never passed"; }
	sleep 1
done

answer=$(curl -sk -w '\n%{http_code}' https://localhost:4318/v1/traces \
	-H 'Content-Type: application/json' \
	-d '{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"5b8efff798038103d269b633813fc60c","spanId":"eee19b7ec3c1b174","name":"smoke","startTimeUnixNano":"1","endTimeUnixNano":"2"}]}]}]}')
code=$(printf '%s\n' "$answer" | tail -n 1)
body=$(printf '%s\n' "$answer" | sed '$d')
[ "$code" = 401 ] || { docker logs "$cid" >&2; fail "export without a token answered $code, want 401"; }
[ "$body" = '{"code":16,"message":"no token"}' ] || fail "401 body is '$body', want the 'no token' status"

curl -s http://127.0.0.1:8888/metrics | grep -q "^target_info{.*service_version=\"$version\"" ||
	fail "target_info does not carry service_version=$version"

echo "smoke: $image $version healthy, tokenless export refused, versions agree"
