"""Turn bench JSON lines into markdown tables: one row per test, one column per server, best value in bold.
Compatibility checks (unit "compat") get their own pass/fail table."""
import json, sys
from collections import OrderedDict

rows, servers = OrderedDict(), []
for line in open(sys.argv[1]):
    r = json.loads(line)
    if r["server"] not in servers:
        servers.append(r["server"])
    rows.setdefault((r["test"], r["unit"]), {})[r["server"]] = r

def header():
    print("| Test | " + " | ".join(servers) + " |")
    print("|---|" + "---:|" * len(servers))

perf = [(k, v) for k, v in rows.items() if k[1] != "compat"]
compat = [(k, v) for k, v in rows.items() if k[1] == "compat"]
if perf:
    header()
for (test, unit), by in perf:
    ok = {s: r["value"] for s, r in by.items() if not r.get("error")}
    best = (min if unit == "ms" else max)(ok.values()) if ok else None
    cells = []
    for s in servers:
        r = by.get(s)
        if r is None:
            cells.append("—")
        elif r.get("error"):
            cells.append("error")
        else:
            v = f"{r['value']:,.1f}" if unit == "ms" else f"{r['value']:,.0f}"
            cells.append(f"**{v}**" if r["value"] == best else v)
    print(f"| {test} ({unit}) | " + " | ".join(cells) + " |")

if compat:
    if perf:
        print()
    header()
    for (test, _), by in compat:
        cells = ["—" if s not in by else "✗" if by[s].get("error") else "✓" for s in servers]
        print(f"| {test} | " + " | ".join(cells) + " |")
    print()
    print("<details><summary>Failure details</summary>\n")
    for (test, _), by in compat:
        for s in servers:
            if s in by and by[s].get("error"):
                print(f"- **{s}**, {test}: `{by[s]['error'][:200]}`")
    print("\n</details>")
