#!/usr/bin/env python3
"""Build standalone ScoutDeck binaries into dist/ (or --out)."""
import sys

sys.dont_write_bytecode = True

import argparse
import os
import pathlib
import platform
import shutil
import subprocess


def host_target() -> tuple[str, str]:
    """Return normalized (os, arch) matching Go GOOS/GOARCH for this machine."""
    os_map = {'darwin': 'darwin', 'windows': 'windows', 'linux': 'linux'}
    arch_map = {'arm64': 'arm64', 'aarch64': 'arm64', 'amd64': 'amd64', 'x86_64': 'amd64'}
    sys_os = os_map.get(platform.system().lower())
    sys_arch = arch_map.get(platform.machine().lower())
    if not sys_os or not sys_arch:
        raise SystemExit(f'Unsupported host platform: {platform.system()} {platform.machine()}; pass --os and --arch')
    return sys_os, sys_arch


def resolve_target(target_os: str | None, target_arch: str | None) -> tuple[str, str]:
    """Fill omitted --os/--arch flags from the host platform."""
    if target_os and target_arch:
        return target_os, target_arch
    default_os, default_arch = host_target()
    return target_os or default_os, target_arch or default_arch


def build_env(target_os: str, target_arch: str) -> dict[str, str]:
    """Return environment variables for clean Go builds."""
    env = dict(os.environ, GOOS=target_os, GOARCH=target_arch)
    env['CGO_ENABLED'] = '0' if target_os == 'windows' else '1'
    return env


def build_go(root: pathlib.Path, out_path: pathlib.Path, package: str, env: dict[str, str]) -> None:
    """Compile a Go binary without embedding local paths or VCS metadata."""
    out_path.parent.mkdir(parents=True, exist_ok=True)
    subprocess.run(
        ['go', 'build', '-trimpath', '-buildvcs=false', '-o', str(out_path), package],
        cwd=root,
        env=env,
        check=True,
    )


def build_voice(root: pathlib.Path, out_path: pathlib.Path, target_os: str, target_arch: str,
                skip: bool = False) -> bool:
    """Compile scoutdeck-voice on Apple Silicon macOS (requires macOS 26+ SDK)."""
    if skip or target_os != 'darwin' or target_arch != 'arm64' or platform.system().lower() != 'darwin':
        return False
    if shutil.which('swiftc') is None:
        raise SystemExit('swiftc is required for scoutdeck-voice on Apple Silicon macOS (pass --no-voice to skip)')
    out_path.parent.mkdir(parents=True, exist_ok=True)
    try:
        subprocess.run(
            ['swiftc', '-O', '-parse-as-library', '-target', 'arm64-apple-macos26.0',
             str(root / 'voice' / 'main.swift'), '-o', str(out_path)],
            check=True,
        )
    except subprocess.CalledProcessError as exc:
        raise SystemExit(
            'Failed to build scoutdeck-voice (requires macOS 26+ SDK / Xcode; pass --no-voice to skip)'
        ) from exc
    return True


def sign_darwin(path: pathlib.Path, identifier: str, target_os: str) -> None:
    """Apply a distinct ad-hoc code signature on macOS so firewall rules stay stable."""
    if target_os != 'darwin' or platform.system().lower() != 'darwin':
        return
    subprocess.run(['codesign', '--force', '--sign', '-', '--identifier', identifier, str(path)], check=True)


def main() -> None:
    parser = argparse.ArgumentParser(description='Build scoutdeck, scoutdeckctl and (on Apple Silicon macOS) scoutdeck-voice.')
    parser.add_argument('--os', choices=['darwin', 'windows', 'linux'])
    parser.add_argument('--arch', choices=['arm64', 'amd64'])
    parser.add_argument('--out', default='dist', help='output directory (default: dist)')
    parser.add_argument('--no-voice', action='store_true', help='skip building scoutdeck-voice on Apple Silicon macOS')
    args = parser.parse_args()

    target_os, target_arch = resolve_target(args.os, args.arch)
    if target_os == 'darwin' and platform.system().lower() != 'darwin':
        raise SystemExit('macOS targets must be built on a Mac (cgo and codesign required)')

    root = pathlib.Path(__file__).resolve().parent.parent
    out = pathlib.Path(args.out)
    if not out.is_absolute():
        out = root / out
    ext = '.exe' if target_os == 'windows' else ''
    env = build_env(target_os, target_arch)

    app_bin = out / ('scoutdeck' + ext)
    cli_bin = out / ('scoutdeckctl' + ext)
    voice_bin = out / 'scoutdeck-voice'

    build_go(root, app_bin, '.', env)
    build_go(root, cli_bin, './cmd/scoutdeckctl', env)
    built_voice = build_voice(root, voice_bin, target_os, target_arch, skip=args.no_voice)

    sign_darwin(app_bin, 'com.scoutdeck.controller', target_os)
    sign_darwin(cli_bin, 'com.scoutdeck.controller.scoutdeckctl', target_os)
    if built_voice:
        sign_darwin(voice_bin, 'com.scoutdeck.controller.voice', target_os)

    print(app_bin)
    print(cli_bin)
    if built_voice:
        print(voice_bin)


if __name__ == '__main__':
    main()
