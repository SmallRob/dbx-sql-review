# -*- coding: utf-8 -*-
"""按官方 .dbxp 格式组装未签名候选包，并生成市场所需的候选元数据。

用途：官方 CLI（`dbx-plugin package .`）只为**当前 runner 平台**构建 sidecar，
因此在同一台机器上无法一次性产出 5 个平台的候选。本脚本用 Go 交叉编译补齐，
产出的包结构与官方 CLI 完全一致：

    manifest.json                              entrypoints.backend.executable 按 target 重写
    bin/<target>/dbx-plugin-sql-review[.exe]   0755
    assets/*
    ui/*
    checksums.json                              对上述文件逐个 sha256

本脚本只做本地验证与候选清单生成；**正式发布请走官方 CI**
（见 .github/workflows/plugin-release.yml），签名由 DBX Store 完成。

用法：
    python scripts/build-candidates.py            # 构建 5 个平台 + 生成候选元数据
    python scripts/build-candidates.py --target linux-x64
"""
from __future__ import annotations

import argparse
import hashlib
import json
import shutil
import subprocess
import sys
import zipfile
from pathlib import Path

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent
DIST = ROOT / "dist"
CROSS = ROOT / ".crossbuild"

# 与官方 reusable workflow 的 build-matrix 保持一致。
TARGETS = {
    "linux-x64": ("linux", "amd64", ""),
    "linux-arm64": ("linux", "arm64", ""),
    "darwin-x64": ("darwin", "amd64", ""),
    "darwin-arm64": ("darwin", "arm64", ""),
    "windows-x64": ("windows", "amd64", ".exe"),
}

# 仓库源码地址（owner/repo）。插件 ID、publisher、图标与产物 URL 都由它派生，
# 避免多处硬编码后各自漂移。
REPOSITORY = "SmallRob/dbx-sql-review"


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_file(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def run(cmd: list[str], cwd: Path | None = None) -> None:
    print("  $ " + " ".join(cmd))
    proc = subprocess.run(cmd, cwd=str(cwd) if cwd else None)
    if proc.returncode != 0:
        raise SystemExit("命令失败: %s" % " ".join(cmd))


def build_binary(target: str) -> Path:
    goos, goarch, ext = TARGETS[target]
    CROSS.mkdir(parents=True, exist_ok=True)
    out = CROSS / ("dbx-plugin-sql-review-%s-%s%s" % (goos, goarch, ext))
    env = {
        "GOOS": goos,
        "GOARCH": goarch,
        # SDK 随仓库自带，构建不依赖外网。
        "GOPROXY": "off",
        "GOFLAGS": "-mod=mod",
    }
    import os

    merged = dict(os.environ)
    merged.update(env)
    print("[build] %s" % target)
    subprocess.run(
        ["go", "build", "-o", str(out), "."],
        cwd=str(ROOT / "backend"),
        env=merged,
        check=True,
    )
    return out


def pack(target: str, binary: Path) -> Path:
    manifest = json.loads((ROOT / "manifest.json").read_text(encoding="utf-8"))
    pid, version = manifest["id"], manifest["version"]

    binary_rel = "bin/%s/dbx-plugin-sql-review%s" % (target, binary.suffix)

    # 官方 CLI 会按目标平台重写 sidecar 路径；这里必须保持一致，
    # 否则包内文件在 bin/<target>/ 而 manifest 指向 bin/…，宿主根本找不到二进制。
    manifest["entrypoints"]["backend"]["executable"] = binary_rel
    manifest_blob = (json.dumps(manifest, ensure_ascii=False, indent=2) + "\n").encode("utf-8")

    files: list[tuple[str, bytes]] = [("manifest.json", manifest_blob)]
    for folder in ("assets", "ui"):
        base = ROOT / folder
        if not base.exists():
            continue
        for path in sorted(base.rglob("*")):
            if path.is_file():
                files.append((path.relative_to(ROOT).as_posix(), path.read_bytes()))
    files.append((binary_rel, binary.read_bytes()))

    checksums = {name: sha256_bytes(data) for name, data in files}
    checksums_blob = json.dumps(
        {"algorithm": "sha256", "files": checksums},
        ensure_ascii=False, indent=2, sort_keys=True,
    ).encode("utf-8")

    DIST.mkdir(parents=True, exist_ok=True)
    out = DIST / ("%s-%s-%s.dbxp" % (pid, version, target))
    tmp = out.with_suffix(".dbxp.part")

    with zipfile.ZipFile(tmp, "w", zipfile.ZIP_DEFLATED, compresslevel=9) as zf:
        for name, data in files:
            info = zipfile.ZipInfo(name, date_time=(2026, 1, 1, 0, 0, 0))
            info.compress_type = zipfile.ZIP_DEFLATED
            mode = 0o755 if name.startswith("bin/") else 0o644
            info.external_attr = (0o100000 | mode) << 16
            zf.writestr(info, data)
        info = zipfile.ZipInfo("checksums.json", date_time=(2026, 1, 1, 0, 0, 0))
        info.compress_type = zipfile.ZIP_DEFLATED
        info.external_attr = (0o100000 | 0o644) << 16
        zf.writestr(info, checksums_blob)

    tmp.replace(out)

    # 自检：包内 manifest 指向的 sidecar 必须真的在包里，版本也必须一致。
    with zipfile.ZipFile(out) as zf:
        names = set(zf.namelist())
        packed_manifest = json.loads(zf.read("manifest.json").decode("utf-8"))
    executable = packed_manifest["entrypoints"]["backend"]["executable"]
    if executable not in names:
        raise SystemExit("%s: manifest 指向的可执行文件不存在: %s" % (out.name, executable))
    if packed_manifest["version"] != version:
        raise SystemExit("%s: 包内版本 %s 与 manifest.json 的 %s 不一致"
                         % (out.name, packed_manifest["version"], version))
    return out


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--target", action="append", choices=sorted(TARGETS),
                    help="只构建指定平台（可重复）；默认全部")
    ap.add_argument("--skip-build", action="store_true",
                    help="跳过编译，直接用 .crossbuild 里已有的二进制打包")
    args = ap.parse_args()

    targets = args.target or list(TARGETS)
    artifacts = []
    for target in targets:
        binary = build_binary(target) if not args.skip_build else None
        if binary is None:
            goos, goarch, ext = TARGETS[target]
            candidate = CROSS / ("dbx-plugin-sql-review-%s-%s%s" % (goos, goarch, ext))
            if not candidate.exists():
                raise SystemExit("缺少已构建的二进制: %s" % candidate)
            binary = candidate
        package = pack(target, binary)
        artifacts.append({
            "target": target,
            "url": package.name,
            "sha256": sha256_file(package),
            "size": package.stat().st_size,
        })
        print("  packed %s (%d bytes)" % (package.name, package.stat().st_size))

    # release-candidates.json —— 官方 reusable workflow 生成的同名文件的等价物。
    manifest = json.loads((ROOT / "manifest.json").read_text(encoding="utf-8"))
    release = {
        "plugin": {
            "id": manifest["id"],
            "name": manifest["name"],
            "description": manifest.get("description", ""),
            "publisher": manifest["publisher"],
            "version": manifest["version"],
            "permissions": sorted(manifest.get("permissions", [])),
        },
        "artifacts": sorted(artifacts, key=lambda a: a["target"]),
    }
    out = DIST / "release-candidates.json"
    out.write_text(json.dumps(release, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print("wrote %s" % out)

    # 候选元数据（市场 PR 用的 candidates/<plugin-id>.json）。
    store = json.loads((ROOT / ".dbx-store.json").read_text(encoding="utf-8"))

    # 图标必须是 HTTP(S) 绝对地址：官方 sync-release-candidate.mjs 会把
    # .dbx-store.json 里的相对图标改写成 tag 固定的 raw URL，这里保持同一规则。
    icon = store["icon"]
    if not icon.startswith("http://") and not icon.startswith("https://"):
        icon = "https://raw.githubusercontent.com/%s/v%s/%s" % (
            REPOSITORY,
            manifest["version"],
            icon.lstrip("/"),
        )

    candidate = {
        "schemaVersion": 1,
        "id": manifest["id"],
        "publisher": manifest["publisher"],
        "version": manifest["version"],
        "name": store["name"],
        "description": store["description"],
        "icon": icon,
        "tags": store["tags"],
        "permissions": sorted(manifest.get("permissions", [])),
        "source": store["source"],
        "homepage": store["homepage"],
        "license": store["license"],
        "releaseNotes": store["releaseNotes"],
        "localizations": store.get("localizations", {}),
        "targets": [
            {
                "target": a["target"],
                "url": "https://github.com/%s/releases/download/v%s/%s"
                       % (REPOSITORY, manifest["version"], a["url"]),
                "sha256": a["sha256"],
                "size": a["size"],
            }
            for a in release["artifacts"]
        ],
    }
    cand_path = ROOT / "store" / "candidates" / ("%s.json" % manifest["id"])
    cand_path.parent.mkdir(parents=True, exist_ok=True)
    cand_path.write_text(json.dumps(candidate, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print("wrote %s" % cand_path)

    # 未签名校验：包里不能出现 signature.json。
    for a in release["artifacts"]:
        package = DIST / a["url"]
        with zipfile.ZipFile(package) as zf:
            names = zf.namelist()
            if "signature.json" in names:
                raise SystemExit("候选包不得包含 signature.json: %s" % package)
            if "manifest.json" not in names:
                raise SystemExit("候选包缺少 manifest.json: %s" % package)
    print("自检通过：所有候选包均未签名，且包含 manifest.json")
    print(
        "提醒：候选元数据里的 sha256 来自本地交叉编译，与官方 CI 产物不一定逐字节相同；"
        "正式上架前必须按 docs/store-submission.md 3.2 用 Release 产物重新同步。"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
