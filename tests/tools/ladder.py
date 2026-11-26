#!/usr/bin/env python3
"""Draw one SIP call from a `tcpdump -A` text capture as an HTML/SVG ladder.

    sudo tcpdump -i any -nn -l -A -s0 'udp and (port 6060 or port 6061)' > call.txt
    python3 tests/tools/ladder.py call.txt --leaf 192.0.2.5 \\
        --label 192.0.2.20:5060="PBX" --title "Failover" --out call.html

Lanes: the caller (source of the first INVITE), the leaf (every address on
--leaf with a --leaf-ports port), then every other address in order of first
appearance. Only the first call's Call-ID is drawn; OPTIONS probes are left
out. Gaps over 0.4 s are marked with their length. A packet repeated within
20 ms (same addresses, first line, CSeq and branch) is drawn and labelled as a
copy, which some networks produce. With --summary (a real_pbx.py summary) and
--rtp-peer, a band notes the RTP the caller counted from that peer.
"""

from __future__ import annotations

import argparse
import html
import json
import re

HDR = re.compile(r"^(\d\d):(\d\d):(\d\d\.\d+) (\S+)\s+(In|Out)\s+IP (\S+)\.(\d+) > (\S+)\.(\d+):")
START = re.compile(r"((?:INVITE|ACK|BYE|CANCEL|OPTIONS|UPDATE|INFO|PRACK|REFER|NOTIFY) sip:\S+ SIP/2\.0|SIP/2\.0 \d{3} [^\r\n]*)")


def packets(path: str) -> list[dict]:
    out, cur = [], None
    for line in open(path, errors="replace").read().splitlines():
        m = HDR.match(line)
        if m:
            if cur:
                out.append(cur)
            h, mi, s = m.group(1, 2, 3)
            cur = {"t": int(h) * 3600 + int(mi) * 60 + float(s),
                   "src": f"{m.group(6)}:{m.group(7)}", "dst": f"{m.group(8)}:{m.group(9)}", "body": []}
        elif cur is not None:
            cur["body"].append(line)
    if cur:
        out.append(cur)
    for p in out:
        text = "\n".join(p["body"])
        m = START.search(text)
        p["start"] = m.group(1).strip() if m else ""
        for key, rx in (("cseq", r"^CSeq: (\d+ \S+)"), ("callid", r"^Call-ID: (\S+)"),
                        ("branch", r"branch=([^;\s]+)")):
            m = re.search(rx, text, re.M)
            p[key] = m.group(1) if m else ""
    return [p for p in out if p["start"]]


def label(p: dict) -> str:
    s = p["start"]
    if s.startswith("SIP/2.0 "):
        reason = s[12:]
        if reason.lower().startswith("trying"):
            reason = "Trying"
        method = p["cseq"].split()[1] if " " in p["cseq"] else ""
        return f"{s[8:11]} {reason}  ({method})"
    return " ".join(s.split()[:2])


def render(args: argparse.Namespace) -> str:
    pk = [p for p in packets(args.capture) if not p["cseq"].endswith("OPTIONS")]
    first = next(p for p in pk if p["start"].startswith("INVITE "))
    pk = [p for p in pk if p["callid"] == first["callid"]]
    leaf_ports = set(args.leaf_ports.split(","))
    is_leaf = lambda a: a.rsplit(":", 1)[0] == args.leaf and a.rsplit(":", 1)[1] in leaf_ports  # noqa: E731
    labels = dict(item.split("=", 1) for item in args.label)
    lanes = [first["src"], "leaf"]
    for p in pk:
        for a in (p["src"], p["dst"]):
            if not is_leaf(a) and a not in lanes:
                lanes.append(a)
    lane = lambda a: 1 if is_leaf(a) else lanes.index(a)  # noqa: E731
    names = [labels.get(first["src"], "Caller") + f"<br><small>{first['src']}</small>",
             f"CallStork<br><small>{args.leaf}:{args.leaf_ports.split(',')[0]}</small>"]
    names += [html.escape(labels.get(a, a.rsplit(':', 1)[0])) + f"<br><small>{a}</small>" for a in lanes[2:]]

    seen: dict = {}
    for p in pk:
        key = (p["src"], p["dst"], p["start"], p["cseq"], p["branch"])
        p["dup"] = key in seen and p["t"] - seen[key] < 0.02
        seen[key] = p["t"]

    rtp = 0
    if args.summary:
        rtp = json.load(open(args.summary)).get("rtp_packets", 0)
    width, left, right = 1240, 120, 150
    xs = [left + i * (width - left - right) / (len(lanes) - 1) for i in range(len(lanes))]
    t0, rows, y, prev, rtp_done = pk[0]["t"], [], 30, pk[0]["t"], False
    for p in pk:
        if p["t"] - prev > 0.4 and rows:
            rows.append(("gap", y, p["t"] - prev))
            y += 25
        rows.append(("msg", y, p))
        y += 29
        prev = p["t"]
        if (rtp and args.rtp_peer and not rtp_done and p["start"].startswith("ACK")
                and p["dst"].startswith(args.rtp_peer + ":")):
            rtp_done = True
            rows.append(("rtp", y, None))
            y += 30
    h = y + 20
    parts = [f'<line x1="{x}" y1="0" x2="{x}" y2="{h}" class="lane"/>' for x in xs]
    for kind, yy, data in rows:
        if kind == "gap":
            parts.append(f'<text x="{left - 14}" y="{yy + 17}" class="gap" text-anchor="end">{data:.1f} s</text>')
            for dy in (4, 21):
                parts.append(f'<line x1="{left - 8}" y1="{yy + dy}" x2="{width - right + 30}" y2="{yy + dy}" class="gapline"/>')
            continue
        if kind == "rtp":
            xa, xb = xs[0], xs[lanes.index(next(a for a in lanes[2:] if a.startswith(args.rtp_peer + ':')))]
            parts.append(f'<rect x="{xa}" y="{yy - 12}" width="{xb - xa}" height="20" rx="10" class="rtp"/>')
            parts.append(f'<text x="{(xa + xb) / 2}" y="{yy + 2}" class="rtplbl" text-anchor="middle">RTP flows directly '
                         f'between caller and {html.escape(args.rtp_peer)}, not through the leaf ({rtp} packets counted by the caller)</text>')
            continue
        p = data
        x1, x2 = xs[lane(p["src"])], xs[lane(p["dst"])]
        resp = p["start"].startswith("SIP/2.0")
        code = p["start"][8:11] if resp else ""
        cls = "req" if not resp else {"2": "ok", "1": "prov"}.get(code[:1], "err")
        d = 1 if x2 > x1 else -1
        parts.append(f'<text x="{left - 14}" y="{yy + 4}" class="time" text-anchor="end">+{(p["t"] - t0) * 1000:,.0f} ms</text>')
        parts.append(f'<line x1="{x1}" y1="{yy}" x2="{x2 - d * 7}" y2="{yy}" class="arrow {cls}"/>')
        parts.append(f'<polygon points="{x2},{yy} {x2 - d * 9},{yy - 5} {x2 - d * 9},{yy + 5}" class="{cls}"/>')
        dup = '<tspan class="dup"> copy</tspan>' if p["dup"] else ""
        parts.append(f'<text x="{(x1 + x2) / 2}" y="{yy - 6}" class="lbl {cls}" text-anchor="middle">'
                     f'{html.escape(label(p))}{dup}</text>')
    heads = "".join(f'<div class="lh" style="left:{x}px">{n}</div>' for x, n in zip(xs, names))
    notes = "".join(f"<li>{html.escape(n)}</li>" for n in args.note)
    return f"""<!doctype html><html><head><meta charset="utf-8"><title>{html.escape(args.title)}</title><style>
body{{margin:0;background:#fbfbfa;font:14px/1.35 "DejaVu Sans",Arial,sans-serif;color:#1d2125}}
.wrap{{padding:18px 20px 8px 20px}} h1{{font-size:21px;margin:0 0 3px 0}} .sub{{color:#555;margin:0 0 14px 0}}
.heads{{position:relative;height:44px;margin-bottom:2px}}
.lh{{position:absolute;transform:translateX(-50%);text-align:center;font-weight:600;font-size:13.5px;background:#fff;
    border:1px solid #c9ced4;border-radius:6px;padding:4px 10px;white-space:nowrap}}
.lh small{{font-weight:400;color:#555;font-family:"DejaVu Sans Mono",monospace;font-size:11.5px}}
svg text{{font-family:"DejaVu Sans Mono",monospace}} .lane{{stroke:#c9ced4;stroke-width:1.5;stroke-dasharray:4 4}}
.arrow{{stroke-width:1.8}} .time{{font-size:12px;fill:#666}} .lbl{{font-size:12.5px}}
.req{{stroke:#1f5fbf;fill:#1f5fbf}} .prov{{stroke:#7a7f86;fill:#5e636a}} .ok{{stroke:#1d7a3a;fill:#1d7a3a}} .err{{stroke:#b3261e;fill:#b3261e}}
text.req,text.prov,text.ok,text.err{{stroke:none}} .dup{{fill:#8a5a00;font-style:italic;font-size:11.5px}}
.gap{{font-size:12.5px;fill:#8a5a00;font-weight:bold}} .gapline{{stroke:#e2c98a;stroke-width:1;stroke-dasharray:2 3}}
.rtp{{fill:#e8f3ea;stroke:#8cc49a}} .rtplbl{{font-size:12px;fill:#1d5a2c}} ul{{margin:8px 0 0 0;padding-left:18px;font-size:13.5px}}
</style></head><body><div class="wrap"><h1>{html.escape(args.title)}</h1><p class="sub">{html.escape(args.subtitle)}</p>
<div class="heads">{heads}</div><svg width="{width}" height="{h}" xmlns="http://www.w3.org/2000/svg">{''.join(parts)}</svg>
<ul>{notes}</ul></div></body></html>"""


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("capture", help="tcpdump -A text output")
    ap.add_argument("--leaf", required=True, help="the leaf's IP address")
    ap.add_argument("--leaf-ports", default="6060,6061")
    ap.add_argument("--label", action="append", default=[], help="host:port=Name (repeatable)")
    ap.add_argument("--title", default="SIP call")
    ap.add_argument("--subtitle", default="")
    ap.add_argument("--note", action="append", default=[])
    ap.add_argument("--summary", help="real_pbx.py summary JSON, for the RTP count")
    ap.add_argument("--rtp-peer", help="host the RTP came from (draws the RTP band)")
    ap.add_argument("--out", required=True)
    args = ap.parse_args()
    open(args.out, "w", encoding="utf-8").write(render(args))


if __name__ == "__main__":
    main()
