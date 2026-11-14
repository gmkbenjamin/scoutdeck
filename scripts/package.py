#!/usr/bin/env python3
"""Build source-path-free desktop packages. Run macOS targets on a Mac."""
import sys

sys.dont_write_bytecode = True

import argparse
import pathlib
import platform
import plistlib
import shutil
import subprocess
import tempfile
import zipfile

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
import build


def main() -> None:
    parser = argparse.ArgumentParser(description='Package ScoutDeck.app (macOS) or Windows zip release into dist/.')
    parser.add_argument('--os', choices=['darwin', 'windows'])
    parser.add_argument('--arch', choices=['arm64', 'amd64'])
    parser.add_argument('--out', default='dist', help='output directory (default: dist)')
    parser.add_argument('--no-voice', action='store_true', help='skip building scoutdeck-voice on Apple Silicon macOS')
    args = parser.parse_args()

    target_os, target_arch = build.resolve_target(args.os, args.arch)
    if target_os not in ('darwin', 'windows'):
        raise SystemExit(f'Unsupported package OS {target_os!r}; pass --os darwin or --os windows')
    if target_os == 'darwin' and platform.system().lower() != 'darwin':
        raise SystemExit('macOS packages must be built on a Mac (cgo and codesign required)')

    root = pathlib.Path(__file__).resolve().parent.parent
    out_dir = pathlib.Path(args.out)
    if not out_dir.is_absolute():
        out_dir = root / out_dir

    work = pathlib.Path(tempfile.mkdtemp(prefix='scoutdeck-package-'))
    try:
        target = work / ('scoutdeck-' + target_os + '-' + target_arch)
        target.mkdir(parents=True, exist_ok=True)
        env = build.build_env(target_os, target_arch)

        if target_os == 'windows':
            app = target / 'scoutdeck.exe'
            cli = target / 'scoutdeckctl.exe'
        else:
            app = target / 'ScoutDeck.app' / 'Contents' / 'MacOS' / 'scoutdeck'
            cli = target / 'scoutdeckctl'
            app.parent.mkdir(parents=True, exist_ok=True)
            info = dict(
                CFBundleExecutable='scoutdeck',
                CFBundleIdentifier='com.scoutdeck.controller',
                CFBundleName='ScoutDeck',
                CFBundleDisplayName='ScoutDeck',
                CFBundlePackageType='APPL',
                CFBundleVersion='1',
                CFBundleShortVersionString='1.0',
                CFBundleIconFile='ScoutDeck',
                LSApplicationCategoryType='public.app-category.utilities',
                LSMinimumSystemVersion='12.0',
                NSHighResolutionCapable=True,
                # macOS asks before an app reaches devices on the local network.
                NSLocalNetworkUsageDescription='ScoutDeck connects to your robot on the local network '
                                               'for video, driving and docking.',
                NSSpeechRecognitionUsageDescription='The voice assistant turns what the robot hears into '
                                                    'commands, on this Mac.',
                NSMicrophoneUsageDescription="The voice assistant can listen through this Mac's microphone.",
            )
            resources = app.parent.parent / 'Resources'
            resources.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(root / 'assets' / 'ScoutDeck.icns', resources / 'ScoutDeck.icns')
            with (app.parent.parent / 'Info.plist').open('wb') as f:
                plistlib.dump(info, f)

        build.build_go(root, app, '.', env)
        build.build_go(root, cli, './cmd/scoutdeckctl', env)

        if target_os == 'darwin':
            bundled_cli = app.parent / 'scoutdeckctl'
            shutil.copyfile(cli, bundled_cli)
            bundled_cli.chmod(0o755)
            voice_bin = app.parent / 'scoutdeck-voice'
            built_voice = build.build_voice(root, voice_bin, target_os, target_arch, skip=args.no_voice)

            app_bundle = app.parent.parent.parent
            # Extended attributes (quarantine, provenance) break strict signature checks.
            subprocess.run(['xattr', '-cr', str(app_bundle)], check=True)
            build.sign_darwin(cli, 'com.scoutdeck.controller.scoutdeckctl', target_os)
            build.sign_darwin(bundled_cli, 'com.scoutdeck.controller.scoutdeckctl', target_os)
            if built_voice:
                build.sign_darwin(voice_bin, 'com.scoutdeck.controller.voice', target_os)
            build.sign_darwin(app_bundle, 'com.scoutdeck.controller', target_os)
            subprocess.run(['codesign', '--verify', '--deep', '--strict', str(app_bundle)], check=True)

        for name in ['README.md', 'AGENT_API.md']:
            shutil.copyfile(root / name, target / name)

        # Include all checked-in dependency notices with binary distributions.
        goroslib_notice = target / 'licenses' / 'goroslib-LICENSE'
        goroslib_notice.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(root / 'third_party' / 'goroslib' / 'LICENSE', goroslib_notice)

        modules = subprocess.check_output(
            ['go', 'list', '-deps', '-f', '{{if .Module}}{{.Module.Path}} {{.Module.Dir}}{{end}}', '.', './cmd/scoutdeckctl'],
            cwd=root,
            env=env,
            text=True,
        )
        for line in set(modules.splitlines()):
            fields = line.split(' ', 1)
            if len(fields) != 2 or not fields[1] or fields[0] == 'github.com/gmkbenjamin/scoutdeck':
                continue
            folder = pathlib.Path(fields[1])
            for source in folder.glob('LICENSE*'):
                if source.is_file():
                    dest = target / 'licenses' / (fields[0].replace('/', '_') + '-' + source.name)
                    shutil.copyfile(source, dest)

        goroot = subprocess.check_output(['go', 'env', 'GOROOT'], env=env, text=True).strip()
        go_license = next(
            (p for p in [pathlib.Path(goroot) / 'LICENSE', pathlib.Path(goroot).parent / 'LICENSE'] if p.is_file()),
            None,
        )
        if go_license is None:
            raise FileNotFoundError('Go distribution LICENSE was not found beside GOROOT')
        shutil.copyfile(go_license, target / 'licenses' / 'Go-LICENSE')

        archive = out_dir / (target.name + '.zip')
        archive.parent.mkdir(parents=True, exist_ok=True)
        with zipfile.ZipFile(archive, 'w', zipfile.ZIP_DEFLATED) as zf:
            for path in sorted(target.rglob('*')):
                if path.is_file():
                    zf.write(path, path.relative_to(target.parent))

        if target_os == 'darwin':
            # Leave a ready-to-run bundle for double-clicking (ditto --norsrc keeps signature intact).
            bundle = out_dir / 'ScoutDeck.app'
            shutil.rmtree(bundle, ignore_errors=True)
            subprocess.run(['ditto', '--norsrc', str(target / 'ScoutDeck.app'), str(bundle)], check=True)
            print(bundle)

        print(archive)
    finally:
        shutil.rmtree(work, ignore_errors=True)


if __name__ == '__main__':
    main()
