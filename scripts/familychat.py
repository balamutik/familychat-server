#!/usr/bin/env python3
"""FamilyChat release installer and local maintenance CLI (stdlib only)."""
import argparse
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

REPOSITORY = 'balamutik/familychat-server'
DEFAULT_ROOT = Path('/opt/familychat')
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
        stage.rename(destination)


def environment(root, state):
    # Do not let an unrelated shell's Compose/APNs/DATABASE settings override managed settings.
    env = {key: os.environ[key] for key in ('PATH', 'HOME', 'LANG', 'TERM') if key in os.environ}
    env['COMPOSE_PROJECT_NAME'] = state['project']
    env['HTTPS_ALLOWED_ORIGINS'] = state['domain']
    return env


def compose(root, release, state, *args, **kwargs):
    return run(['docker', 'compose', '--project-name', state['project'], '--env-file', root / '.env',
                '-f', release / 'compose.yaml', '-f', release / 'compose.https.yaml', *args],
               cwd=release, env=environment(root, state), **kwargs)


def link_state(root, release):
    for name in ('.env', 'secrets', '.https'):
        path = release / name
        if not path.is_symlink():
            path.symlink_to(root / name)
        elif path.resolve() != (root / name).resolve():
            raise InstallError(f'Неверная ссылка на настройки: {path}')


def activate(root, candidate, state):
    link_state(root, candidate)
    compose(root, candidate, state, 'config', '-q')
    compose(root, candidate, state, 'pull')
    state['pending_version'] = candidate.name
    save_state(root, state)
    run(['bash', candidate / 'scripts/deploy-https.sh', 'up', '--host', state['domain'],
         '--letsencrypt', '--email', state['email'], '--no-build'], env=environment(root, state))
    compose(root, candidate, state, 'up', '-d', '--no-build', '--wait', '--wait-timeout', '180',
            'api', 'worker', 'turn', 'nginx')
    current = root / 'current'
    pending = root / '.current-next'
    pending.unlink(missing_ok=True)
    pending.symlink_to(candidate)
    os.replace(pending, current)
    state.pop('pending_version', None)
    save_state(root, state)


def prompt(message):
    # Buffered read/write mode requires seek support, which terminals do not have.
    with open('/dev/tty', 'w', encoding='utf-8') as output, open('/dev/tty', 'r', encoding='utf-8') as source:
        output.write(message + ' ')
        output.flush()
        value = source.readline()
    if not value:
        raise InstallError('Ввод прерван.')
    return value.strip()


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


def ensure_docker(distro, codename):
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
    version = run(['docker', 'compose', 'version', '--short'], capture_output=True, text=True).stdout.strip()
    match = re.match(r'v?(\d+)\.(\d+)\.(\d+)', version)
    if not match or tuple(map(int, match.groups())) < (2, 24, 4):
        raise InstallError('Обновите Docker Compose до 2.24.4 или новее.')
    for command in ('curl', 'openssl'):
        if not shutil.which(command):
            run(['apt-get', 'update'])
            run(['apt-get', 'install', '-y', command])


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


def install_commands(root):
    launcher = '#!/bin/sh\nexec python3 ' + shlex.quote(str(root / 'current/scripts/familychat.py')) + ' --root ' + shlex.quote(str(root)) + ' "$@"\n'
    atomic_write(Path('/usr/local/bin/familychat'), launcher, 0o755)
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


def main(argv=None):
    parser = argparse.ArgumentParser(description='Установка и обслуживание FamilyChat')
    parser.add_argument('--root', type=Path, default=DEFAULT_ROOT, help=argparse.SUPPRESS)
    parser.add_argument('command', choices=('install', 'update', 'status', 'logs', 'renew'))
    parser.add_argument('--version', help='Конкретный релиз vX.Y.Z (по умолчанию последний)')
    args = parser.parse_args(argv)
    distro, codename = require_system()
    root = args.root.resolve()
    root.mkdir(parents=True, exist_ok=True, mode=0o700)
    with (root / '.lock').open('w') as lock:
        try:
            if args.command not in ('status', 'logs'):
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise InstallError('Другая команда FamilyChat уже выполняется.')
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
        else:
            if not (root / 'installation.json').exists():
                raise InstallError('Сначала выполните установку.')
            state = load_state(root)
        if args.command in ('install', 'update'):
            if args.command == 'update':
                print('Обновление применяет миграции БД. Сохраните согласованную резервную копию PostgreSQL и файлов.')
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
            if args.command == 'status':
                if state.get('pending_version'):
                    print('Не завершена активация релиза: ' + state['pending_version'])
                print('Версия: ' + json.loads((release / 'release.json').read_text())['version'])
                compose(root, release, state, 'ps')
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
