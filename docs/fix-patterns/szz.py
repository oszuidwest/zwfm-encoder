#!/usr/bin/env python3
"""Strict SZZ + base-rate + hotspot measurement per DEFINITIONS.md.

Usage: szz.py <repo> <outdir> [--until SHA_OR_DATE]
--until restricts the analysed history (for the backtest): only commits with
author date < UNTIL are considered, and hotspot metrics are computed at the
last commit before UNTIL.
"""
import collections
import csv
import json
import os
import re
import subprocess
import sys
from datetime import datetime

REPO, OUT = sys.argv[1], sys.argv[2]
UNTIL = sys.argv[sys.argv.index("--until") + 1] if "--until" in sys.argv else None
START = "2025-12-16"
FIX_RE = re.compile(r"^fix(\([^)]*\))?!?[:\s]", re.I)
SRC_EXT = (".go", ".js", ".mjs", ".ts", ".svelte", ".css", ".html", ".sh")
COMMENT_PREFIX = ("//", "#", "/*", "*", "<!--")
os.makedirs(OUT, exist_ok=True)


def git(*args):
    return subprocess.run(["git", "-C", REPO, *args], capture_output=True, text=True, errors="replace").stdout


def is_source(p):
    if not p.endswith(SRC_EXT):
        return False
    if p.endswith("_test.go") or p.endswith(".test.js") or ".spec." in p:
        return False
    parts = p.split("/")
    return not any(x in ("tests", "docs", ".github", "node_modules", "vendor") for x in parts[:-1])


def module(p):
    parts = p.split("/")
    if parts[0] == "internal" and len(parts) > 2:
        return parts[1]
    if len(parts) == 1 and p.endswith(".go"):
        return "root"
    if parts[0] == "web":
        return "web"
    if parts[0] == "deploy" or (len(parts) == 1 and p.endswith(".sh")):
        return "deploy"
    if parts[0] == "zabbix":
        return "zabbix"
    return "other"


def meaningful(line):
    s = line.strip()
    return bool(s) and not s.startswith(COMMENT_PREFIX)


# --- commits in period -------------------------------------------------------
log = git("log", "--no-merges", "--format=%H%x00%ad%x00%s", "--date=short", f"--since={START}T00:00:00", "HEAD")
commits = {}
order = []
for row in log.splitlines():
    sha, date, subj = row.split("\x00", 2)
    if date < START or (UNTIL and date >= UNTIL):
        continue
    commits[sha] = {"date": date, "subject": subj, "fix": bool(FIX_RE.match(subj))}
    order.append(sha)
fixes = [s for s in order if commits[s]["fix"]]


def blame(rev, path):
    """Return list of (sha, content) per line of path at rev."""
    out = git("blame", "-w", "-M", "-C", "--porcelain", rev, "--", path)
    res, cur = [], None
    for ln in out.splitlines():
        if ln.startswith("\t"):
            res.append((cur, ln[1:]))
        else:
            tok = ln.split(" ")
            if len(tok[0]) == 40 and len(tok) >= 3 and tok[1].isdigit():
                cur = tok[0]
    return res


HUNK = re.compile(r"^@@ -(\d+)(?:,(\d+))? \+\d+(?:,\d+)? @@")


def old_lines(c, path):
    """Old-side line numbers deleted/modified, plus anchor lines for pure additions."""
    out = git("diff", "-U0", "-w", "--no-renames", f"{c}^", c, "--", path)
    deleted, anchors = [], []
    for ln in out.splitlines():
        m = HUNK.match(ln)
        if not m:
            continue
        a, b = int(m.group(1)), int(m.group(2) or "1")
        if b == 0:
            anchors.append(a)  # insertion after line a
        else:
            deleted.extend(range(a, a + b))
    return deleted, anchors


mods = collections.defaultdict(lambda: collections.Counter())
pairs = []
fix_rows = []
for c in fixes:
    status = git("diff", "--name-status", "--no-renames", f"{c}^", c)
    files = [l.split("\t", 1)[1] for l in status.splitlines() if l.startswith("M\t")]
    files = [f for f in files if is_source(f)]
    intro_lines = collections.Counter()  # (sha, module) -> lines
    lenient = collections.Counter()
    touched_mods = set(module(f) for f in files)
    for f in files:
        bl = blame(f"{c}^", f)
        if not bl:
            continue
        m = module(f)
        deleted, anchors = old_lines(c, f)
        # base rate over the whole file
        for sha, txt in bl:
            if meaningful(txt):
                mods[m]["base_lines"] += 1
                if sha in commits and commits[sha]["fix"]:
                    mods[m]["base_fix_lines"] += 1
        for n in deleted:
            if 1 <= n <= len(bl) and meaningful(bl[n - 1][1]):
                sha = bl[n - 1][0]
                intro_lines[(sha, m)] += 1
                mods[m]["blamed_lines"] += 1
                if sha in commits and commits[sha]["fix"]:
                    mods[m]["blamed_fix_lines"] += 1
        for n in anchors:
            # nearest meaningful line at or above the insertion point
            k = n
            while k >= 1 and not meaningful(bl[k - 1][1]):
                k -= 1
            if k >= 1:
                lenient[(bl[k - 1][0], m)] += 1
    blamed_mods = set(m for (_, m) in intro_lines)
    fix_intros = {(s, m): n for (s, m), n in intro_lines.items() if s in commits and commits[s]["fix"] and s != c}
    lenient_fix = {(s, m) for (s, m) in list(intro_lines) + list(lenient) if s in commits and commits[s]["fix"] and s != c}
    for m in touched_mods:
        mods[m]["fix_commits"] += 1
    for m in blamed_mods:
        mods[m]["fix_with_blamed"] += 1
    for m in set(m for (_, m) in fix_intros):
        mods[m]["fix_induced"] += 1
    for m in set(m for (_, m) in lenient_fix):
        mods[m]["fix_induced_lenient"] += 1
    for (s, m), n in fix_intros.items():
        d = (datetime.fromisoformat(commits[c]["date"]) - datetime.fromisoformat(commits[s]["date"])).days
        pairs.append([c[:10], commits[c]["subject"], s[:10], commits[s]["subject"], m, n, d])
    fix_rows.append({
        "sha": c[:10], "date": commits[c]["date"], "subject": commits[c]["subject"],
        "modules": sorted(touched_mods), "blamed_modules": sorted(blamed_mods),
        "introducing": sorted({s[:10] for (s, _) in intro_lines}),
        "fix_introducing": sorted({s[:10] for (s, _) in fix_intros}),
    })

# --- hotspots ----------------------------------------------------------------
head = git("rev-list", "-1", f"--before={UNTIL}T00:00:00", "HEAD").strip() if UNTIL else "HEAD"
revs, fixc = collections.Counter(), collections.Counter()
for c in order:
    for f in git("diff-tree", "--no-commit-id", "--name-only", "-r", "--no-renames", c).splitlines():
        revs[f] += 1
        if commits[c]["fix"]:
            fixc[f] += 1
tree = [p for p in git("ls-tree", "-r", "--name-only", head).splitlines() if is_source(p)]
gocog = {}
if not UNTIL:
    out = subprocess.run([os.environ.get("GOCOGNIT", "gocognit"), "."], cwd=REPO,
                         capture_output=True, text=True).stdout
    for ln in out.splitlines():
        # "<score> <pkg> <func> <file>:<line>:<col>"
        tok = ln.split()
        path = tok[-1].rsplit(":", 2)[0]
        gocog[path] = gocog.get(path, 0) + int(tok[0])
hot = []
for p in tree:
    if p.endswith(".go") and not UNTIL:
        cx = gocog.get(p, 0)
    else:
        cx = sum(1 for l in git("show", f"{head}:{p}").splitlines() if l.strip())
    hot.append([p, module(p), revs[p], fixc[p], cx])
    mods[module(p)]["kloc_lines"] += sum(1 for l in git("show", f"{head}:{p}").splitlines() if l.strip())
    mods[module(p)]["revisions"] += revs[p]
hot.sort(key=lambda r: -r[2] * r[4])

# --- write -------------------------------------------------------------------
with open(os.path.join(OUT, "per_module.csv"), "w", newline="") as fh:
    w = csv.writer(fh)
    w.writerow(["module", "loc", "file_revisions", "fix_commits", "fix_with_blamed", "fix_induced", "ratio",
                "fix_induced_lenient", "blamed_lines", "blamed_fix_lines", "observed", "expected", "lift",
                "fixes_per_kloc"])
    for m, d in sorted(mods.items(), key=lambda kv: -kv[1]["fix_commits"]):
        obs = d["blamed_fix_lines"] / d["blamed_lines"] if d["blamed_lines"] else 0
        exp = d["base_fix_lines"] / d["base_lines"] if d["base_lines"] else 0
        w.writerow([m, d["kloc_lines"], d["revisions"], d["fix_commits"], d["fix_with_blamed"], d["fix_induced"],
                    round(d["fix_induced"] / d["fix_with_blamed"], 2) if d["fix_with_blamed"] else "",
                    d["fix_induced_lenient"], d["blamed_lines"], d["blamed_fix_lines"], round(obs, 3), round(exp, 3),
                    round(obs / exp, 2) if exp else "",
                    round(d["fix_commits"] / (d["kloc_lines"] / 1000), 2) if d["kloc_lines"] else ""])
with open(os.path.join(OUT, "pairs.csv"), "w", newline="") as fh:
    w = csv.writer(fh)
    w.writerow(["fix", "fix_subject", "introducing", "introducing_subject", "module", "lines", "days"])
    w.writerows(sorted(pairs, key=lambda r: (r[4], r[0])))
with open(os.path.join(OUT, "hotspots.csv"), "w", newline="") as fh:
    w = csv.writer(fh)
    w.writerow(["path", "module", "revisions", "fixes", "complexity"])
    w.writerows(hot)
with open(os.path.join(OUT, "fixes.json"), "w") as fh:
    json.dump(fix_rows, fh, indent=1)
print(f"period commits={len(order)} fixes={len(fixes)} pairs={len(pairs)}")
