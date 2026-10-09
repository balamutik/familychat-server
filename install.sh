#!/usr/bin/env bash
# Public bootstrap: downloads a checksummed, versioned deployment bundle.
set -euo pipefail
main() {
    if [[ "${1:-}" == --help ]]; then
        printf 'FamilyChat installer for Ubuntu/Debian VPS. Usage: sudo bash install.sh [vX.Y.Z]\n'
        return
    fi
    [[ "$(uname -s)" == Linux && "$EUID" == 0 ]] || { echo 'Запустите установку на Ubuntu/Debian VPS через sudo.' >&2; return 1; }
    # shellcheck source=/dev/null
    source /etc/os-release
    [[ "$ID" == ubuntu || "$ID" == debian ]] || { echo 'Поддерживаются Ubuntu и Debian.' >&2; return 1; }
    [[ -d /run/systemd/system ]] || { echo 'Требуется VPS с systemd.' >&2; return 1; }
    if ! command -v python3 >/dev/null || ! command -v curl >/dev/null || [[ ! -f /etc/ssl/certs/ca-certificates.crt ]]; then
        printf 'Для установщика нужны Python 3, curl и CA-сертификаты. Установить? [y/N] ' >/dev/tty
        read -r answer </dev/tty
        case "$answer" in y|Y|yes|да|д) ;; *) echo 'Установка отменена.'; return 1 ;; esac
        apt-get update
        apt-get install -y python3 curl ca-certificates
    fi
    local temp cleanup
    temp="$(mktemp -d)"
    printf -v cleanup 'rm -rf -- %q' "$temp"
    # Capture the quoted path now: the local variable is gone when EXIT runs.
    # shellcheck disable=SC2064
    trap "$cleanup" EXIT
    python3 - "$temp" "${1:-}" <<'PY'
import hashlib, io, json, pathlib, re, sys, tarfile, urllib.request
root = pathlib.Path(sys.argv[1])
repo = 'balamutik/familychat-server'
def get(url):
    request = urllib.request.Request(url, headers={'User-Agent': 'FamilyChat-installer'})
    with urllib.request.urlopen(request, timeout=60) as response:
        data = response.read((16 << 20) + 1)
    if len(data) > 16 << 20:
        raise ValueError('Установочный файл слишком большой')
    return data
try:
    version = sys.argv[2] or json.loads(get(f'https://api.github.com/repos/{repo}/releases/latest'))['tag_name']
    if not re.fullmatch(r'v[0-9]+\.[0-9]+\.[0-9]+', version):
        raise ValueError('Ожидается версия vX.Y.Z')
    base = f'https://github.com/{repo}/releases/download/{version}'
    data = get(base + '/familychat-server.tar.gz')
    sums = get(base + '/SHA256SUMS').decode()
    expected = [line.split()[0] for line in sums.splitlines() if len(line.split()) == 2 and line.split()[1] == 'familychat-server.tar.gz']
    if expected != [hashlib.sha256(data).hexdigest()]:
        raise ValueError('SHA256 архива не совпадает')
    with tarfile.open(fileobj=io.BytesIO(data), mode='r:gz') as archive:
        total = 0
        for member in archive.getmembers():
            path = pathlib.Path(member.name)
            total += member.size
            if path.is_absolute() or '..' in path.parts or not (member.isfile() or member.isdir()) or total > 16 << 20:
                raise ValueError('Небезопасный архив')
            target = root / path
            if member.isdir():
                target.mkdir(parents=True, exist_ok=True)
            else:
                target.parent.mkdir(parents=True, exist_ok=True)
                target.write_bytes(archive.extractfile(member).read())
    manifest = json.loads((root / 'release.json').read_text())
    if manifest['version'] != version or manifest['repository'] != repo:
        raise ValueError('Релиз не совпадает с установочным комплектом')
    (root / 'version').write_text(version)
except Exception as error:
    sys.exit(f'Не удалось получить релиз FamilyChat: {error}. Проверьте доступность публичного релиза.')
PY
    python3 "$temp/scripts/familychat.py" install --version "$(cat "$temp/version")"
}
main "$@"
