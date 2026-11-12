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
                CFBundleName='Scout', CFBundleDisplayName='Scout', CFBundlePackageType='APPL',
                CFBundleVersion='1', CFBundleShortVersionString='1.0', CFBundleIconFile='Scout',
                LSApplicationCategoryType='public.app-category.utilities',
                LSMinimumSystemVersion='12.0', NSHighResolutionCapable=True,
                # macOS asks before an app reaches devices on the local network.
                NSLocalNetworkUsageDescription='Scout connects to your robot on the local network '
                                               'for video, driving and docking.',
                NSSpeechRecognitionUsageDescription='The voice assistant turns what the robot hears into '
                                                    'commands, on this Mac.',
                NSMicrophoneUsageDescription='The voice assistant can listen through this Mac\'s microphone.')
    resources = app.parent.parent / 'Resources'
    resources.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(root / 'assets' / 'Scout.icns', resources / 'Scout.icns')
    with (app.parent.parent / 'Info.plist').open('wb') as f:
        plistlib.dump(info, f)
for path, package in [(app, '.'), (cli, './cmd/scoutctl')]:
    subprocess.run(['go', 'build', '-trimpath', '-buildvcs=false', '-o', str(path), package],
                   cwd=root, env=env, check=True)
if args.os == 'darwin' and args.arch == 'arm64':
    # On-device voice assistant (Apple Intelligence needs Apple silicon and macOS 26+).
    subprocess.run(['swiftc', '-O', '-parse-as-library', '-target', 'arm64-apple-macos26.0',
                    str(root / 'voice' / 'main.swift'), '-o', str(app.parent / 'scout-voice')], check=True)
if args.os == 'darwin':
    # Extended attributes (quarantine, provenance) break strict signature checks.
    subprocess.run(['xattr', '-cr', str(app.parent.parent.parent)], check=True)
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
if args.os == 'darwin':
    # Also leave a ready-to-run bundle for double-clicking (ditto keeps the signature intact).
    bundle = root / 'dist' / 'Scout.app'
    shutil.rmtree(bundle, ignore_errors=True)
    subprocess.run(['ditto', str(target / 'Scout.app'), str(bundle)], check=True)
    print(bundle)
shutil.rmtree(work)
print(archive)
