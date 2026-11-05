#!/usr/bin/env python3
"""Build source-path-free desktop packages. Run macOS targets on a Mac."""
import argparse
import os
import pathlib
import plistlib
import shutil
import subprocess
import zipfile
import tempfile

parser = argparse.ArgumentParser()
parser.add_argument('--os', choices=['darwin', 'windows'], required=True)
parser.add_argument('--arch', choices=['arm64', 'amd64'], required=True)
args = parser.parse_args()
root = pathlib.Path(__file__).resolve().parent.parent
work = pathlib.Path(tempfile.mkdtemp(prefix='scout-package-'))
target = work / ('scout-' + args.os + '-' + args.arch)
target.mkdir(parents=True, exist_ok=True)
env = dict(os.environ, GOOS=args.os, GOARCH=args.arch)
if args.os == 'windows':
    env['CGO_ENABLED'] = '0'
    app = target / 'scout.exe'
    cli = target / 'scoutctl.exe'
else:
    env['CGO_ENABLED'] = '1'
    app = target / 'Scout.app' / 'Contents' / 'MacOS' / 'scout'
    cli = target / 'scoutctl'
    app.parent.mkdir(parents=True, exist_ok=True)
    info = dict(CFBundleExecutable='scout', CFBundleIdentifier='com.scout.controller',
                CFBundleName='Scout', CFBundlePackageType='APPL', CFBundleVersion='1',
                CFBundleShortVersionString='1.0', NSHighResolutionCapable=True)
    with (app.parent.parent / 'Info.plist').open('wb') as f:
        plistlib.dump(info, f)
for path, package in [(app, '.'), (cli, './cmd/scoutctl')]:
    subprocess.run(['go', 'build', '-trimpath', '-buildvcs=false', '-o', str(path), package],
                   cwd=root, env=env, check=True)
if args.os == 'darwin':
    subprocess.run(['codesign', '--force', '--sign', '-', '--identifier',
                    'com.scout.controller', str(app.parent.parent.parent)], check=True)
for name in ['README.md', 'AGENT_API.md']:
    shutil.copyfile(root / name, target / name)
# Include all checked-in dependency notices with binary distributions.
for source in [root / 'third_party' / 'goroslib' / 'LICENSE']:
    out = target / 'licenses' / 'goroslib-LICENSE'
    out.parent.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(source, out)
modules = subprocess.check_output(['go', 'list', '-deps', '-f', '{{if .Module}}{{.Module.Path}} {{.Module.Dir}}{{end}}', '.', './cmd/scoutctl'],
                                  cwd=root, env=env, text=True)
for line in set(modules.splitlines()):
    fields = line.split(' ', 1)
    if len(fields) != 2 or not fields[1] or fields[0] == 'github.com/shell-company/scout':
        continue
    folder = pathlib.Path(fields[1])
    for source in folder.glob('LICENSE*'):
        if source.is_file():
            out = target / 'licenses' / (fields[0].replace('/', '_') + '-' + source.name)
            shutil.copyfile(source, out)
goroot = subprocess.check_output(['go', 'env', 'GOROOT'], env=env, text=True).strip()
go_license = next((p for p in [pathlib.Path(goroot) / 'LICENSE', pathlib.Path(goroot).parent / 'LICENSE'] if p.is_file()), None)
if go_license is None:
    raise FileNotFoundError('Go distribution LICENSE was not found beside GOROOT')
shutil.copyfile(go_license, target / 'licenses' / 'Go-LICENSE')
archive = root / 'dist' / (target.name + '.zip')
archive.parent.mkdir(exist_ok=True)
with zipfile.ZipFile(archive, 'w', zipfile.ZIP_DEFLATED) as out:
    for path in sorted(target.rglob('*')):
        if path.is_file():
            out.write(path, path.relative_to(target.parent))
shutil.rmtree(work)
print(archive)
