"""Turn bench JSON lines into a markdown table: one row per test, one column per server, best value in bold."""
import json, sys
from collections import OrderedDict

rows, servers = OrderedDict(), []
for line in open(sys.argv[1]):
    r = json.loads(line)
    if r["server"] not in servers:
        servers.append(r["server"])
    rows.setdefault((r["test"], r["unit"]), {})[r["server"]] = r

print("| Test | " + " | ".join(servers) + " |")
print("|---|" + "---:|" * len(servers))
for (test, unit), by in rows.items():
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
