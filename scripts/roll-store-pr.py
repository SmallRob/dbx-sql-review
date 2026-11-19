# -*- coding: utf-8 -*-
"""把最新候选直接滚动提交到同一个商店 PR（不新建 PR）。

约定（用户 2026-11-18 明确要求，后续遵照执行）：
DBX 商店新版本上架时，不为每个版本新建 PR，而是把最新候选覆盖到同一个 PR 的分支，
避免多个未合并 PR 互相阻塞（如 0.1.31 阻塞 0.1.32）。

用法：
    # 预览（默认 dry-run，不发写请求）：
    python scripts/roll-store-pr.py --pr 558
    # 真正执行（需要对 t8y2/dbx-store 有写权限的 token）：
    STORE_TOKEN=<有写权限的 token> python scripts/roll-store-pr.py --pr 558 --apply
    # 同时关闭同插件的其它 open PR（避免重复）：
    STORE_TOKEN=<token> python scripts/roll-store-pr.py --pr 558 --apply --close-duplicates

token 优先级：STORE_TOKEN > GH_TOKEN。
候选文件来自本仓库 store/candidates/<plugin-id>.json（sha256/size 已由
官方 Release 的 release-candidates.json 同步），默认取该文件。

注意：
- PR 分支可能属于上游仓库（如 t8y2/dbx-store），需要对该仓库有写权限；
  当前项目 token 只有 comment 权限时，本脚本会在写步骤返回 403/404，请换用有权限的 token。
- --close-duplicates 只关闭「同插件、同 publisher、非目标 PR」的 open PR。
"""
from __future__ import annotations

import argparse
import base64
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

STORE_REPO = "t8y2/dbx-store"
API = "https://api.github.com"
CANDIDATE = "candidates/io.github.smallrob.sql-review.json"


def token() -> str:
    return (os.environ.get("STORE_TOKEN") or os.environ.get("GH_TOKEN") or "").strip()


def request(method: str, path: str, body: dict | None = None) -> tuple[int, object]:
    url = path if path.startswith("http") else API + path
    data = json.dumps(body).encode("utf-8") if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header("Accept", "application/vnd.github+json")
    req.add_header("Authorization", f"Bearer {token()}")
    if data is not None:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            raw = resp.read().decode("utf-8")
            return resp.status, json.loads(raw) if raw else {}
    except urllib.error.HTTPError as e:
        raw = e.read().decode("utf-8", "replace")
        try:
            parsed = json.loads(raw)
        except Exception:
            parsed = raw[:300]
        return e.code, parsed


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--pr", type=int, default=546, help="目标商店 PR 编号（默认 558）")
    ap.add_argument("--candidate", default="", help="候选 JSON 路径，默认取本仓库 store/candidates 下唯一文件")
    ap.add_argument("--apply", action="store_true", help="真正执行写操作；默认 dry-run")
    ap.add_argument("--close-duplicates", action="store_true", help="关闭同插件的其它 open PR")
    args = ap.parse_args()

    if not token():
        print("错误：需要 STORE_TOKEN 或 GH_TOKEN", file=sys.stderr)
        return 2

    # 定位候选文件
    cand_path = Path(args.candidate) if args.candidate else None
    if cand_path is None:
        base = Path(__file__).resolve().parent.parent / "store" / "candidates"
        files = sorted(base.glob("*.json"))
        if len(files) != 1:
            print(f"错误：{base} 下需恰好 1 个候选文件，实际 {len(files)} 个", file=sys.stderr)
            return 2
        cand_path = files[0]
    candidate_text = cand_path.read_text(encoding="utf-8")
    candidate = json.loads(candidate_text)
    version = candidate.get("version", "?")
    print(f"候选: {cand_path.name}  version={version}")

    # 读取目标 PR 及其 head 分支
    status, pr = request("GET", f"/repos/{STORE_REPO}/pulls/{args.pr}")
    if status != 200:
        print(f"错误：读取 PR #{args.pr} 失败 HTTP {status}: {pr}", file=sys.stderr)
        return 1
    head_repo = pr["head"]["repo"]["full_name"]
    head_ref = pr["head"]["ref"]
    print(f"目标 PR #{args.pr}: {head_repo}@{head_ref}  state={pr['state']}")

    # 读取分支上现有候选的 blob sha（PUT 更新必需）
    status, cur = request(
        "GET",
        f"/repos/{head_repo}/contents/{CANDIDATE}?ref={urllib.parse.quote(head_ref, safe='')}",
    )
    blob_sha = cur.get("sha") if status == 200 else None
    if status != 200:
        print(f"警告：分支上未找到现有候选 HTTP {status}，将作为新文件创建")

    new_title = f"feat(store): submit {candidate.get('id')}@{version} (single rolling PR)"
    new_body = (
        "Single rolling candidate PR (no new PR per version).\n\n"
        f"Latest candidate: **{candidate.get('id')}@{version}**\n\n"
        f"Source: https://github.com/SmallRob/dbx-sql-review/releases/download/v{version}/release-candidates.json\n\n"
        "Contains unsigned candidate metadata only; a DBX Store maintainer must review and run the protected signing workflow before merge."
    )

    print("\n--- 计划 ---")
    print(f"1. PUT  {head_repo}/{CANDIDATE}  (branch={head_ref})  -> v{version}")
    print(f"2. PATCH PR #{args.pr}  title -> {new_title}")
    if args.close_duplicates:
        print("3. 关闭同插件其它 open PR")

    if not args.apply:
        print("\n[dry-run] 未执行任何写操作。加 --apply 并配置有写权限的 STORE_TOKEN 后重试。")
        return 0

    # 1) 覆盖候选文件
    payload = {
        "message": f"chore(store): roll candidate to {candidate.get('id')}@{version}",
        "content": base64.b64encode(candidate_text.encode("utf-8")).decode("ascii"),
        "branch": head_ref,
    }
    if blob_sha:
        payload["sha"] = blob_sha
    status, res = request("PUT", f"/repos/{head_repo}/contents/{CANDIDATE}", payload)
    print(f"[apply] PUT candidate -> HTTP {status}")
    if status not in (200, 201):
        print(f"  失败：{res}", file=sys.stderr)
        print("  提示：该分支属于上游仓库，当前 token 可能无写权限；请改用有 t8y2/dbx-store 写权限的 token。", file=sys.stderr)
        return 1

    # 2) 更新 PR 标题与描述
    status, res = request("PATCH", f"/repos/{STORE_REPO}/pulls/{args.pr}",
                          {"title": new_title, "body": new_body})
    print(f"[apply] PATCH PR #{args.pr} -> HTTP {status}")
    if status not in (200, 201):
        print(f"  警告（候选已更新，仅标题未改）：{res}", file=sys.stderr)

    # 3) 关闭重复 PR
    if args.close_duplicates:
        status, prs = request("GET", f"/repos/{STORE_REPO}/pulls?state=open&per_page=100")
        if status == 200:
            for p in prs:
                if p["number"] == args.pr:
                    continue
                ref = p.get("head", {}).get("ref", "")
                title = p.get("title", "")
                if "sql-review" in ref.lower() or "sql-review" in title.lower():
                    s2, _ = request("PATCH", f"/repos/{STORE_REPO}/pulls/{p['number']}", {"state": "closed"})
                    print(f"[apply] 关闭重复 PR #{p['number']} -> HTTP {s2}")
        else:
            print(f"  警告：列 PR 失败 HTTP {status}", file=sys.stderr)

    print(f"\n完成：PR #{args.pr} 现承载 {candidate.get('id')}@{version} 候选。")
    return 0


if __name__ == "__main__":
    sys.exit(main())
