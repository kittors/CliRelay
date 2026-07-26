#!/usr/bin/env python3
"""Lightweight backend structure guard for architecture refactors.

The guard intentionally uses only Python's standard library so it can run in
local shells and GitHub Actions without installing project-specific tooling.
"""

from __future__ import annotations

import json
import re
import sys
from pathlib import Path
from typing import Any


WARNING_LINE_THRESHOLD = 800
BLOCKING_LINE_THRESHOLD = 1200

ALLOWLIST_PATH = Path("docs/internal-review/backend-structure-allowlist.json")
MANAGEMENT_DIR = Path("internal/api/handlers/management")
SERVER_PATH = Path("internal/api/server.go")

MANAGEMENT_PERSISTENCE_SYMBOLS = (
    "config.SaveConfigPreserveComments",
    "config.SaveConfigPreserveCommentsUpdateNestedScalar",
    "settingsstore.UpsertRuntimeSetting",
    "settingsstore.PersistRuntimeSettingsFromConfig",
    "usage.CleanDBBackedConfigFromYAML",
    "settingsstore.MigrateRuntimeSettingsFromConfig",
    "settingsstore.ApplyStoredRuntimeSettings",
)


def repo_root() -> Path:
    return Path(__file__).resolve().parents[1]


def rel(path: Path) -> str:
    return path.relative_to(repo_root()).as_posix()


def read_module_path(root: Path) -> str:
    go_mod = root / "go.mod"
    for line in go_mod.read_text(encoding="utf-8").splitlines():
        if line.startswith("module "):
            return line.split(None, 1)[1].strip()
    raise RuntimeError("go.mod does not declare a module path")


def load_allowlist(root: Path) -> dict[str, Any]:
    path = root / ALLOWLIST_PATH
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError as exc:
        raise RuntimeError(f"missing allowlist: {ALLOWLIST_PATH}") from exc
    except json.JSONDecodeError as exc:
        raise RuntimeError(f"invalid JSON in {ALLOWLIST_PATH}: {exc}") from exc


def go_files(root: Path) -> list[Path]:
    skip_dirs = {".git", ".gocache", ".tmp-go", "vendor"}
    files: list[Path] = []
    for path in root.rglob("*.go"):
        relative_parts = path.relative_to(root).parts
        if any(part in skip_dirs for part in relative_parts):
            continue
        if relative_parts[:4] == ("internal", "storage", "postgres", "ent"):
            continue
        files.append(path)
    return sorted(files)


def line_count(path: Path) -> int:
    with path.open("rb") as handle:
        return sum(1 for _ in handle)


def quoted_imports(path: Path) -> set[str]:
    text = path.read_text(encoding="utf-8", errors="ignore")
    return set(re.findall(r'"([^"]+)"', text))


def count_symbol_occurrences(path: Path, symbols: tuple[str, ...]) -> dict[str, int]:
    text = path.read_text(encoding="utf-8", errors="ignore")
    counts: dict[str, int] = {}
    for symbol in symbols:
        count = len(re.findall(re.escape(symbol), text))
        if count:
            counts[symbol] = count
    return counts


def internal_dirs(files: list[Path], root: Path) -> tuple[set[str], set[str]]:
    prod_dirs: set[str] = set()
    test_dirs: set[str] = set()
    for path in files:
        relative = path.relative_to(root)
        if not relative.parts or relative.parts[0] != "internal":
            continue
        directory = relative.parent.as_posix()
        if path.name.endswith("_test.go"):
            test_dirs.add(directory)
        else:
            prod_dirs.add(directory)
    return prod_dirs, test_dirs


def scan(update_baseline: bool = False) -> int:
    root = repo_root()
    module_path = read_module_path(root)
    internal_import_prefix = f"{module_path}/internal/"
    allowlist = load_allowlist(root)

    files = go_files(root)
    production_files = [p for p in files if not p.name.endswith("_test.go")]
    test_files = [p for p in files if p.name.endswith("_test.go")]
    internal_files = [p for p in files if p.relative_to(root).parts[:1] == ("internal",)]
    internal_production_files = [p for p in internal_files if not p.name.endswith("_test.go")]

    failures: list[str] = []
    warnings: list[str] = []
    notes: list[str] = []

    large_allowlist = {
        entry["path"]: entry
        for entry in allowlist.get("large_files", {}).get("allow_above_1200", [])
    }
    over_800: list[tuple[str, int]] = []
    over_1200: list[tuple[str, int]] = []
    for path in production_files:
        lines = line_count(path)
        relative = rel(path)
        if lines > WARNING_LINE_THRESHOLD:
            over_800.append((relative, lines))
        if lines > BLOCKING_LINE_THRESHOLD:
            over_1200.append((relative, lines))
            entry = large_allowlist.get(relative)
            if entry is None:
                failures.append(
                    f"{relative} has {lines} lines and is not in the >1200 line allowlist"
                )
                continue
            max_lines = int(entry.get("max_lines", 0))
            if max_lines and lines > max_lines:
                failures.append(
                    f"{relative} grew from allowlisted max {max_lines} to {lines} lines"
                )
            elif max_lines and lines < max_lines:
                notes.append(
                    f"{relative} is below allowlisted max {max_lines}; consider tightening the allowlist"
                )
    # Ratchet on the 800-line band: files already over it are frozen at their
    # current size and may only shrink, and a new file crossing it fails. Before
    # this, anything between 800 and 1200 lines could grow freely, which is how
    # the current backlog of oversized files accumulated.
    band_baseline = {
        entry["path"]: int(entry["max_lines"])
        for entry in allowlist.get("large_files", {}).get("baseline_above_800", [])
    }
    for relative, lines in over_800:
        if relative in large_allowlist:
            # Already governed by the stricter >1200 allowlist above.
            continue
        allowed = band_baseline.get(relative)
        if allowed is None:
            failures.append(
                f"{relative} has {lines} lines, over the {WARNING_LINE_THRESHOLD} line limit, "
                "and is not in the baseline. Split it, or record it with "
                "scripts/check-backend-structure.py --update-baseline"
            )
        elif lines > allowed:
            failures.append(
                f"{relative} grew from baseline {allowed} to {lines} lines; "
                "files over the limit may only shrink"
            )
        elif lines < allowed:
            notes.append(
                f"{relative} shrank from baseline {allowed} to {lines} lines; "
                "run --update-baseline to lock in the improvement"
            )
    for relative in sorted(set(band_baseline) - {r for r, _ in over_800}):
        notes.append(
            f"{relative} is no longer over {WARNING_LINE_THRESHOLD} lines; "
            "run --update-baseline to drop it from the baseline"
        )

    if update_baseline:
        return write_band_baseline(root, allowlist, over_800, large_allowlist)

    sdk_allowlist: dict[str, list[str]] = allowlist.get("sdk_internal_imports", {}).get(
        "allow", {}
    )
    sdk_internal_files: dict[str, list[str]] = {}
    for path in sorted((root / "sdk").rglob("*.go")):
        if path.name.endswith("_test.go"):
            continue
        imports = sorted(i for i in quoted_imports(path) if i.startswith(internal_import_prefix))
        if not imports:
            continue
        relative = rel(path)
        sdk_internal_files[relative] = imports
        allowed_imports = set(sdk_allowlist.get(relative, []))
        if not allowed_imports:
            failures.append(f"{relative} imports internal packages but is not allowlisted")
            continue
        for import_path in imports:
            if import_path not in allowed_imports:
                failures.append(f"{relative} has new internal import {import_path}")
        removed = sorted(allowed_imports.difference(imports))
        if removed:
            notes.append(
                f"{relative} no longer imports {', '.join(removed)}; consider tightening the allowlist"
            )

    persistence_allowlist: dict[str, dict[str, int]] = allowlist.get(
        "management_persistence_calls", {}
    ).get("allow", {})
    persistence_files: dict[str, dict[str, int]] = {}
    for path in sorted((root / MANAGEMENT_DIR).rglob("*.go")):
        if path.name.endswith("_test.go"):
            continue
        counts = count_symbol_occurrences(path, MANAGEMENT_PERSISTENCE_SYMBOLS)
        if not counts:
            continue
        relative = rel(path)
        persistence_files[relative] = counts
        allowed_counts = persistence_allowlist.get(relative, {})
        if not allowed_counts:
            failures.append(
                f"{relative} directly calls persistence functions but is not allowlisted"
            )
            continue
        for symbol, count in counts.items():
            allowed_count = int(allowed_counts.get(symbol, 0))
            if count > allowed_count:
                failures.append(
                    f"{relative} has {count} {symbol} calls; allowlisted maximum is {allowed_count}"
                )
            elif count < allowed_count:
                notes.append(
                    f"{relative} has fewer {symbol} calls than the allowlist; consider tightening it"
                )

    server_text = (root / SERVER_PATH).read_text(encoding="utf-8", errors="ignore")
    management_routes = len(re.findall(r"\bmgmt\.(GET|POST|PUT|PATCH|DELETE)\s*\(", server_text))
    handler_methods = 0
    for path in sorted((root / MANAGEMENT_DIR).rglob("*.go")):
        text = path.read_text(encoding="utf-8", errors="ignore")
        handler_methods += len(re.findall(r"func\s*\(\s*h\s+\*Handler\s*\)", text))

    prod_dirs, test_dirs = internal_dirs(files, root)
    missing_test_dirs = sorted(prod_dirs.difference(test_dirs))

    print("Backend structure scan")
    print(f"Repository: {root}")
    print(f"Go files: {len(files)} total, {len(production_files)} production, {len(test_files)} tests")
    print(
        "Internal Go files: "
        f"{len(internal_files)} total, {len(internal_production_files)} production, "
        f"{len(internal_files) - len(internal_production_files)} tests"
    )
    print(
        f"Large production files: {len(over_800)} over {WARNING_LINE_THRESHOLD} lines, "
        f"{len(over_1200)} over {BLOCKING_LINE_THRESHOLD} lines"
    )
    print(f"SDK production files importing internal packages: {len(sdk_internal_files)}")
    print(f"Management handler files with direct persistence calls: {len(persistence_files)}")
    print(f"Management routes registered in {SERVER_PATH}: {management_routes}")
    print(f"management.Handler receiver methods: {handler_methods}")
    print(
        "Internal production directories: "
        f"{len(prod_dirs)} total, {len(test_dirs)} with direct tests, "
        f"{len(missing_test_dirs)} without direct tests"
    )

    if warnings:
        print("\nWarnings")
        for warning in warnings:
            print(f"- {warning}")

    if notes:
        print("\nNotes")
        for note in notes:
            print(f"- {note}")

    if failures:
        print("\nFailures")
        for failure in failures:
            print(f"- {failure}")
        return 1

    print("\nResult: passed")
    return 0


def write_band_baseline(
    root: Path,
    allowlist: dict[str, Any],
    over_800: list[tuple[str, int]],
    large_allowlist: dict[str, Any],
) -> int:
    """Record every file currently over the warning threshold as the new floor."""
    entries = [
        {"path": relative, "max_lines": lines}
        for relative, lines in sorted(over_800)
        if relative not in large_allowlist
    ]
    allowlist.setdefault("large_files", {})["baseline_above_800"] = entries
    path = root / ALLOWLIST_PATH
    path.write_text(json.dumps(allowlist, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
    print(f"\nBaseline updated: {len(entries)} file(s) over {WARNING_LINE_THRESHOLD} lines.")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(scan("--update-baseline" in sys.argv))
    except RuntimeError as exc:
        print(f"Backend structure scan failed: {exc}", file=sys.stderr)
        raise SystemExit(1)
