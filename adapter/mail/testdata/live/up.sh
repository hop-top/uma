#!/usr/bin/env bash
# Starts the containers of the live mail tests and prints the environment the
# tests read. Everything it creates is named uma-live-evidence-<run>-* and
# labelled uma.live-evidence=1, publishes only to 127.0.0.1 on ports Docker
# picks, and is removed by down.sh. Nothing here leaves the machine.
#
#   eval "$(adapter/mail/testdata/live/up.sh)"      # start, export the env
#   UMA_LIVE_MAIL=1 go test ./adapter/mail/...      # run
#   adapter/mail/testdata/live/down.sh              # remove
#
# Images are pinned by digest (Mailpit) or built from a digest-pinned base
# (Dovecot, Postfix). Progress goes to stderr, the exports to stdout.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
prefix=uma-live-evidence
run=${UMA_LIVE_RUN:-$(date +%s)-$$}
state=${UMA_LIVE_STATE:-${TMPDIR:-/tmp}/$prefix-$run}
mailpit_image='axllent/mailpit:v1.31.2@sha256:74d609a42ec279aa63c6b4622a6fa9b5408d1ad5b1d76a1c4be40a265ce0863d'
labels=(--label uma.live-evidence=1 --label "uma.live-evidence.run=$run")

log() { echo "up: $*" >&2; }

cleanup_on_error() {
  log "failed; removing run $run"
  UMA_LIVE_RUN=$run UMA_LIVE_STATE=$state "$here/down.sh" >&2 || true
}
trap cleanup_on_error ERR

mkdir -p "$state/certs"
chmod 700 "$state"

# --- throwaway PKI: a CA and one server certificate for the loopback names ---
cd "$state/certs"
cat >ca.ext <<'X'
basicConstraints=critical,CA:TRUE
keyUsage=critical,keyCertSign,cRLSign
X
cat >server.ext <<'X'
basicConstraints=CA:FALSE
keyUsage=critical,digitalSignature
extendedKeyUsage=serverAuth
subjectAltName=DNS:localhost,DNS:mail.example.test,IP:127.0.0.1
X
openssl ecparam -genkey -name prime256v1 -noout -out ca.key 2>/dev/null
openssl req -x509 -new -key ca.key -sha256 -days 2 -subj '/CN=uma live evidence CA' \
  -extensions v3 -config <(printf '[req]\ndistinguished_name=d\n[d]\n[v3]\n'; cat ca.ext) -out ca.pem
openssl ecparam -genkey -name prime256v1 -noout -out server.key 2>/dev/null
openssl req -new -key server.key -subj '/CN=localhost' -out server.csr
openssl x509 -req -in server.csr -CA ca.pem -CAkey ca.key -CAcreateserial -sha256 -days 2 \
  -extfile server.ext -out server.pem 2>/dev/null
# Server key and certificate are the only files containers read.
mkdir -p tls && cp server.pem server.key tls/ && chmod 644 tls/*
cd "$here"

# port <container> <container port>: the host port Docker picked.
hostport() { docker port "$1" "$2" | head -n1 | sed 's/.*://'; }

# wait_banner <host> <port> <label> <regexp>: waits until the server behind
# the published port answers with a greeting matching the regexp. Docker's own
# proxy accepts connections before the server in the container listens, so a
# connect alone proves nothing.
wait_banner() {
  local i line
  for i in $(seq 1 120); do
    line=
    if exec 3<>"/dev/tcp/$1/$2" 2>/dev/null; then
      read -r -t 2 line <&3 || true
      exec 3<&- 3>&-
    fi
    if [[ $line =~ $4 ]]; then return 0; fi
    sleep 0.5
  done
  log "$3 did not greet on $1:$2"
  return 1
}

# --- Mailpit: SMTP submission catcher with STARTTLS and AUTH ---
mp=$prefix-$run-mailpit
log "starting $mp"
docker create --name "$mp" "${labels[@]}" \
  -p 127.0.0.1::1025 -p 127.0.0.1::8025 \
  -e MP_SMTP_TLS_CERT=/tls/server.pem -e MP_SMTP_TLS_KEY=/tls/server.key \
  -e MP_SMTP_AUTH='desk@example.test:desk-live-pw' \
  -e MP_MAX_MESSAGES=5000 \
  "$mailpit_image" >/dev/null
tar -C "$state/certs" -cf - tls | docker cp - "$mp:/"
docker start "$mp" >/dev/null
mp_smtp=$(hostport "$mp" 1025/tcp)
mp_http=$(hostport "$mp" 8025/tcp)

# --- Dovecot: IMAP, implicit TLS and STARTTLS ---
dv=$prefix-$run-dovecot
log "building and starting $dv"
docker build -q -t "$prefix-dovecot:1" "$here/dovecot" >/dev/null
docker create --name "$dv" "${labels[@]}" -p 127.0.0.1::143 -p 127.0.0.1::993 \
  "$prefix-dovecot:1" >/dev/null
tar -C "$state/certs" -cf - tls | docker cp - "$dv:/etc/dovecot/"
docker start "$dv" >/dev/null
dv_starttls=$(hostport "$dv" 143/tcp)
dv_tls=$(hostport "$dv" 993/tcp)

# --- Postfix: the sending MTA of the smtp.mail tests ---
pf=$prefix-$run-postfix
log "building and starting $pf"
docker build -q -t "$prefix-postfix:1" "$here/postfix" >/dev/null
docker run -d --name "$pf" "${labels[@]}" -p 127.0.0.1::25 "$prefix-postfix:1" >/dev/null
pf_port=$(hostport "$pf" 25/tcp)

wait_banner 127.0.0.1 "$mp_smtp" mailpit '^220 '
wait_banner 127.0.0.1 "$dv_starttls" dovecot '^\* OK '
wait_banner 127.0.0.1 "$pf_port" postfix '^220 '
# Implicit TLS has no plaintext greeting; the STARTTLS listener of the same
# Dovecot is up, and 993 follows it. Postfix must also answer its control
# commands before the tests reload it.
for i in $(seq 1 120); do
  docker exec "$pf" postfix status >/dev/null 2>&1 && break
  [ "$i" -lt 120 ] || { log "postfix did not start"; false; }
  sleep 0.5
done

env=$state/env
{
  echo "export UMA_LIVE_MAIL=1"
  echo "export UMA_LIVE_RUN=$run"
  echo "export UMA_LIVE_CA=$state/certs/ca.pem"
  echo "export UMA_LIVE_IMAP_HOST=127.0.0.1"
  echo "export UMA_LIVE_IMAP_TLS_PORT=$dv_tls"
  echo "export UMA_LIVE_IMAP_STARTTLS_PORT=$dv_starttls"
  echo "export UMA_LIVE_IMAP_USER=desk@example.test"
  echo "export UMA_LIVE_IMAP_PASS=desk-live-pw"
  echo "export UMA_LIVE_SMTP_HOST=127.0.0.1"
  echo "export UMA_LIVE_SMTP_STARTTLS_PORT=$mp_smtp"
  echo "export UMA_LIVE_SMTP_USER=desk@example.test"
  echo "export UMA_LIVE_SMTP_PASS=desk-live-pw"
  echo "export UMA_LIVE_SMTP_API=http://127.0.0.1:$mp_http"
  echo "export UMA_LIVE_MTA_CONTAINER=$pf"
  echo "export UMA_LIVE_MTA_PORT=$pf_port"
  echo "export UMA_LIVE_STATE=$state"
} >"$env"
log "ready; environment in $env"
cat "$env"
