#!/usr/bin/env python3
"""Turn the two arms' issue reports into the comparison that sets the defaults.

Reads <dir>/<arm>/<issue>.json, each one a `bd-auto issue run` report, and prints
markdown ready to paste into the issue's notes.

Cost is the headline and token counts are printed underneath it, deliberately in
that order: cache reads bill at a fraction of the input price, so the resume arm
always wins on raw input tokens whether or not it is actually cheaper.
"""

import json
import os
import sys

ARMS = ("fresh", "resume")


def load(root, arm):
    d = os.path.join(root, arm)
    if not os.path.isdir(d):
        return None
    out = {"issues": [], "wall": None}
    wall = os.path.join(d, "wall-seconds.txt")
    if os.path.exists(wall):
        with open(wall) as f:
            out["wall"] = int(f.read().strip() or 0)
    for name in sorted(os.listdir(d)):
        if not name.endswith(".json"):
            continue
        with open(os.path.join(d, name)) as f:
            try:
                out["issues"].append(json.load(f))
            except json.JSONDecodeError:
                out["issues"].append({"issue": name[:-5], "outcome": "no-report"})
    return out


def totals(arm):
    t = {
        "cost": 0.0,
        "rounds": 0,
        "attempts": 0,
        "infra": 0,
        "seconds": 0.0,
        "done": 0,
        "issues": 0,
        "input": 0,
        "output": 0,
        "cache_read": 0,
        "cache_creation": 0,
    }
    for rep in arm["issues"]:
        t["issues"] += 1
        if rep.get("outcome") == "done":
            t["done"] += 1
        u = rep.get("usage") or {}
        t["cost"] += u.get("cost_usd", 0.0)
        t["input"] += u.get("input_tokens", 0)
        t["output"] += u.get("output_tokens", 0)
        t["cache_read"] += u.get("cache_read_tokens", 0)
        t["cache_creation"] += u.get("cache_creation_tokens", 0)
        t["seconds"] += rep.get("seconds", 0.0)
        for at in rep.get("attempts") or []:
            t["attempts"] += 1
            t["rounds"] += at.get("rounds", 0)
            t["infra"] += at.get("infra_retries", 0)
    return t


def main():
    root = sys.argv[1] if len(sys.argv) > 1 else "reports"
    arms = {a: load(root, a) for a in ARMS}
    have = {a: v for a, v in arms.items() if v}
    if not have:
        print("no reports under " + root)
        return 1

    print("## Resume versus fresh: measured\n")
    meta = os.path.join(root, "meta.txt")
    if os.path.exists(meta):
        with open(meta) as f:
            fields = [ln.strip() for ln in f if ln.strip()]
        print("Fixture: " + ", ".join(fields) + "\n")
    print("| arm | config | issues done | model processes | attempts | "
          "total_cost_usd | wall clock |")
    print("|---|---|---|---|---|---|---|")
    cfg = {"fresh": "max_rounds 1, retry 3", "resume": "max_rounds 4, retry 1"}
    tot = {}
    for a, arm in have.items():
        t = totals(arm)
        tot[a] = t
        procs = t["rounds"] + t["infra"]
        wall = "%ds" % arm["wall"] if arm["wall"] is not None else "-"
        print("| %s | %s | %d/%d | %d | %d | $%.4f | %s |" % (
            a, cfg[a], t["done"], t["issues"], procs, t["attempts"], t["cost"], wall))

    if len(tot) == 2:
        f, r = tot["fresh"], tot["resume"]
        print()
        if f["cost"] > 0:
            delta = (r["cost"] - f["cost"]) / f["cost"] * 100.0
            cheaper = "resume" if r["cost"] < f["cost"] else "fresh"
            print("**%s is cheaper: $%.4f versus $%.4f, a %+.1f%% difference "
                  "for the resume arm.**" % (cheaper, min(r["cost"], f["cost"]),
                                             max(r["cost"], f["cost"]), delta))
        if f["done"] != f["issues"] or r["done"] != r["issues"]:
            print("\nNote: an arm did not finish every issue, so the totals are "
                  "not comparing equal work.")

    print("\n### Per issue\n")
    print("| arm | issue | outcome | attempts | rounds | infra | cost_usd | seconds |")
    print("|---|---|---|---|---|---|---|---|")
    for a, arm in have.items():
        for rep in arm["issues"]:
            ats = rep.get("attempts") or []
            rounds = sum(x.get("rounds", 0) for x in ats)
            infra = sum(x.get("infra_retries", 0) for x in ats)
            u = rep.get("usage") or {}
            print("| %s | %s | %s | %d | %d | %d | $%.4f | %.0f |" % (
                a, rep.get("issue", "?"), rep.get("outcome", "?"), len(ats),
                rounds, infra, u.get("cost_usd", 0.0), rep.get("seconds", 0.0)))

    print("\n### Tokens, for completeness only\n")
    print("These do not decide anything. Cache reads bill far below input price, "
          "so the arm that reads more cache looks worse here while costing less.\n")
    print("| arm | input | output | cache read | cache creation |")
    print("|---|---|---|---|---|")
    for a in have:
        t = tot[a]
        print("| %s | %d | %d | %d | %d |" % (
            a, t["input"], t["output"], t["cache_read"], t["cache_creation"]))
    return 0


if __name__ == "__main__":
    sys.exit(main())
