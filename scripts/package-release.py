#!/usr/bin/env python3
"""Build a small deployment bundle; no application source or private configuration."""
import argparse
import hashlib
import io
import json
from pathlib import Path
import re
import tarfile

ROOT = Path(__file__).resolve().parents[1]


def package(version, image, output):
    if not re.fullmatch(r'v[0-9]+\.[0-9]+\.[0-9]+', version):
        raise ValueError('version must be vX.Y.Z')
    if not re.fullmatch(r'ghcr\.io/[a-z0-9_.-]+/[a-z0-9_.-]+@sha256:[a-f0-9]{64}', image):
        raise ValueError('image must be an immutable GHCR digest')
    files = {name: (ROOT / name).read_bytes() for name in (
        'compose.https.yaml', 'scripts/deploy-https.sh', 'scripts/familychat.py',
        'deploy/nginx/default.conf.template')}
    compose = (ROOT / 'compose.yaml').read_text()
    if compose.count('    build: .\n') != 2:
        raise ValueError('expected API and worker build declarations')
    files['compose.yaml'] = compose.replace('    build: .\n', f'    image: {image}\n').encode()
    files['release.json'] = (json.dumps({'version': version, 'image': image, 'content_encryption': 1,
                                       'repository': 'balamutik/familychat-server'}, indent=2)+'\n').encode()
    output.mkdir(parents=True, exist_ok=True)
    archive_path = output / 'familychat-server.tar.gz'
    with tarfile.open(archive_path, 'w:gz') as archive:
        for name, data in files.items():
            entry = tarfile.TarInfo(name)
            entry.size = len(data)
            entry.mode = 0o755 if name.startswith('scripts/') else 0o644
            archive.addfile(entry, io.BytesIO(data))
    (output / 'install.sh').write_bytes((ROOT / 'install.sh').read_bytes())
    (output / 'SHA256SUMS').write_text(''.join(
        f'{hashlib.sha256((output/name).read_bytes()).hexdigest()}  {name}\n'
        for name in ('familychat-server.tar.gz', 'install.sh')))


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--version', required=True)
    parser.add_argument('--image', required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    package(args.version, args.image, args.output)
