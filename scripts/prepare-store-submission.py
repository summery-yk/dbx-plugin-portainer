#!/usr/bin/env python3
"""Generate DBX Store submission metadata from built unsigned candidates.

Usage:
    python scripts/prepare-store-submission.py --repo OWNER/REPO --tag v0.2.0
    python scripts/prepare-store-submission.py --repo OWNER/REPO --targets windows-x64,darwin-arm64

Inputs (read-only):
    manifest.json
    .dbx-store.json                                    # author-maintained listing metadata
    dist/<id>-<version>-<target>.artifact.json         # one per built platform
    dist/<id>-<version>-<target>.dbxp                  # sha256/size re-verified

Outputs (written):
    .dbx-store.json                                    # source/homepage refreshed for the repo
    release-candidates.json                            # attach to the GitHub Release
    dist/store/candidates/<id>.json                    # manual PR to t8y2/dbx-store:main
    dist/store/REPO-AND-PR.md                          # step-by-step with the given repo

The DBX CLI builds a native Go sidecar and rejects a --target that differs from
the build host ("run this package command on the target platform"), so each
target must be packaged on its own native runner. See .github/workflows/release.yml.
Targets are discovered from dist/: a local single-platform build still works.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import re
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent

DEFAULT_TARGETS = ("windows-x64", "darwin-x64", "darwin-arm64", "linux-x64", "linux-arm64")
TARGET_RE = re.compile(r"^[a-z0-9-]{1,64}$")  # matches dbx-store validate.mjs


def load_json(path: Path):
    return json.loads(path.read_text(encoding="utf-8"))


def dump_json(path: Path, value) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, ensure_ascii=False, indent=2) + "\n", encoding="utf-8", newline="\n")
    print("wrote", path.relative_to(ROOT))


def parse_targets(value: str | None) -> list[str]:
    if not value or value.strip().lower() in ("auto", "all"):
        return list(DEFAULT_TARGETS)
    targets = [item.strip() for item in value.split(",") if item.strip()]
    for target in targets:
        if not TARGET_RE.fullmatch(target):
            raise SystemExit(f"invalid --targets entry: {target!r}")
    if len(set(targets)) != len(targets):
        raise SystemExit("duplicate targets in --targets")
    return targets


def discover_artifacts(plugin_id: str, version: str, targets: list[str]) -> list[dict]:
    found: list[dict] = []
    for target in targets:
        artifact_path = ROOT / "dist" / f"{plugin_id}-{version}-{target}.artifact.json"
        if not artifact_path.exists():
            print(f"skip   {target}: no artifact metadata in dist/")
            continue
        artifact = load_json(artifact_path)
        if artifact.get("target") != target:
            raise SystemExit(
                f"unexpected target in {artifact_path.name}: {artifact.get('target')!r} != {target!r}"
            )
        package = ROOT / "dist" / artifact["url"]
        if not package.exists():
            raise SystemExit(f"missing package for {target}: {package}")
        actual = hashlib.sha256(package.read_bytes()).hexdigest()
        if actual != artifact["sha256"]:
            raise SystemExit(f"artifact sha mismatch for {target}: {actual} != {artifact['sha256']}")
        if package.stat().st_size != artifact["size"]:
            raise SystemExit(f"artifact size mismatch for {target}")
        print(f"verified {package.name}: sha256={actual[:16]}... size={artifact['size']}")
        found.append(artifact)
    if not found:
        raise SystemExit(
            "No artifacts found in dist/. Build at least one target first, e.g. "
            "`dbx-plugin package . --target windows-x64` on a matching native host."
        )
    found.sort(key=lambda item: item["target"])
    return found


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--repo", required=True, help="GitHub repository slug, e.g. summery-yk/dbx-plugin-portainer")
    ap.add_argument("--tag", default=None, help="Source tag / release tag (default: v<manifest version>)")
    ap.add_argument("--license", default=None, help="SPDX license id (default: from .dbx-store.json, else Apache-2.0)")
    ap.add_argument("--release-notes", default=None, help="Override release notes")
    ap.add_argument(
        "--targets",
        default=None,
        help="Comma-separated targets to include (default: auto-discover all built targets in dist/)",
    )
    args = ap.parse_args()

    repo = args.repo.strip().strip("/")
    assert re.fullmatch(r"[A-Za-z0-9._-]+/[A-Za-z0-9._-]+", repo), f"invalid --repo: {args.repo!r}"

    manifest = load_json(ROOT / "manifest.json")
    version = manifest["version"]
    plugin_id = manifest["id"]
    tag = args.tag or f"v{version}"
    artifacts = discover_artifacts(plugin_id, version, parse_targets(args.targets))
    candidate_dir = ROOT / "dist" / "store"

    source = f"https://github.com/{repo}/tree/{tag}"
    homepage = f"https://github.com/{repo}"

    # Inherit the author-maintained listing fields, refresh identity-derived ones.
    existing = load_json(ROOT / ".dbx-store.json") if (ROOT / ".dbx-store.json").exists() else {}
    store_meta = dict(existing)
    store_meta.update(
        {
            "name": manifest["name"],
            "description": manifest["description"],
            "icon": manifest["icon"],
            "permissions": manifest["permissions"],
            "source": source,
            "homepage": homepage,
            "license": args.license or existing.get("license") or "Apache-2.0",
        }
    )
    if args.release_notes:
        store_meta["releaseNotes"] = args.release_notes
    store_meta.setdefault("releaseNotes", f"{version}: see CHANGELOG.md for details.")
    if "localizations" in manifest:
        store_meta["localizations"] = manifest["localizations"]
    if "tags" not in store_meta:
        store_meta["tags"] = ["portainer", "docker", "container", "mcp"]
    dump_json(ROOT / ".dbx-store.json", store_meta)

    # release-candidates.json — read by dbx-store/scripts/sync-release-candidate.mjs.
    release_candidates = {
        "plugin": {
            "id": plugin_id,
            "publisher": manifest["publisher"],
            "version": version,
            "name": manifest["name"],
            "description": manifest["description"],
            "permissions": manifest["permissions"],
        },
        "artifacts": [
            {
                "target": artifact["target"],
                "url": artifact["url"],
                "sha256": artifact["sha256"],
                "size": artifact["size"],
            }
            for artifact in artifacts
        ],
    }
    dump_json(ROOT / "release-candidates.json", release_candidates)

    candidate = {
        "schemaVersion": 1,
        "id": plugin_id,
        "publisher": manifest["publisher"],
        "version": version,
        **store_meta,
        "icon": f"https://raw.githubusercontent.com/{repo}/{tag}/{manifest['icon']}",
        "targets": [
            {
                "target": artifact["target"],
                "url": f"https://github.com/{repo}/releases/download/{tag}/{artifact['url']}",
                "sha256": artifact["sha256"],
                "size": artifact["size"],
            }
            for artifact in artifacts
        ],
    }
    dump_json(candidate_dir / "candidates" / f"{plugin_id}.json", candidate)

    asset_rows = "\n".join(
        f"| `{a['url']}` | `{a['target']}` | `{a['sha256']}` | {a['size']} |" for a in artifacts
    )
    asset_list = "\n".join(f"{i}. `dist/{a['url']}`" for i, a in enumerate(artifacts, 1))
    steps = f"""# GitHub 仓库与 dbx-store PR（{repo} @ {tag}）

## A. 作者仓库 Release（审核输入）

仓库：`{homepage}`，源码 Tag：`{tag}`（与 manifest version `{version}` 一致）。

Release 资产（{len(artifacts) + 2} 项）：

{asset_list}
{len(artifacts) + 1}. 仓库根目录 `release-candidates.json`
{len(artifacts) + 2}. Release Notes

| 包 | target | SHA-256 | size |
|---|---|---|---|
{asset_rows}

多平台产物由 `.github/workflows/release.yml` 在各目标平台的**原生 runner** 上构建
（DBX CLI 拒绝 host 与 `--target` 不一致的带后端打包）。打 `v*` tag 或手动触发工作流即可。

**商店不从 tag 源码打包**，同步器下载 Release 中的未签名 `.dbxp` 并按上表 SHA-256 与
大小校验；`source` tag 仅用于审核溯源。

## B. 向 dbx-store 提交 PR

- PR 目标：`t8y2/dbx-store:main`
- 首次上架：fork 后提交 `candidates/{plugin_id}.json`（已生成于 `dist/store/candidates/`）
- 不要提交 `.dbxp`、源码树、私钥或自签的 `verified: true`
- CI 在维护者 `/sign` 前故意为红（`open candidate(s) awaiting DBX Store signing`），属预期
- 维护者审核后写回 `plugins/{plugin_id}.json` / `catalog/index.json`，检查转绿后合并

## C. 自动同步（可选，建议同时申请）

向 `t8y2/dbx-store` 的 `automation/plugin-sources.json` 追加：

```json
{{ "repository": "{repo}", "metadataPath": ".dbx-store.json", "autoUpdate": true }}
```

注册后只需打新 tag + 发 Release，同步器自动创建/更新候选 PR，全部平台 target 随
`release-candidates.json` 的 `artifacts` 一起同步。

## D. 校验命令

```bash
node tools/verify-manifest.mjs
```
"""
    out = candidate_dir / "REPO-AND-PR.md"
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(steps, encoding="utf-8", newline="\n")
    print("wrote", out.relative_to(ROOT))
    print(f"\nPrepared {len(artifacts)} target(s): {', '.join(a['target'] for a in artifacts)}")


if __name__ == "__main__":
    main()
