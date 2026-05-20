#!/usr/bin/env python3
"""
compare-results.py — compare result changes based on git state.

Modes:
  local   Compare working tree (unstaged + staged) vs HEAD (default)
  staged  Compare staged changes vs HEAD
  commit  Compare HEAD vs a base ref (e.g. origin/master)
  ref     Compare any two arbitrary git refs

Output formats:
  terminal  Colour-coded human-readable output (default)
  markdown  GitHub-flavoured Markdown table (suitable for PR comments)
  json      Machine-readable JSON

Usage examples:
  # Local: working tree vs HEAD
  python3 scripts/compare-results.py

  # Staged only vs HEAD
  python3 scripts/compare-results.py --mode staged

  # HEAD vs origin/master (CI / PR review)
  python3 scripts/compare-results.py --mode commit --base origin/master

  # Arbitrary refs
  python3 scripts/compare-results.py --mode ref --base origin/main --head HEAD

  # Output as Markdown (for attaching to a PR comment)
  python3 scripts/compare-results.py --mode commit --base origin/master --format markdown

  # Include all changed files, even those with no step status changes
  python3 scripts/compare-results.py --all
"""

import argparse
import json
import os
import subprocess
import sys
from dataclasses import dataclass, field
from pathlib import Path
from typing import Optional

RESULTS_DIR = "results"

# ──────────────────────────────────────────────────────────────────────────────
# Terminal colours (disabled when writing to a pipe / --format is not terminal)
# ──────────────────────────────────────────────────────────────────────────────

ANSI = {
    "reset": "\033[0m",
    "bold": "\033[1m",
    "green": "\033[32m",
    "red": "\033[31m",
    "yellow": "\033[33m",
    "cyan": "\033[36m",
    "dim": "\033[2m",
}


def c(text: str, *codes: str, use_color: bool = True) -> str:
    if not use_color:
        return text
    prefix = "".join(ANSI[code] for code in codes)
    return f"{prefix}{text}{ANSI['reset']}"


# ──────────────────────────────────────────────────────────────────────────────
# Data structures
# ──────────────────────────────────────────────────────────────────────────────

STATUS_PASS = "pass"
STATUS_FAIL = "fail"
STATUS_MISSING = "missing"  # step absent from one side


@dataclass
class StepChange:
    step: str
    before: str  # pass | fail | missing
    after: str  # pass | fail | missing

    @property
    def changed(self) -> bool:
        return self.before != self.after

    @property
    def regression(self) -> bool:
        return self.before == STATUS_PASS and self.after == STATUS_FAIL

    @property
    def fix(self) -> bool:
        return self.before == STATUS_FAIL and self.after == STATUS_PASS

    @property
    def new_pass(self) -> bool:
        return self.before == STATUS_MISSING and self.after == STATUS_PASS

    @property
    def new_fail(self) -> bool:
        return self.before == STATUS_MISSING and self.after == STATUS_FAIL

    @property
    def removed(self) -> bool:
        return self.before != STATUS_MISSING and self.after == STATUS_MISSING


@dataclass
class ResultChange:
    path: str  # relative path within results/
    file_status: str  # A, M, D
    result_id: str
    before_overall: Optional[str]
    after_overall: Optional[str]
    step_changes: list[StepChange] = field(default_factory=list)

    @property
    def has_step_status_changes(self) -> bool:
        return any(sc.changed for sc in self.step_changes)

    @property
    def overall_changed(self) -> bool:
        return self.before_overall != self.after_overall

    @property
    def any_regression(self) -> bool:
        return any(sc.regression for sc in self.step_changes)

    @property
    def any_fix(self) -> bool:
        return any(sc.fix or sc.new_pass for sc in self.step_changes)


# ──────────────────────────────────────────────────────────────────────────────
# Git helpers
# ──────────────────────────────────────────────────────────────────────────────

def git(*args: str) -> str:
    """Run a git command and return stdout, raising on error."""
    result = subprocess.run(
        ["git", *args],
        capture_output=True,
        text=True,
    )
    if result.returncode != 0:
        raise RuntimeError(f"git {' '.join(args)} failed:\n{result.stderr.strip()}")
    return result.stdout


def git_repo_root() -> Path:
    return Path(git("rev-parse", "--show-toplevel").strip())


def get_changed_files_working_tree(results_dir: str) -> list[tuple[str, str]]:
    """Return (status, path) for files changed in the working tree vs HEAD.
    Includes both staged and unstaged changes.
    """
    # --diff-filter=AMD: Added, Modified, Deleted
    staged = git(
        "diff", "--cached", "--name-status", "--diff-filter=AMD", "--", results_dir
    ).splitlines()
    unstaged = git(
        "diff", "--name-status", "--diff-filter=AMD", "--", results_dir
    ).splitlines()
    untracked = git(
        "ls-files", "--others", "--exclude-standard", "--", results_dir
    ).splitlines()

    seen: dict[str, str] = {}
    for line in staged + unstaged:
        if not line.strip():
            continue
        parts = line.split("\t", 1)
        if len(parts) == 2:
            status, path = parts
            seen[path] = status[0]  # first char: A/M/D

    for path in untracked:
        path = path.strip()
        if path:
            seen[path] = "A"

    return list(seen.items())


def get_changed_files_staged(results_dir: str) -> list[tuple[str, str]]:
    """Return (status, path) for staged changes only vs HEAD."""
    lines = git(
        "diff", "--cached", "--name-status", "--diff-filter=AMD", "--", results_dir
    ).splitlines()
    result = []
    for line in lines:
        if not line.strip():
            continue
        parts = line.split("\t", 1)
        if len(parts) == 2:
            result.append((parts[1].strip(), parts[0][0]))
    return result


def get_changed_files_between_refs(
    base: str, head: str, results_dir: str
) -> list[tuple[str, str]]:
    """Return (status, path) for files changed between two git refs."""
    lines = git(
        "diff", "--name-status", "--diff-filter=AMD", f"{base}...{head}", "--", results_dir
    ).splitlines()
    result = []
    for line in lines:
        if not line.strip():
            continue
        parts = line.split("\t", 1)
        if len(parts) == 2:
            result.append((parts[1].strip(), parts[0][0]))
    return result


def read_json_at_ref(ref: str, path: str) -> Optional[dict]:
    """Read a JSON file at a specific git ref. Returns None if not found."""
    try:
        content = git("show", f"{ref}:{path}")
        return json.loads(content)
    except (RuntimeError, json.JSONDecodeError):
        return None


def read_json_working_tree(root: Path, path: str) -> Optional[dict]:
    """Read a JSON file from the working tree."""
    full = root / path
    if not full.exists():
        return None
    try:
        return json.loads(full.read_text())
    except json.JSONDecodeError:
        return None


# ──────────────────────────────────────────────────────────────────────────────
# Comparison logic
# ──────────────────────────────────────────────────────────────────────────────

def extract_steps(data: Optional[dict]) -> dict[str, str]:
    if data is None:
        return {}
    return {name: step.get("status", "unknown") for name, step in data.get("steps", {}).items()}


def compare_results(
    before_data: Optional[dict],
    after_data: Optional[dict],
    path: str,
    file_status: str,
) -> ResultChange:
    result_id = (
        after_data.get("id") if after_data else
        before_data.get("id") if before_data else
        os.path.basename(path).replace(".json", "")
    )

    before_overall = before_data.get("overall_status") if before_data else None
    after_overall = after_data.get("overall_status") if after_data else None

    before_steps = extract_steps(before_data)
    after_steps = extract_steps(after_data)

    all_step_names = sorted(set(before_steps) | set(after_steps))
    step_changes = []
    for step in all_step_names:
        b = before_steps.get(step, STATUS_MISSING)
        a = after_steps.get(step, STATUS_MISSING)
        step_changes.append(StepChange(step=step, before=b, after=a))

    return ResultChange(
        path=path,
        file_status=file_status,
        result_id=result_id,
        before_overall=before_overall,
        after_overall=after_overall,
        step_changes=step_changes,
    )


# ──────────────────────────────────────────────────────────────────────────────
# Diff runners
# ──────────────────────────────────────────────────────────────────────────────

def run_working_tree_diff(root: Path, results_dir: str) -> list[ResultChange]:
    changes = []
    for path, status in get_changed_files_working_tree(results_dir):
        if not path.endswith(".json"):
            continue
        before = read_json_at_ref("HEAD", path)
        after = read_json_working_tree(root, path)
        changes.append(compare_results(before, after, path, status))
    return changes


def run_staged_diff(root: Path, results_dir: str) -> list[ResultChange]:
    changes = []
    for path, status in get_changed_files_staged(results_dir):
        if not path.endswith(".json"):
            continue
        before = read_json_at_ref("HEAD", path)
        # Read staged version via git index
        try:
            content = git("show", f":{path}")
            after = json.loads(content)
        except (RuntimeError, json.JSONDecodeError):
            after = None
        changes.append(compare_results(before, after, path, status))
    return changes


def run_ref_diff(root: Path, results_dir: str, base: str, head: str) -> list[ResultChange]:
    changes = []
    for path, status in get_changed_files_between_refs(base, head, results_dir):
        if not path.endswith(".json"):
            continue
        before = read_json_at_ref(base, path)
        after = read_json_at_ref(head, path)
        changes.append(compare_results(before, after, path, status))
    return changes


# ──────────────────────────────────────────────────────────────────────────────
# Output formatters
# ──────────────────────────────────────────────────────────────────────────────

step_sort_order = {
    "overall": 1,
    "stack_up": 2,
    "install": 3,
    "smoke": 4,
    "playwright": 5,
}

def status_icon(status: str, use_color: bool) -> str:
    icons = {
        STATUS_PASS: c("✓ pass", "green", use_color=use_color),
        STATUS_FAIL: c("✗ fail", "red", use_color=use_color),
        STATUS_MISSING: c("— n/a", "dim", use_color=use_color),
    }
    return icons.get(status, status)


def format_terminal(
    changes: list[ResultChange],
    show_all: bool,
    base_label: str,
    head_label: str,
) -> str:
    use_color = sys.stdout.isatty()
    lines = []

    # Filter unless --all
    if not show_all:
        filtered = [ch for ch in changes if ch.has_step_status_changes or ch.overall_changed]
    else:
        filtered = changes

    if not filtered:
        lines.append(c("No step status changes detected.", "dim", use_color=use_color))
        return "\n".join(lines)

    regressions = sum(1 for ch in filtered if ch.any_regression)
    fixes = sum(1 for ch in filtered if ch.any_fix)
    new_results = sum(1 for ch in filtered if ch.file_status == "A")

    lines.append(c(f"Result diff  {base_label} → {head_label}", "bold", "cyan", use_color=use_color))
    lines.append(c(f"  Changed: {len(filtered)}   Regressions: {regressions}   Fixes: {fixes}   New: {new_results}", "dim", use_color=use_color))
    lines.append("")

    for ch in sorted(filtered, key=lambda x: x.result_id):
        # Header line
        if ch.file_status == "A":
            badge = c("[NEW]", "cyan", use_color=use_color)
        elif ch.file_status == "D":
            badge = c("[DEL]", "dim", use_color=use_color)
        else:
            badge = c("[MOD]", "yellow", use_color=use_color)

        overall_part = ""
        if ch.overall_changed:
            overall_part = (
                f"  overall: {status_icon(ch.before_overall or STATUS_MISSING, use_color)} → "
                f"{status_icon(ch.after_overall or STATUS_MISSING, use_color)}"
            )
        elif ch.after_overall:
            overall_part = f"  overall: {status_icon(ch.after_overall, use_color)}"

        lines.append(f"{badge} {c(ch.result_id, 'bold', use_color=use_color)}{overall_part}")

        sorted_steps = sorted(ch.step_changes, key=lambda sc: step_sort_order.get(sc.step.lower(), 100))
        
        for sc in sorted_steps:
            if not show_all and not sc.changed:
                continue
            arrow = f"{status_icon(sc.before, use_color)} → {status_icon(sc.after, use_color)}"
            if sc.regression:
                label = c("  REGRESSION", "red", "bold", use_color=use_color)
            elif sc.fix:
                label = c("  FIXED     ", "green", "bold", use_color=use_color)
            elif sc.new_fail:
                label = c("  NEW FAIL  ", "red", use_color=use_color)
            elif sc.new_pass:
                label = c("  NEW PASS  ", "green", use_color=use_color)
            elif sc.removed:
                label = c("  REMOVED   ", "dim", use_color=use_color)
            else:
                label = "           "
            lines.append(f"    {c(sc.step.ljust(12), 'dim', use_color=use_color)} {arrow}{label}")

        lines.append("")

    return "\n".join(lines)


def _md_status(status: Optional[str]) -> str:
    if status == STATUS_PASS:
        return "✅ pass"
    if status == STATUS_FAIL:
        return "❌ fail"
    if status == STATUS_MISSING:
        return "—"
    return status or "—"


def _md_step_change(sc: StepChange) -> str:
    before = _md_status(sc.before)
    after = _md_status(sc.after)
    if sc.regression:
        return f"~~{before}~~ → **{after}** 🔴"
    if sc.fix:
        return f"~~{before}~~ → **{after}** 🟢"
    if sc.new_fail:
        return f"{after} 🔴"
    if sc.new_pass:
        return f"{after} 🟢"
    if sc.removed:
        return f"~~{before}~~ → —"
    return f"{after}"


def format_markdown(
    changes: list[ResultChange],
    show_all: bool,
    base_label: str,
    head_label: str,
) -> str:
    if not show_all:
        filtered = [ch for ch in changes if ch.has_step_status_changes or ch.overall_changed]
    else:
        filtered = changes

    lines = []
    lines.append("## Result diff")
    lines.append(f"**Base:** `{base_label}` → **Head:** `{head_label}`")
    lines.append("")

    if not filtered:
        lines.append("_No step status changes detected._")
        return "\n".join(lines)

    regressions = sum(1 for ch in filtered if ch.any_regression)
    fixes = sum(1 for ch in filtered if ch.any_fix)
    new_results = sum(1 for ch in filtered if ch.file_status == "A")

    lines.append(
        f"| Metric | Count |"
        f"\n|--------|-------|"
        f"\n| Changed results | {len(filtered)} |"
        f"\n| Regressions | {regressions} |"
        f"\n| Fixes | {fixes} |"
        f"\n| New results | {new_results} |"
    )
    lines.append("")

    # Collect all step names for the table
    all_steps: list[str] = []
    for ch in filtered:
        for sc in ch.step_changes:
            if sc.step not in all_steps:
                all_steps.append(sc.step)

    all_steps = sorted(all_steps, key=lambda s: step_sort_order.get(s.lower(), 100))

    # Table header
    header = "| Result ID | Overall |" + "".join(f" {s} |" for s in all_steps)
    sep = "|---|---|" + "".join("---|" for _ in all_steps)
    lines.append(header)
    lines.append(sep)

    for ch in sorted(filtered, key=lambda x: x.result_id):
        label = ch.result_id
        if ch.file_status == "A":
            label = f"🆕 {label}"
        elif ch.file_status == "D":
            label = f"🗑️ {label}"

        if ch.overall_changed:
            overall_cell = f"{_md_status(ch.before_overall)} → {_md_status(ch.after_overall)}"
        else:
            overall_cell = _md_status(ch.after_overall)

        step_map = {sc.step: sc for sc in ch.step_changes}
        step_cells = ""
        for step in all_steps:
            sc = step_map.get(step)
            if sc is None:
                step_cells += " — |"
            elif not show_all and not sc.changed:
                step_cells += f" {_md_status(sc.after)} |"
            else:
                step_cells += f" {_md_step_change(sc)} |"

        lines.append(f"| `{label}` | {overall_cell} |{step_cells}")

    lines.append("")
    lines.append("<details>")
    lines.append("<summary>Legend</summary>")
    lines.append("")
    lines.append("- 🟢 Fixed / newly passing")
    lines.append("- 🔴 Regression / newly failing")
    lines.append("- ~~before~~ → **after** = status changed")
    lines.append("- 🆕 = new result file added")
    lines.append("- 🗑️ = result file deleted")
    lines.append("")
    lines.append("</details>")

    return "\n".join(lines)


def format_json_output(
    changes: list[ResultChange],
    show_all: bool,
    base_label: str,
    head_label: str,
) -> str:
    if not show_all:
        filtered = [ch for ch in changes if ch.has_step_status_changes or ch.overall_changed]
    else:
        filtered = changes

    output = {
        "base": base_label,
        "head": head_label,
        "summary": {
            "total_changed": len(filtered),
            "regressions": sum(1 for ch in filtered if ch.any_regression),
            "fixes": sum(1 for ch in filtered if ch.any_fix),
            "new_results": sum(1 for ch in filtered if ch.file_status == "A"),
        },
        "results": [],
    }

    for ch in sorted(filtered, key=lambda x: x.result_id):
        entry = {
            "id": ch.result_id,
            "path": ch.path,
            "file_status": ch.file_status,
            "before_overall": ch.before_overall,
            "after_overall": ch.after_overall,
            "steps": [
                {
                    "step": sc.step,
                    "before": sc.before,
                    "after": sc.after,
                    "changed": sc.changed,
                    "regression": sc.regression,
                    "fix": sc.fix,
                }
                for sc in ch.step_changes
            ],
        }
        output["results"].append(entry)

    return json.dumps(output, indent=2)


# ──────────────────────────────────────────────────────────────────────────────
# CLI
# ──────────────────────────────────────────────────────────────────────────────

def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description="Compare result changes based on git state.",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=__doc__,
    )

    parser.add_argument(
        "--mode",
        choices=["local", "staged", "commit", "ref"],
        default="local",
        help=(
            "local: working tree vs HEAD (default); "
            "staged: staged changes vs HEAD; "
            "commit: HEAD vs --base ref; "
            "ref: --base vs --head"
        ),
    )
    parser.add_argument(
        "--base",
        default="origin/master",
        help="Base git ref for 'commit' or 'ref' mode (default: origin/master)",
    )
    parser.add_argument(
        "--head",
        default="HEAD",
        help="Head git ref for 'ref' mode (default: HEAD)",
    )
    parser.add_argument(
        "--format",
        dest="output_format",
        choices=["terminal", "markdown", "json"],
        default="terminal",
        help="Output format (default: terminal)",
    )
    parser.add_argument(
        "--results-dir",
        default=RESULTS_DIR,
        help=f"Path to the results directory relative to repo root (default: {RESULTS_DIR})",
    )
    parser.add_argument(
        "--all",
        action="store_true",
        help="Show all changed files, not just those with step status changes",
    )
    parser.add_argument(
        "--exit-code",
        action="store_true",
        help="Exit with code 1 if any regressions are found",
    )

    return parser


def main() -> None:
    parser = build_parser()
    args = parser.parse_args()

    try:
        root = git_repo_root()
    except RuntimeError as e:
        print(f"Error: {e}", file=sys.stderr)
        sys.exit(2)

    # Change to repo root so relative paths work consistently
    os.chdir(root)

    results_dir = args.results_dir

    try:
        if args.mode == "local":
            changes = run_working_tree_diff(root, results_dir)
            base_label = "HEAD"
            head_label = "working tree"
        elif args.mode == "staged":
            changes = run_staged_diff(root, results_dir)
            base_label = "HEAD"
            head_label = "staged"
        elif args.mode == "commit":
            changes = run_ref_diff(root, results_dir, args.base, "HEAD")
            base_label = args.base
            head_label = "HEAD"
        elif args.mode == "ref":
            changes = run_ref_diff(root, results_dir, args.base, args.head)
            base_label = args.base
            head_label = args.head
        else:
            parser.error(f"Unknown mode: {args.mode}")
            return
    except RuntimeError as e:
        print(f"Error: {e}", file=sys.stderr)
        sys.exit(2)

    if args.output_format == "terminal":
        output = format_terminal(changes, args.all, base_label, head_label)
    elif args.output_format == "markdown":
        output = format_markdown(changes, args.all, base_label, head_label)
    elif args.output_format == "json":
        output = format_json_output(changes, args.all, base_label, head_label)
    else:
        output = ""

    print(output)

    if args.exit_code:
        has_regressions = any(ch.any_regression for ch in changes)
        sys.exit(1 if has_regressions else 0)


if __name__ == "__main__":
    main()
