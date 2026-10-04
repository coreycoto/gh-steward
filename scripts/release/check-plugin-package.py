#!/usr/bin/env python3
"""Check this repository's portable plugin, immediate skills, and local catalog."""

from __future__ import annotations

import json
import re
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
PLUGIN = ROOT / "plugins" / "gh-steward"
MANIFEST_PATH = PLUGIN / "plugin.json"
CATALOG_PATH = ROOT / ".agents" / "plugins" / "marketplace.json"
SCHEMA = "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json"
ALLOWED_MANIFEST_FIELDS = {
    "$schema",
    "name",
    "version",
    "description",
    "author",
    "homepage",
    "repository",
    "license",
    "keywords",
    "extensions",
}
SEMVER = re.compile(r"^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$")


def read_json(path: Path) -> dict:
    value = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(value, dict):
        raise ValueError(f"{path.relative_to(ROOT)} must be a JSON object")
    return value


def check() -> None:
    manifest = read_json(MANIFEST_PATH)
    unknown = set(manifest) - ALLOWED_MANIFEST_FIELDS
    if unknown:
        raise ValueError(f"portable manifest has non-standard fields: {sorted(unknown)}")
    if manifest.get("$schema") != SCHEMA:
        raise ValueError("portable manifest must target Agent Plugins 1.0.0")
    if manifest.get("name") != "gh-steward":
        raise ValueError("portable manifest name must be gh-steward")
    version = manifest.get("version")
    if not isinstance(version, str) or not SEMVER.fullmatch(version):
        raise ValueError("portable manifest version must be semantic version text")
    if not isinstance(manifest.get("description"), str) or not manifest["description"].strip():
        raise ValueError("portable manifest description is required")
    author = manifest.get("author")
    if not isinstance(author, dict) or set(author) - {"name", "email", "url"}:
        raise ValueError("portable manifest author must use the standard author fields")
    extensions = manifest.get("extensions", {})
    if not isinstance(extensions, dict) or any(not isinstance(v, dict) for v in extensions.values()):
        raise ValueError("portable manifest extensions must be namespace-to-object mappings")
    openai = extensions.get("com.openai", {})
    if not isinstance(openai, dict) or not isinstance(openai.get("interface", {}), dict):
        raise ValueError("OpenAI presentation metadata must stay under extensions.com.openai")
    if any(key in openai for key in {"hooks", "mcpServers", "apps"}):
        raise ValueError("the initial tool plugin does not declare OpenAI hooks, apps, or MCP servers")

    skills_root = PLUGIN / "skills"
    skill_dirs = sorted(p for p in skills_root.iterdir() if p.is_dir() and not p.is_symlink())
    if not skill_dirs:
        raise ValueError("portable package must contain an immediate skills/<name>/ directory")
    discovered = set()
    for path in skill_dirs:
        skill_path = path / "SKILL.md"
        if not skill_path.is_file() or skill_path.is_symlink():
            raise ValueError(f"{skill_path.relative_to(ROOT)} is required as a regular file")
        text = skill_path.read_text(encoding="utf-8")
        frontmatter = re.match(r"\A---\n(?P<meta>.*?)\n---\n", text, re.DOTALL)
        if not frontmatter:
            raise ValueError(f"{skill_path.relative_to(ROOT)} requires YAML frontmatter")
        values = dict(
            (match.group(1), match.group(2).strip())
            for match in re.finditer(r"^(name|description):\s*(.+?)\s*$", frontmatter["meta"], re.MULTILINE)
        )
        if values.get("name") != path.name or not values.get("description"):
            raise ValueError(f"{skill_path.relative_to(ROOT)} must declare its directory name and a description")
        if path.name in discovered:
            raise ValueError(f"duplicate skill directory {path.name}")
        discovered.add(path.name)

    catalog = read_json(CATALOG_PATH)
    if catalog.get("name") != "gh-steward-marketplace":
        raise ValueError("local marketplace identifier is unexpected")
    entries = catalog.get("plugins")
    if not isinstance(entries, list) or len(entries) != 1:
        raise ValueError("local marketplace must contain only the gh-steward entry")
    entry = entries[0]
    source = entry.get("source", {}) if isinstance(entry, dict) else {}
    policy = entry.get("policy", {}) if isinstance(entry, dict) else {}
    if (
        entry.get("name") != "gh-steward"
        or source != {"source": "local", "path": "./plugins/gh-steward"}
        or policy.get("installation") != "AVAILABLE"
        or policy.get("authentication") not in {"ON_INSTALL", "ON_USE"}
        or entry.get("category") != "Developer Tools"
    ):
        raise ValueError("local marketplace entry does not resolve the portable package or use supported policy fields")
    print(f"plugin package valid: gh-steward {version}; {len(discovered)} immediate skills")


if __name__ == "__main__":
    try:
        check()
    except (OSError, ValueError, json.JSONDecodeError) as error:
        raise SystemExit(str(error)) from error
