#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

usage() {
    cat <<'EOF'
Usage:
  scripts/deploy-https.sh up --host HOST (--cert CERT --key KEY | --self-signed | --letsencrypt --email EMAIL) [options]
  scripts/deploy-https.sh renew --host HOST [options]

HOST is a DNS name or an IPv4/IPv6 address. Options:
  --bind ADDRESS       Published host address (default: 0.0.0.0; use :: for IPv6)
  --http-port PORT     Published HTTP/ACME port (default: 80)
  --https-port PORT    Published HTTPS port (default: 443)
  --no-build           Reuse the existing server image instead of rebuilding it
  --dry-run            Validate options, supplied PEM files and Compose without starting containers
  --staging            Use Let's Encrypt staging for the first issuance

Public Let's Encrypt issuance requires incoming port 80 and a publicly reachable
DNS name or IP. For a private/LAN IP, use --self-signed or a certificate from
your own CA. Run 'renew --host HOST' daily for Let's Encrypt certificates.
EOF
}

fail() { printf 'Error: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || fail "$1 is required"; }
compose() { docker compose -f compose.yaml -f compose.https.yaml "$@"; }
certbot_run() {
    compose --profile certbot run --rm --no-deps --user "$(id -u):$(id -g)" certbot \
        --work-dir /etc/letsencrypt/work --logs-dir /etc/letsencrypt/logs "$@"
}

[[ $# -gt 0 ]] || { usage; exit 2; }
ACTION="$1"
shift
[[ "$ACTION" == up || "$ACTION" == renew ]] || { usage; exit 2; }

HOST=""
BIND_HOST="0.0.0.0"
HTTP_PORT=80
HTTPS_PORT=443
MODE=""
CERT_FILE=""
KEY_FILE=""
EMAIL=""
NO_BUILD=false
DRY_RUN=false
STAGING=false

while [[ $# -gt 0 ]]; do
    case "$1" in
        --host|--bind|--http-port|--https-port|--cert|--key|--email)
            [[ $# -ge 2 ]] || fail "$1 requires a value"
            case "$1" in
                --host) HOST="$2" ;;
                --bind) BIND_HOST="$2" ;;
                --http-port) HTTP_PORT="$2" ;;
                --https-port) HTTPS_PORT="$2" ;;
                --cert) CERT_FILE="$2" ;;
                --key) KEY_FILE="$2" ;;
                --email) EMAIL="$2" ;;
            esac
            shift 2 ;;
        --self-signed|--letsencrypt)
            [[ -z "$MODE" ]] || fail "choose only one certificate mode"
            MODE="${1#--}"
            shift ;;
        --no-build) NO_BUILD=true; shift ;;
        --dry-run) DRY_RUN=true; shift ;;
        --staging) STAGING=true; shift ;;
        -h|--help) usage; exit 0 ;;
        *) fail "unknown argument: $1" ;;
    esac
done

need docker
need openssl
need python3
need curl
[[ -f .env ]] || fail "create server/.env from .env.example first"
[[ -f secrets/admin-password.txt ]] || fail "create secrets/admin-password.txt first"
[[ -n "$HOST" ]] || fail "--host is required"

HOST_KIND="$(python3 - "$HOST" <<'PY'
import ipaddress, re, sys
host = sys.argv[1]
try:
    address = ipaddress.ip_address(host)
except ValueError:
    if len(host) > 253 or not re.fullmatch(r'[A-Za-z0-9.-]+', host):
        sys.exit('invalid DNS name')
    labels = host.rstrip('.').split('.')
    if not labels or any(not re.fullmatch(r'[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?', x) for x in labels):
        sys.exit('invalid DNS name')
    print('dns')
else:
    print('ipv4' if address.version == 4 else 'ipv6')
PY
)" || fail "invalid --host"

for port in "$HTTP_PORT" "$HTTPS_PORT"; do
    [[ "$port" =~ ^[0-9]+$ ]] || fail "ports must be numbers"
    (( 10#$port >= 1 && 10#$port <= 65535 )) || fail "port is outside 1..65535"
done
[[ "$HTTP_PORT" != "$HTTPS_PORT" ]] || fail "HTTP and HTTPS ports must differ"

if [[ "$BIND_HOST" == "::" ]]; then
    COMPOSE_BIND_HOST='[::]'
    PROBE_HOST='[::1]'
else
    python3 - "$BIND_HOST" <<'PY' || fail "--bind must be an IPv4 address or ::"
import ipaddress, sys
try:
    value = ipaddress.ip_address(sys.argv[1])
except ValueError:
    sys.exit(1)
sys.exit(0 if value.version == 4 else 1)
PY
    COMPOSE_BIND_HOST="$BIND_HOST"
    PROBE_HOST='127.0.0.1'
    [[ "$BIND_HOST" == 0.0.0.0 || "$BIND_HOST" == 127.0.0.1 ]] && : || PROBE_HOST="$BIND_HOST"
fi

PUBLIC_HOST="$HOST"
[[ "$HOST_KIND" == ipv6 ]] && PUBLIC_HOST="[$HOST]"
HTTPS_REDIRECT_PORT=""
[[ "$HTTPS_PORT" == 443 ]] || HTTPS_REDIRECT_PORT=":$HTTPS_PORT"
HTTPS_ALLOWED_ORIGINS="$PUBLIC_HOST$HTTPS_REDIRECT_PORT"
HTTPS_BIND_HOST="$COMPOSE_BIND_HOST"
HTTPS_HTTP_PORT="$HTTP_PORT"
HTTPS_HTTPS_PORT="$HTTPS_PORT"
export HTTPS_REDIRECT_PORT HTTPS_ALLOWED_ORIGINS HTTPS_BIND_HOST HTTPS_HTTP_PORT HTTPS_HTTPS_PORT

CERT_DIR="$ROOT_DIR/.https/certs"
LE_DIR="$ROOT_DIR/.https/letsencrypt"
mkdir -p "$CERT_DIR" "$ROOT_DIR/.https/challenges" "$LE_DIR"
chmod 700 "$ROOT_DIR/.https" "$CERT_DIR" "$LE_DIR"
CERT_NAME="familychat-$(printf '%s' "$HOST" | openssl dgst -sha256 -r | awk '{print substr($1,1,12)}')"
[[ "$STAGING" == false ]] || CERT_NAME+="-staging"
LE_CERT="$LE_DIR/live/$CERT_NAME/fullchain.pem"
LE_KEY="$LE_DIR/live/$CERT_NAME/privkey.pem"
CERT_BOOTSTRAP=false

check_identity() {
    local san
    san="$(openssl x509 -in "$1" -noout -ext subjectAltName)" || return 1
    if [[ "$HOST_KIND" == dns ]]; then
        [[ "$san" == *DNS:* ]] || return 1
        openssl x509 -in "$1" -noout -checkhost "$HOST" >/dev/null
    else
        [[ "$san" == *"IP Address:"* ]] || return 1
        openssl x509 -in "$1" -noout -checkip "$HOST" >/dev/null
    fi
}

validate_certificate() {
    [[ -r "$1" && -r "$2" ]] || fail "certificate and private key must be readable"
    openssl x509 -in "$1" -noout >/dev/null || fail "invalid certificate PEM"
    openssl pkey -in "$2" -passin pass: -noout >/dev/null 2>&1 || fail "invalid or encrypted private key PEM"
    check_identity "$1" || fail "certificate does not contain $HOST in its subjectAltName"
    openssl x509 -in "$1" -noout -checkend 0 >/dev/null || fail "certificate has expired"
    local cert_public key_public
    cert_public="$(openssl x509 -in "$1" -pubkey -noout | openssl pkey -pubin -outform DER | openssl dgst -sha256)"
    key_public="$(openssl pkey -in "$2" -passin pass: -pubout -outform DER | openssl dgst -sha256)"
    [[ "$cert_public" == "$key_public" ]] || fail "certificate and private key do not match"
}

install_certificate() {
    validate_certificate "$1" "$2"
    local tmp_cert tmp_key
    tmp_cert="$(mktemp "$CERT_DIR/.fullchain.XXXXXX")"
    tmp_key="$(mktemp "$CERT_DIR/.privkey.XXXXXX")"
    install -m 644 "$1" "$tmp_cert"
    install -m 600 "$2" "$tmp_key"
    mv -f "$tmp_cert" "$CERT_DIR/fullchain.pem"
    mv -f "$tmp_key" "$CERT_DIR/privkey.pem"
}

self_signed() {
    local temp_cert temp_key san
    temp_cert="$(mktemp "$CERT_DIR/.self-cert.XXXXXX")"
    temp_key="$(mktemp "$CERT_DIR/.self-key.XXXXXX")"
    san="DNS:$HOST"
    [[ "$HOST_KIND" == dns ]] || san="IP:$HOST"
    openssl req -x509 -newkey rsa:3072 -sha256 -nodes -days 365 \
        -subj "/CN=$HOST" -addext "subjectAltName=$san" \
        -keyout "$temp_key" -out "$temp_cert" >/dev/null 2>&1
    install_certificate "$temp_cert" "$temp_key"
    rm -f "$temp_cert" "$temp_key"
}

if [[ "$ACTION" == up ]]; then
    if [[ -n "$CERT_FILE" || -n "$KEY_FILE" ]]; then
        [[ -n "$CERT_FILE" && -n "$KEY_FILE" && -z "$MODE" ]] || fail "--cert and --key must be used together"
        MODE=provided
    fi
    [[ -n "$MODE" ]] || fail "choose --cert/--key, --self-signed or --letsencrypt"
    if [[ "$MODE" == letsencrypt ]]; then
        [[ -n "$EMAIL" ]] || fail "--email is required with --letsencrypt"
        [[ "$HTTP_PORT" == 80 && "$HTTPS_PORT" == 443 ]] || fail "Let's Encrypt requires public ports 80 and 443"
        if [[ "$HOST_KIND" != dns ]]; then
            python3 - "$HOST" <<'PY' || fail "Let's Encrypt requires a public IP; use --self-signed for a LAN IP"
import ipaddress, sys
sys.exit(0 if ipaddress.ip_address(sys.argv[1]).is_global else 1)
PY
        fi
    else
        [[ "$STAGING" == false ]] || fail "--staging is only for Let's Encrypt"
    fi

    if [[ "$DRY_RUN" == true ]]; then
        [[ "$MODE" != provided ]] || validate_certificate "$CERT_FILE" "$KEY_FILE"
        compose config -q
        printf 'Configuration valid; HTTPS will use https://%s%s/ (%s certificate).\n' "$PUBLIC_HOST" "$HTTPS_REDIRECT_PORT" "$MODE"
        exit 0
    fi

    if [[ "$MODE" == provided ]]; then
        install_certificate "$CERT_FILE" "$KEY_FILE"
    elif [[ "$MODE" == self-signed ]]; then
        if [[ ! -f .https/mode || ! -f .https/host || "$(cat .https/mode)" != self-signed || "$(cat .https/host)" != "$HOST" ]] || \
           [[ ! -f "$CERT_DIR/privkey.pem" ]] || \
           ! check_identity "$CERT_DIR/fullchain.pem" 2>/dev/null || \
           ! openssl x509 -in "$CERT_DIR/fullchain.pem" -noout -checkend 2592000 >/dev/null 2>&1; then
            self_signed
        fi
    else
        if [[ -f "$LE_CERT" && -f "$LE_KEY" ]]; then
            if openssl x509 -in "$LE_CERT" -noout -checkend 0 >/dev/null 2>&1; then
                install_certificate "$LE_CERT" "$LE_KEY"
            else
                self_signed
                CERT_BOOTSTRAP=true
            fi
        else
            self_signed
            CERT_BOOTSTRAP=true
        fi
    fi

    compose config -q
    # Pull proxy tooling before changing an existing API deployment.
    compose pull nginx
    [[ "$MODE" == letsencrypt ]] && compose --profile certbot pull certbot
    if [[ "$CERT_BOOTSTRAP" == true ]]; then
        trap 'compose stop nginx || true' EXIT
    fi
    if [[ "$NO_BUILD" == true ]]; then
        compose up -d --no-build api worker turn nginx
    else
        compose up -d --build api worker turn nginx
    fi

    if [[ "$MODE" == letsencrypt ]]; then
        issued=true
        if [[ -f "$LE_CERT" ]]; then
            certbot_run renew --non-interactive --cert-name "$CERT_NAME" || issued=false
        else
            issue=(certonly --webroot --webroot-path /var/www/acme --non-interactive
                   --agree-tos --email "$EMAIL" --cert-name "$CERT_NAME")
            [[ "$STAGING" == false ]] || issue+=(--staging)
            if [[ "$HOST_KIND" == dns ]]; then
                issue+=(--domain "$HOST")
            else
                issue+=(--preferred-profile shortlived --ip-address "$HOST")
            fi
            certbot_run "${issue[@]}" || issued=false
        fi
        if [[ "$issued" == false ]]; then
            fail "certificate issuance/renewal failed"
        fi
        install_certificate "$LE_CERT" "$LE_KEY"
        compose exec -T nginx nginx -t
        compose exec -T nginx nginx -s reload
        trap - EXIT
    fi

    curl -kfsS --max-time 15 "https://$PROBE_HOST:$HTTPS_PORT/health/ready" >/dev/null || \
        fail "HTTPS is running but /health/ready did not answer through nginx"
    printf '%s\n' "$MODE" > .https/mode
    printf '%s\n' "$HOST" > .https/host
    printf 'Ready: https://%s%s/\n' "$PUBLIC_HOST" "$HTTPS_REDIRECT_PORT"
    [[ "$MODE" != self-signed ]] || printf 'Trust the local certificate on each client before connecting.\n'
else
    [[ -z "$MODE$CERT_FILE$KEY_FILE$EMAIL" ]] || fail "renew uses the existing Let's Encrypt certificate; pass only --host and network options"
    [[ "$STAGING" == false && "$NO_BUILD" == false ]] || fail "--staging and --no-build are not used with renew"
    [[ -f "$LE_CERT" && -f "$LE_KEY" ]] || fail "no Let's Encrypt certificate found for $HOST"
    compose config -q
    [[ "$DRY_RUN" == false ]] || { printf 'Renewal configuration valid for %s.\n' "$HOST"; exit 0; }
    certbot_run renew --non-interactive --cert-name "$CERT_NAME"
    install_certificate "$LE_CERT" "$LE_KEY"
    compose exec -T nginx nginx -t
    compose exec -T nginx nginx -s reload
    printf 'Certificate refreshed for %s.\n' "$HOST"
fi
