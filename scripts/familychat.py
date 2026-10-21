#!/usr/bin/env python3
"""FamilyChat release installer and local maintenance CLI (stdlib only)."""
import argparse
import base64
import fcntl
import hashlib
import io
import ipaddress
import json
import os
from pathlib import Path
import re
import secrets
import shlex
import shutil
import socket
import subprocess
import sys
import tarfile
import tempfile
import urllib.error
import urllib.request
import urllib.parse

DEFAULT_REPOSITORY = 'balamutik/familychat-server'
REPOSITORY = DEFAULT_REPOSITORY
DEFAULT_ROOT = Path('/opt/familychat')
COMMAND_PATH = Path('/usr/local/bin/familychat')
VERSION = re.compile(r'v[0-9]+\.[0-9]+\.[0-9]+')


class InstallError(Exception):
    pass


def run(args, **kwargs):
    try:
        return subprocess.run([str(a) for a in args], check=True, **kwargs)
    except subprocess.CalledProcessError as error:
        raise InstallError(f'Команда завершилась с ошибкой: {args[0]} (код {error.returncode}).') from error


def download(url, limit=16 << 20):
    if not url.startswith('https://'):
        raise InstallError('Загрузка разрешена только по HTTPS.')
    try:
        request = urllib.request.Request(url, headers={'User-Agent': 'FamilyChat-installer'})
        with urllib.request.urlopen(request, timeout=60) as response:
            data = response.read(limit + 1)
        if len(data) > limit:
            raise InstallError('Слишком большой установочный файл.')
        return data
    except urllib.error.URLError as error:
        raise InstallError(f'Не удалось скачать {url}. Проверьте сеть и публичность релиза.') from error


def latest_version(repository):
    metadata = json.loads(download(f'https://api.github.com/repos/{repository}/releases/latest'))
    version = metadata.get('tag_name', '')
    if not VERSION.fullmatch(version):
        raise InstallError('Нет стабильного релиза vX.Y.Z с установочным комплектом.')
    return version


def atomic_write(path, data, mode=0o600):
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp = tempfile.mkstemp(dir=path.parent)
    try:
        with os.fdopen(fd, 'w') as stream:
            stream.write(data)
        os.chmod(tmp, mode)
        os.replace(tmp, path)
    finally:
        if os.path.exists(tmp):
            os.unlink(tmp)


def save_state(root, state):
    atomic_write(root / 'installation.json', json.dumps(state, indent=2) + '\n')


def load_state(root):
    state = json.loads((root / 'installation.json').read_text())
    for name in ('.env', 'secrets/admin-password.txt'):
        if not (root / name).is_file():
            raise InstallError(f'Отсутствует {root / name}. Восстановите файл из резервной копии; секреты не изменены.')
    return state


def validate_input(domain, email, public_ip):
    labels = domain.split('.')
    if len(domain) > 253 or len(labels) < 2 or any(not re.fullmatch(r'[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?', x) for x in labels):
        raise InstallError('Введите домен, например chat.example.com, без https:// и пути.')
    try:
        ipaddress.ip_address(domain)
    except ValueError:
        pass
    else:
        raise InstallError('Для этой установки нужен домен с DNS-записью A.')
    if not re.fullmatch(r'[^\s@]+@[^\s@]+\.[^\s@]+', email):
        raise InstallError('Некорректный email для сертификата.')
    try:
        address = ipaddress.IPv4Address(public_ip)
        if not address.is_global:
            raise ValueError()
    except ValueError:
        raise InstallError('Нужен публичный IPv4-адрес VPS.')


def configure(root, domain, email, public_ip):
    if (root / 'installation.json').exists():
        return load_state(root)
    validate_input(domain, email, public_ip)
    if any((root / name).exists() for name in ('.env', 'secrets', 'compose.yaml', 'current', '.https')):
        raise InstallError('Обнаружены существующие настройки. Автоматический импорт старой установки не выполняется.')
    root.mkdir(parents=True, exist_ok=True, mode=0o700)
    root.chmod(0o700)
    state = {'domain': domain, 'email': email, 'public_ip': public_ip, 'repository': REPOSITORY,
             'project': 'familychat', 'admin_created': False}
    values = {'POSTGRES_PASSWORD': secrets.token_hex(32), 'S3_ACCESS_KEY': secrets.token_hex(16),
              'S3_SECRET_KEY': secrets.token_hex(32), 'TURN_SECRET': secrets.token_hex(32),
              'TURN_URL': f'turn:{public_ip}:3478?transport=udp', 'TURN_EXTERNAL_IP': public_ip,
              'ALLOWED_ORIGINS': domain, 'HTTPS_ALLOWED_ORIGINS': domain, 'ADMIN_LOGIN': 'admin',
              'APNS_SANDBOX': 'true'}
    atomic_write(root / '.env', ''.join(f'{key}={value}\n' for key, value in values.items()))
    (root / 'secrets').mkdir(mode=0o700)
    atomic_write(root / 'secrets/admin-password.txt', secrets.token_urlsafe(24) + '\n', 0o644)
    # Compose file-backed secrets retain host permissions; the non-root API must read this file.
    # Its parent and installation directory remain root-only (0700).
    (root / '.https').mkdir(mode=0o700)
    save_state(root, state)
    return state


def extract_bundle(data, destination):
    with tarfile.open(fileobj=io.BytesIO(data), mode='r:gz') as archive:
        members = archive.getmembers()
        total = 0
        for member in members:
            path = Path(member.name)
            total += member.size
            if path.is_absolute() or '..' in path.parts or not (member.isfile() or member.isdir()) or total > 16 << 20:
                raise InstallError('Небезопасный установочный архив.')
        destination.mkdir(parents=True, exist_ok=True)
        for member in members:
            path = destination / member.name
            if member.isdir():
                path.mkdir(parents=True, exist_ok=True)
            else:
                path.parent.mkdir(parents=True, exist_ok=True)
                with archive.extractfile(member) as source:
                    path.write_bytes(source.read())
                path.chmod(0o755 if member.mode & 0o111 else 0o644)


def fetch_bundle(repository, version, destination):
    if not VERSION.fullmatch(version) or not re.fullmatch(r'[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+', repository):
        raise InstallError('Некорректная версия или репозиторий.')
    base = f'https://github.com/{repository}/releases/download/{version}'
    data = download(f'{base}/familychat-server.tar.gz')
    checksums = download(f'{base}/SHA256SUMS').decode()
    expected = [line.split()[0] for line in checksums.splitlines()
                if len(line.split()) == 2 and line.split()[1] == 'familychat-server.tar.gz']
    if expected != [hashlib.sha256(data).hexdigest()]:
        raise InstallError('SHA256 установочного архива не совпадает.')
    if destination.exists():
        raise InstallError('Каталог выбранного релиза уже существует.')
    destination.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(dir=destination.parent) as temp:
        stage = Path(temp) / 'release'
        extract_bundle(data, stage)
        manifest = json.loads((stage / 'release.json').read_text())
        if manifest['version'] != version or manifest['repository'] != repository:
            raise InstallError('Версия установочного комплекта не совпадает с релизом.')
        for name in ('compose.yaml', 'compose.https.yaml', 'scripts/familychat.py', 'scripts/deploy-https.sh', 'deploy/nginx/default.conf.template'):
            if not (stage / name).is_file():
                raise InstallError(f'В релизе отсутствует {name}.')
        if manifest.get('external_key_service', 0) >= 1 and not (stage / 'compose.keyserver.yaml').is_file():
            raise InstallError('В релизе отсутствует compose.keyserver.yaml.')
        if manifest.get('push_gateway', 0) >= 1 and not (stage / 'compose.push-gateway.yaml').is_file():
            raise InstallError('В релизе отсутствует compose.push-gateway.yaml.')
        stage.rename(destination)


def environment(root, state):
    # Do not let an unrelated shell's Compose/APNs/DATABASE settings override managed settings.
    env = {key: os.environ[key] for key in ('PATH', 'HOME', 'LANG', 'TERM') if key in os.environ}
    env['COMPOSE_PROJECT_NAME'] = state['project']
    env['HTTPS_ALLOWED_ORIGINS'] = state['domain']
    return env


def external_keys_enabled(root):
    return any(line.startswith('KEY_SERVICE_CONFIG_FILE=') and line.partition('=')[2].strip()
               for line in (root / '.env').read_text().splitlines())


def configure_key_service(root, source):
    try:
        if not source.is_file() or source.stat().st_size > 16384:
            raise ValueError()
        config = json.loads(source.read_text())
        expected = {'scheme', 'key_id', 'public_key', 'key_server_url', 'ticket_issuer', 'ticket_secret'}
        if not isinstance(config, dict) or set(config) != expected or not all(isinstance(value, str) for value in config.values()):
            raise ValueError()
        public = base64.b64decode(config['public_key'], validate=True)
        secret = base64.b64decode(config['ticket_secret'], validate=True)
        endpoint = urllib.parse.urlsplit(config['key_server_url'])
        if (config['scheme'] != 'fc1' or len(public) != 32 or len(secret) != 32
                or hashlib.sha256(public).hexdigest() != config['key_id']
                or not config['ticket_issuer'].strip() or len(config['ticket_issuer']) > 200
                or endpoint.scheme != 'https' or not endpoint.hostname
                or endpoint.username is not None or endpoint.password is not None
                or endpoint.query or endpoint.fragment or endpoint.path != '/api/v1/keys/current'
                or endpoint.port is None or not 1 <= endpoint.port <= 65535
                or endpoint.port in (80, 443, 8080)):
            raise ValueError()
    except (OSError, ValueError, TypeError):
        raise InstallError('Некорректный файл подключения сервера ключей. Используйте main-server.json, экспортированный сервером ключей; нужен HTTPS и отдельный порт (обычно 8443).')
    existing = root / 'secrets/content-public-key.txt'
    previous = root / 'secrets/key-service.json'
    try:
        old_public = existing.read_text().strip() if existing.is_file() else None
        if previous.is_file():
            old_public = json.loads(previous.read_text())['public_key']
        if old_public is not None and base64.b64decode(old_public, validate=True) != public:
            raise InstallError('Новый ключ не совпадает с текущим. Сначала импортируйте исходную пару ключей в отдельный сервер ключей, чтобы сохранить историю.')
    except (OSError, ValueError, KeyError):
        raise InstallError('Не удалось проверить существующий ключ. Восстановите настройки из резервной копии.')
    atomic_write(previous, json.dumps(config, indent=2) + '\n', 0o400)
    if os.geteuid() == 0:
        os.chown(previous, 10001, 10001)
    env_file = root / '.env'
    lines = [line for line in env_file.read_text().splitlines() if not line.startswith('KEY_SERVICE_CONFIG_FILE=')]
    atomic_write(env_file, '\n'.join(lines) + '\nKEY_SERVICE_CONFIG_FILE=./secrets/key-service.json\n')


def compose(root, release, state, *args, **kwargs):
    files = ['-f', release / 'compose.yaml', '-f', release / 'compose.https.yaml']
    if external_keys_enabled(root):
        if not (release / 'compose.keyserver.yaml').is_file():
            raise InstallError('Этот релиз не поддерживает отдельный сервер ключей. Обновите основной сервер.')
        files += ['-f', release / 'compose.keyserver.yaml']
    if push_gateway_enabled(root):
        require_push_gateway_release(release)
        files += ['-f', release / 'compose.push-gateway.yaml']
    return run(['docker', 'compose', '--project-name', state['project'], '--env-file', root / '.env',
                *files, *args], cwd=release, env=environment(root, state), **kwargs)


def link_state(root, release):
    for name in ('.env', 'secrets', '.https'):
        path = release / name
        if not path.is_symlink():
            path.symlink_to(root / name)
        elif path.resolve() != (root / name).resolve():
            raise InstallError(f'Неверная ссылка на настройки: {path}')


def release_protocols(root, candidate, state):
    current_path = root / 'current/release.json'
    candidate_path = candidate / 'release.json'
    current = json.loads(current_path.read_text()) if current_path.exists() else {}
    selected = json.loads(candidate_path.read_text()) if candidate_path.exists() else {}
    if external_keys_enabled(root) and selected.get('external_key_service', 0) < 1:
        raise InstallError('Этот релиз не поддерживает отдельный сервер ключей; откат отменён.')
    if push_gateway_enabled(root):
        require_push_gateway_release(candidate)
    minimum_crypto = max(current.get('content_encryption', 0), state.get('content_encryption', 0))
    candidate_crypto = selected.get('content_encryption', 0)
    if minimum_crypto > candidate_crypto:
        raise InstallError('Нельзя откатить зашифрованную установку на сервер без поддержки шифрования.')
    minimum_grants = max(current.get('device_key_grants', 0), state.get('device_key_grants', 0))
    candidate_grants = selected.get('device_key_grants', 0)
    if (type(candidate_grants) is not int or candidate_grants < minimum_grants
            or (candidate_grants > 0 and selected.get('key_delivery') != 'device-grant-v1')):
        raise InstallError('Нельзя откатить установку на релиз без защищённой выдачи ключей для устройств (device-grant-v1).')
    return {'content_encryption': max(minimum_crypto, candidate_crypto),
            'device_key_grants': max(minimum_grants, candidate_grants)}


def require_device_grant_release(release):
    manifest = json.loads((release / 'release.json').read_text())
    grants = manifest.get('device_key_grants', 0)
    if (manifest.get('external_key_service', 0) < 1 or type(grants) is not int or grants < 1
            or manifest.get('key_delivery') != 'device-grant-v1'):
        raise InstallError('Сначала обновите основной сервер до релиза с выдачей ключей для устройств (device-grant-v1).')


def activate(root, candidate, state):
    protocols = release_protocols(root, candidate, state)
    link_state(root, candidate)
    compose(root, candidate, state, 'config', '-q')
    compose(root, candidate, state, 'pull')
    state['pending_version'] = candidate.name
    # Persist before starting a candidate: migration can finish before activation does.
    state.update(protocols)
    save_state(root, state)
    compose(root, candidate, state, 'stop', 'api', 'worker')
    run(['bash', candidate / 'scripts/deploy-https.sh', 'up', '--host', state['domain'],
         '--letsencrypt', '--email', state['email'], '--no-build'], env=environment(root, state))
    compose(root, candidate, state, 'up', '-d', '--no-build', '--wait', '--wait-timeout', '86400',
            'api', 'worker', 'turn', 'nginx')
    current = root / 'current'
    pending = root / '.current-next'
    pending.unlink(missing_ok=True)
    pending.symlink_to(candidate)
    os.replace(pending, current)
    state.pop('pending_version', None)
    save_state(root, state)


def cleanup_managed_images(candidate):
    """Remove unused labelled release images, never containers or data volumes."""
    candidate = candidate.resolve()
    settings = candidate / '.env'
    if not settings.is_symlink():
        return
    root = settings.resolve().parent
    if candidate.parent != root / 'releases' or not (root / 'installation.json').is_file():
        return
    try:
        run(['docker', 'image', 'prune', '--all', '--force', '--filter',
             'label=org.opencontainers.image.source=https://github.com/' + REPOSITORY])
    except (InstallError, OSError) as error:
        print(f'Предупреждение: очистка старых образов не выполнена: {error}', file=sys.stderr)


def prompt(message):
    # A byte-wise terminal erase can leave half of a UTF-8 character. Read one
    # raw line at a time so a decoding failure never consumes the next answer.
    with open('/dev/tty', 'w', encoding='utf-8') as output, open('/dev/tty', 'rb', buffering=0) as source:
        while True:
            output.write(message + ' ')
            output.flush()
            raw = source.readline()
            if not raw:
                raise InstallError('Ввод прерван.')
            try:
                return raw.decode('utf-8').strip()
            except UnicodeDecodeError:
                output.write('Некорректная кодировка ввода. Переключите раскладку и введите ответ заново.\n')


def require_system():
    if sys.platform != 'linux' or os.geteuid() != 0:
        raise InstallError('Запустите установку на Ubuntu/Debian VPS через sudo.')
    info = dict(line.strip().split('=', 1) for line in Path('/etc/os-release').read_text().splitlines() if '=' in line)
    distro = info.get('ID', '').strip('"')
    codename = info.get('VERSION_CODENAME', '').strip('"')
    if distro not in ('ubuntu', 'debian') or not re.fullmatch(r'[a-z]+', codename):
        raise InstallError('Поддерживаются Ubuntu и Debian.')
    if not Path('/run/systemd/system').exists():
        raise InstallError('Для автоматического продления HTTPS требуется systemd.')
    return distro, codename


def install_dependencies(packages):
    if prompt('Нужны пакеты: ' + ', '.join(packages) + '. Установить? [y/N]').lower() not in ('y', 'yes', 'д', 'да'):
        raise InstallError('Установка зависимостей отменена.')
    run(['apt-get', 'update'])
    run(['apt-get', 'install', '-y', *packages])


def compose_supported():
    try:
        version = run(['docker', 'compose', 'version', '--short'], capture_output=True, text=True).stdout.strip()
    except InstallError:
        return False
    match = re.match(r'v?(\d+)\.(\d+)\.(\d+)', version)
    return bool(match and tuple(map(int, match.groups())) >= (2, 24, 4))


def installed_package(name):
    try:
        return run(['dpkg-query', '-W', '-f=${Status}', name], capture_output=True, text=True).stdout.strip() == 'install ok installed'
    except InstallError:
        return False


def ensure_docker(distro, codename):
    missing = [command for command in ('curl', 'openssl') if not shutil.which(command)]
    if missing:
        install_dependencies(missing)
        if any(not shutil.which(command) for command in missing):
            raise InstallError('Не удалось установить curl/OpenSSL. Проверьте PATH и пакеты apt.')
    if not shutil.which('docker'):
        if prompt('Docker не установлен. Установить из официального репозитория Docker? [y/N]').lower() not in ('y', 'yes', 'д', 'да'):
            raise InstallError('Установка отменена. Данные не изменены.')
        run(['apt-get', 'update'])
        run(['apt-get', 'install', '-y', 'ca-certificates', 'curl', 'openssl'])
        key = Path('/etc/apt/keyrings/docker.asc')
        atomic_write(key, download(f'https://download.docker.com/linux/{distro}/gpg').decode(), 0o644)
        arch = run(['dpkg', '--print-architecture'], capture_output=True, text=True).stdout.strip()
        source = f'Types: deb\nURIs: https://download.docker.com/linux/{distro}\nSuites: {codename}\nComponents: stable\nArchitectures: {arch}\nSigned-By: {key}\n'
        atomic_write(Path('/etc/apt/sources.list.d/docker.sources'), source, 0o644)
        run(['apt-get', 'update'])
        run(['apt-get', 'install', '-y', 'docker-ce', 'docker-ce-cli', 'containerd.io', 'docker-buildx-plugin', 'docker-compose-plugin'])
        run(['systemctl', 'enable', '--now', 'docker'])
    run(['docker', 'info'], stdout=subprocess.DEVNULL)
    if not compose_supported():
        if installed_package('docker-ce-cli'):
            package = 'docker-compose-plugin'
        elif installed_package('docker.io'):
            package = 'docker-compose-v2' if distro == 'ubuntu' else 'docker-compose'
        else:
            raise InstallError('Не удалось определить поставщика Docker. Установите Compose plugin 2.24.4 или новее из того же источника, что Docker.')
        install_dependencies([package])
        if not compose_supported():
            raise InstallError('После установки требуется Docker Compose 2.24.4 или новее. Подключите актуальный репозиторий выбранного поставщика Docker.')


def check_network(domain, public_ip, fresh):
    try:
        addresses = {entry[4][0] for entry in socket.getaddrinfo(domain, None, socket.AF_INET)}
        ipv6 = {entry[4][0] for entry in socket.getaddrinfo(domain, None, socket.AF_INET6)}
    except socket.gaierror:
        try:
            addresses = {entry[4][0] for entry in socket.getaddrinfo(domain, None, socket.AF_INET)}
            ipv6 = set()
        except socket.gaierror:
            raise InstallError('Домен не разрешается в IPv4. Создайте DNS-запись A для VPS.')
    if addresses != {public_ip}:
        raise InstallError(f'DNS-запись A должна указывать только на {public_ip}; отключите CDN-проксирование.')
    if ipv6:
        raise InstallError('Для первого выпуска установщик поддерживает IPv4: удалите AAAA-запись этого домена.')
    if fresh:
        ports = [(socket.SOCK_STREAM, port) for port in (80, 443, 3478)]
        ports += [(socket.SOCK_DGRAM, port) for port in (3478, *range(55000, 55040))]
        for kind, port in ports:
            with socket.socket(socket.AF_INET, kind) as sock:
                try:
                    sock.bind(('0.0.0.0', port))
                except OSError as error:
                    raise InstallError(f'Порт {port} занят. Освободите его перед установкой.') from error
    print('В firewall VPS должны быть открыты TCP 80, 443, 3478 и UDP 3478, 55000–55039.')
    print('Проверка локальных портов не подтверждает доступность через firewall провайдера.')


def install_controller(root, source):
    # Maintenance code must survive a failed activation of the server release.
    controller = root / 'manager.py'
    atomic_write(controller, source.read_text(), 0o700)
    launcher = '#!/bin/sh\nexec python3 ' + shlex.quote(str(controller)) + ' --root ' + shlex.quote(str(root)) + ' "$@"\n'
    atomic_write(COMMAND_PATH, launcher, 0o755)


def prepare_managed_upgrade(candidate):
    """Called by the new deployment script even when its parent CLI is legacy."""
    candidate = candidate.resolve()
    settings = candidate / '.env'
    if not settings.is_symlink():
        return  # Source checkout, not a managed release installation.
    root = settings.resolve().parent
    if candidate.parent != root / 'releases' or not (root / 'installation.json').is_file():
        return
    state = load_state(root)
    state.update(release_protocols(root, candidate, state))
    state['pending_version'] = candidate.name
    save_state(root, state)
    install_controller(root, candidate / 'scripts/familychat.py')


def install_commands(root):
    install_controller(root, root / 'current/scripts/familychat.py')
    atomic_write(Path('/etc/systemd/system/familychat-renew.service'),
                 '[Unit]\nDescription=Renew FamilyChat HTTPS certificate\nAfter=docker.service network-online.target\n'
                 '[Service]\nType=oneshot\nExecStart=/usr/local/bin/familychat renew\n', 0o644)
    atomic_write(Path('/etc/systemd/system/familychat-renew.timer'),
                 '[Unit]\nDescription=Daily FamilyChat certificate renewal\n[Timer]\nOnCalendar=daily\n'
                 'RandomizedDelaySec=3600\nPersistent=true\n[Install]\nWantedBy=timers.target\n', 0o644)
    run(['systemctl', 'daemon-reload'])
    run(['systemctl', 'enable', '--now', 'familychat-renew.timer'])


def bootstrap_admin(root, release, state):
    if state['admin_created']:
        return False
    result = compose(root, release, state, 'exec', '-T', 'postgres', 'psql', '-U', 'familychat',
                     '-d', 'familychat', '-Atc', "SELECT count(*) FROM users WHERE role='admin'",
                     capture_output=True, text=True)
    created = int(result.stdout.strip()) == 0
    if created:
        compose(root, release, state, 'run', '--rm', '--no-deps', 'api', 'bootstrap-admin')
    state['admin_created'] = True
    save_state(root, state)
    return created


PUSH_GATEWAY_KEYS = {'PUSH_GATEWAY_URL', 'PUSH_GATEWAY_TOKEN_FILE'}
DIRECT_PUSH_KEYS = {'APNS_KEY_FILE', 'APNS_KEY_ID', 'APNS_TEAM_ID', 'APNS_TOPIC',
                    'FCM_CREDENTIALS_FILE', 'FCM_PROJECT_ID', 'FCM_APPLICATION_ID'}


def push_gateway_enabled(root):
    return any(line.startswith('PUSH_GATEWAY_URL=') and line.partition('=')[2].strip()
               for line in (root / '.env').read_text().splitlines())


def require_push_gateway_release(release):
    manifest_file = release / 'release.json'
    if manifest_file.is_file():
        manifest = json.loads(manifest_file.read_text())
        if type(manifest.get('push_gateway')) is not int or manifest['push_gateway'] < 1:
            raise InstallError('Сначала обновите сервер до релиза с поддержкой push-шлюза.')
    if not (release / 'compose.push-gateway.yaml').is_file():
        raise InstallError('В релизе отсутствует конфигурация push-шлюза.')


def validate_push_gateway_url(url):
    try:
        parsed = urllib.parse.urlsplit(url)
        if (len(url) > 2048 or parsed.scheme != 'https' or not parsed.hostname
                or not re.fullmatch(r'[A-Za-z0-9.-]+', parsed.hostname)
                or parsed.username is not None or parsed.password is not None
                or parsed.path not in ('', '/') or parsed.query or parsed.fragment
                or any(c in url for c in '\r\n\t $#\\?')
                or parsed.netloc.endswith(':')
                or (parsed.port is not None and not 1 <= parsed.port <= 65535)):
            raise ValueError()
    except ValueError:
        raise InstallError('Адрес push-шлюза должен иметь вид https://push.example.com[:порт], без пути, параметров и логина.')
    return url.rstrip('/')


class NoPushRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise InstallError('Push-шлюз вернул перенаправление. Укажите его конечный HTTPS-адрес.')


def probe_push_gateway(url, token):
    request = urllib.request.Request(url + '/v1/status', headers={'Authorization': 'Bearer ' + token})
    try:
        opener = urllib.request.build_opener(NoPushRedirect())
        with opener.open(request, timeout=10) as response:
            raw = response.read(4097)
            if response.status != 200 or len(raw) > 4096:
                raise ValueError()
        result = json.loads(raw)
        if (not isinstance(result, dict) or type(result.get('protocol')) is not int
                or result['protocol'] != 1 or type(result.get('apns')) is not bool
                or type(result.get('fcm')) is not bool or not (result['apns'] or result['fcm'])):
            raise ValueError()
    except (urllib.error.URLError, OSError, ValueError):
        raise InstallError('Проверка push-шлюза не прошла. Проверьте HTTPS-сертификат, адрес, токен и настройку APNs/FCM на шлюзе.') from None


def restart_push(root, release, state):
    compose(root, release, state, 'up', '-d', '--no-deps', '--force-recreate', '--wait', 'api', 'worker')
    compose(root, release, state, 'exec', '-T', 'nginx', 'nginx', '-s', 'reload')


def configure_push_gateway(root, release, state, url, token_source, *, restart=True):
    require_push_gateway_release(release)
    url = validate_push_gateway_url(url)
    try:
        if not token_source.is_file() or token_source.stat().st_size > 256 or token_source.stat().st_mode & 0o077:
            raise ValueError()
        token = token_source.read_text().strip()
        if not re.fullmatch(r'[A-Za-z0-9_-]{22}\.[A-Za-z0-9_-]{43}', token):
            raise ValueError()
        parts = token.split('.')
        if any(base64.urlsafe_b64encode(base64.urlsafe_b64decode(part + '=' * (-len(part) % 4))).decode().rstrip('=') != part for part in parts):
            raise ValueError()
    except (OSError, ValueError, UnicodeError):
        raise InstallError('Нужен файл токена, выданный push-шлюзом, с правами 0400 или 0600.') from None
    probe_push_gateway(url, token)
    root.chmod(0o700)
    secret_dir = root / 'secrets'; secret_dir.mkdir(exist_ok=True, mode=0o700); secret_dir.chmod(0o700)
    token_name = 'push-gateway-' + hashlib.sha256(token.encode()).hexdigest()[:16] + '.token'
    target = secret_dir / token_name
    existed = target.exists()
    previous = (root / '.env').read_text()
    atomic_write(target, token + '\n', 0o400)
    if os.geteuid() == 0:
        os.chown(target, 10001, 10001)
    values = {'PUSH_GATEWAY_URL': url, 'PUSH_GATEWAY_TOKEN_FILE': './secrets/' + token_name}
    lines = [line for line in previous.splitlines() if line.partition('=')[0] not in PUSH_GATEWAY_KEYS | DIRECT_PUSH_KEYS]
    try:
        atomic_write(root / '.env', '\n'.join(lines) + '\n' + ''.join(f'{k}={v}\n' for k,v in values.items()))
        link_state(root, release)
        compose(root, release, state, 'config', '-q')
    except Exception:
        atomic_write(root / '.env', previous)
        if not existed:
            target.unlink(missing_ok=True)
        raise
    # Keep the selected configuration after runtime failures, so rerun/update can recover.
    if restart:
        restart_push(root, release, state)
    print('Push-шлюз настроен. Ключи APNs/FCM на сервере чатов не используются.')
    if any(line.partition('=')[0] in DIRECT_PUSH_KEYS for line in previous.splitlines()):
        print('Прежние файлы APNs/FCM сохранены. После проверки перенесите их с этого сервера в защищённое хранилище.')


def disable_push(root, release, state):
    previous = (root / '.env').read_text()
    lines = [line for line in previous.splitlines() if line.partition('=')[0] not in PUSH_GATEWAY_KEYS | DIRECT_PUSH_KEYS]
    atomic_write(root / '.env', '\n'.join(lines) + '\n')
    try:
        compose(root, release, state, 'config', '-q')
    except Exception:
        atomic_write(root / '.env', previous)
        raise
    restart_push(root, release, state)
    print('Фоновые push-уведомления отключены. Файлы ключей и токенов сохранены.')


def configure_push(root, release, state):
    print('Настройка уведомлений и системных звонков iOS. API и worker будут перезапущены.')
    key_path = Path(prompt('Путь к APNs-ключу Apple (.p8):')).expanduser()
    key_id = prompt('Key ID (10 символов):')
    team_id = prompt('Team ID (10 символов):')
    topic = prompt('Bundle ID [com.familychat.FamilyChatSelfhosted]:') or 'com.familychat.FamilyChatSelfhosted'
    mode = prompt('Сборка: development (Xcode) или production (TestFlight/App Store):').lower()
    if not re.fullmatch(r'[A-Z0-9]{10}', key_id) or not re.fullmatch(r'[A-Z0-9]{10}', team_id):
        raise InstallError('Key ID и Team ID должны содержать 10 латинских заглавных букв или цифр.')
    if not re.fullmatch(r'[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+', topic) or len(topic) > 255:
        raise InstallError('Некорректный Bundle ID.')
    if mode not in ('development', 'production'):
        raise InstallError('Укажите development или production.')
    with key_path.open('rb') as source:
        data = source.read(16385)
    if len(data) > 16384 or not data.startswith(b'-----BEGIN PRIVATE KEY-----'):
        raise InstallError('Нужен незашифрованный APNs-ключ Apple в формате .p8 (PKCS#8).')
    # Validate the exact bytes we will install, without printing private material.
    checked = run(['openssl', 'pkey', '-pubout', '-outform', 'DER'], input=data, capture_output=True).stdout
    p256_prefix = bytes.fromhex('3059301306072a8648ce3d020106082a8648ce3d03010703420004')
    if len(checked) != 91 or not checked.startswith(p256_prefix):
        raise InstallError('APNs требует приватный ключ EC P-256.')
    key_name = 'apns-' + hashlib.sha256(data).hexdigest()[:16] + '.p8'
    secret_dir = root / 'secrets'
    secret_dir.mkdir(exist_ok=True, mode=0o700)
    secret_dir.chmod(0o700)
    # Compose bind mounts retain host mode. Only root can traverse secrets on the host;
    # the non-root worker must be able to read the single mounted file in its container.
    atomic_write(secret_dir / key_name, data.decode('ascii'), 0o644)
    values = {'APNS_KEY_FILE': './secrets/' + key_name, 'APNS_KEY_ID': key_id,
              'APNS_TEAM_ID': team_id, 'APNS_TOPIC': topic,
              'APNS_SANDBOX': 'true' if mode == 'development' else 'false'}
    env_file = root / '.env'
    previous = env_file.read_text()
    lines = [line for line in previous.splitlines() if line.partition('=')[0] not in set(values) | PUSH_GATEWAY_KEYS]
    configured = '\n'.join(lines) + '\n' + ''.join(f'{key}={value}\n' for key, value in values.items())
    atomic_write(env_file, configured)
    try:
        compose(root, release, state, 'config', '-q')
    except Exception:
        atomic_write(env_file, previous)
        raise
    # Retain the selected configuration if Docker fails; rerunning this command is safe.
    compose(root, release, state, 'up', '-d', '--no-deps', '--force-recreate', '--wait', 'api', 'worker')
    # nginx resolves the API address at startup; a recreated container may get a new IP.
    compose(root, release, state, 'exec', '-T', 'nginx', 'nginx', '-s', 'reload')
    print('APNs настроен. Откройте обновлённое приложение и разрешите уведомления и микрофон.')


def main(argv=None):
    parser = argparse.ArgumentParser(description='Установка и обслуживание FamilyChat')
    parser.add_argument('--root', type=Path, default=DEFAULT_ROOT, help=argparse.SUPPRESS)
    parser.add_argument('command', choices=('install', 'update', 'status', 'logs', 'renew', 'configure-push', 'configure-key-server'))
    parser.add_argument('--version', help='Конкретный релиз vX.Y.Z (по умолчанию последний)')
    parser.add_argument('--key-service-config', type=Path, help='main-server.json, экспортированный отдельным сервером ключей')
    parser.add_argument('--push-gateway-url', help='HTTPS-адрес push-шлюза')
    parser.add_argument('--push-gateway-token-file', type=Path, help='Файл индивидуального токена push-шлюза (0400/0600)')
    parser.add_argument('--push-mode', choices=('gateway', 'direct', 'off'), help='Режим команды configure-push')
    args = parser.parse_args(argv)
    if args.key_service_config is not None and args.command not in ('install', 'update', 'configure-key-server'):
        parser.error('--key-service-config используется только с install, update или configure-key-server')
    if bool(args.push_gateway_url) != bool(args.push_gateway_token_file):
        parser.error('--push-gateway-url и --push-gateway-token-file задаются вместе')
    if args.push_gateway_url and args.command not in ('install', 'update', 'configure-push'):
        parser.error('Параметры push-шлюза используются с install, update или configure-push')
    if args.push_mode and (args.command != 'configure-push' or (args.push_gateway_url and args.push_mode != 'gateway')):
        parser.error('--push-mode используется с configure-push; адрес шлюза совместим только с gateway')
    distro, codename = require_system()
    root = args.root.resolve()
    root.mkdir(parents=True, exist_ok=True, mode=0o700)
    with (root / '.lock').open('w') as lock:
        try:
            if args.command not in ('status', 'logs'):
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise InstallError('Другая команда FamilyChat уже выполняется.')
        fresh_install = False
        if args.command == 'install':
            ensure_docker(distro, codename)
            if (root / 'installation.json').exists():
                state = load_state(root)
                print('Найдена установка. Настройки и пароли будут сохранены.')
            else:
                existing = run(['docker', 'ps', '-a', '--filter', 'label=com.docker.compose.project=familychat', '-q'], capture_output=True, text=True)
                volumes = run(['docker', 'volume', 'ls', '--filter', 'label=com.docker.compose.project=familychat', '-q'], capture_output=True, text=True)
                if existing.stdout.strip() or volumes.stdout.strip():
                    raise InstallError('Найдены существующие контейнеры/тома FamilyChat без настроек установщика. Нужен перенос текущей установки.')
                domain = prompt('Домен сервера (например chat.example.com):').lower()
                email = prompt('Email для HTTPS-сертификата:')
                public_ip = download('https://api.ipify.org', 128).decode().strip()
                validate_input(domain, email, public_ip)
                check_network(domain, public_ip, fresh=True)
                state = configure(root, domain, email, public_ip)
                fresh_install = True
        else:
            if not (root / 'installation.json').exists():
                raise InstallError('Сначала выполните установку.')
            state = load_state(root)
        if args.command in ('install', 'update'):
            if args.command == 'update':
                print('Обновление применяет миграции БД. Сохраните согласованную резервную копию PostgreSQL, файлов и каталога secrets (для v2.0.0 также тома encryption_keys).')
                if prompt('Резервная копия готова, продолжить обновление? [y/N]').lower() not in ('y', 'yes', 'д', 'да'):
                    raise InstallError('Обновление отменено.')
            check_network(state['domain'], state['public_ip'], fresh=False)
            current = root / 'current'
            if state.get('pending_version'):
                if args.command == 'update' and args.version and args.version != state['pending_version']:
                    raise InstallError('Сначала завершите прерванное обновление без --version.')
                release = root / 'releases' / state['pending_version']
                print('Продолжение прерванной установки: ' + state['pending_version'])
            elif args.command == 'install' and current.exists():
                release = current.resolve()
            else:
                version = args.version or latest_version(state['repository'])
                if not VERSION.fullmatch(version):
                    raise InstallError('Ожидается версия vX.Y.Z.')
                release = root / 'releases' / version
                if not release.exists():
                    fetch_bundle(state['repository'], version, release)
            if args.key_service_config is not None:
                require_device_grant_release(release)
                configure_key_service(root, args.key_service_config.resolve())
            gateway_url = args.push_gateway_url
            gateway_token = args.push_gateway_token_file
            if fresh_install and not gateway_url:
                gateway_url = prompt('HTTPS-адрес push-шлюза (Enter — настроить позже):')
                if gateway_url:
                    gateway_token = Path(prompt('Путь к файлу токена push-шлюза:')).expanduser()
            if gateway_url:
                configure_push_gateway(root, release, state, gateway_url, gateway_token.resolve(), restart=False)
            activate(root, release, state)
            created = bootstrap_admin(root, release, state)
            install_commands(root)
            print(f"Готово: https://{state['domain']}/admin/\nЛогин: admin")
            if created:
                print('Первый пароль: ' + (root / 'secrets/admin-password.txt').read_text().strip())
            else:
                print('Пароль администратора не изменён.')
        else:
            release = (root / 'current').resolve()
            if not (release / 'release.json').is_file():
                raise InstallError('Установка ещё не завершена. Повторите установочную команду.')
            if args.command == 'configure-key-server':
                require_device_grant_release(release)
                source = args.key_service_config or Path(prompt('Путь к main-server.json с сервера ключей:')).expanduser()
                configure_key_service(root, source.resolve())
                state.update(release_protocols(root, release, state))
                save_state(root, state)
                compose(root, release, state, 'config', '-q')
                compose(root, release, state, 'up', '-d', '--no-deps', '--force-recreate', '--wait', 'api')
                compose(root, release, state, 'exec', '-T', 'nginx', 'nginx', '-s', 'reload')
                print('Подключён отдельный сервер ключей. API регистрирует устройства и не получает приватный ключ переписки.')
                if (root / 'secrets/content-private-key.txt').exists():
                    print('Прежние файлы ключей сохранены как резервная копия. После проверки переноса уберите их с этого хоста по инструкции.')
                return
            if args.command == 'status':
                if state.get('pending_version'):
                    print('Не завершена активация релиза: ' + state['pending_version'])
                print('Версия: ' + json.loads((release / 'release.json').read_text())['version'])
                compose(root, release, state, 'ps')
            elif args.command == 'configure-push':
                mode = 'gateway' if args.push_gateway_url else args.push_mode
                if mode is None:
                    mode = prompt('Push: gateway — отдельный шлюз, direct — APNs на этом сервере, off — отключить [gateway]:').lower() or 'gateway'
                if mode == 'gateway':
                    url = args.push_gateway_url or prompt('HTTPS-адрес push-шлюза:')
                    token_file = args.push_gateway_token_file or Path(prompt('Путь к файлу токена push-шлюза:')).expanduser()
                    configure_push_gateway(root, release, state, url, token_file.resolve())
                elif mode == 'direct':
                    configure_push(root, release, state)
                elif mode == 'off':
                    disable_push(root, release, state)
                else:
                    raise InstallError('Выберите gateway, direct или off.')
            elif args.command == 'logs':
                compose(root, release, state, 'logs', '--tail', '100', '-f', 'api', 'worker', 'nginx')
            else:
                run(['bash', release / 'scripts/deploy-https.sh', 'renew', '--host', state['domain']], env=environment(root, state))


if __name__ == '__main__':
    try:
        main()
    except (InstallError, OSError, ValueError, KeyError, tarfile.TarError) as error:
        print(f'Ошибка: {error}', file=sys.stderr)
        sys.exit(1)
    except KeyboardInterrupt:
        print('\nОперация прервана. Повторный запуск сохранит настройки.', file=sys.stderr)
        sys.exit(130)
