#!/usr/bin/env python3
"""Place one real call through a running leaf to a real PBX and check it.

Unlike run_scenario.py there are no fake PBXs: the leaf's pool is a real PBX
(see docs/real-pbx.md for preparing one). The caller offers real SDP, counts
the RTP that arrives and checks it comes straight from the PBX, not through
the leaf. Every address is an argument; nothing defaults to a real network.

    python3 tests/real_pbx.py --leaf 192.0.2.5:6060 --caller-host 192.0.2.5 \\
        --pbx-host 192.0.2.20 --case answer_pbx_bye --out out/real-pbx
"""

from __future__ import annotations

import argparse
import json
import os
import socket
import sys
import threading
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from sipfixture import CRLF, Harness, Uac, split_addr  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))


def sdp(host: str, rtp_port: int) -> str:
    return CRLF.join([
        "v=0", f"o=- 1 1 IN IP4 {host}", "s=real-pbx-test", f"c=IN IP4 {host}", "t=0 0",
        f"m=audio {rtp_port} RTP/AVP 0 8 101", "a=rtpmap:0 PCMU/8000", "a=rtpmap:8 PCMA/8000",
        "a=rtpmap:101 telephone-event/8000", "a=sendrecv", ""])


def trace_blocks(trace: str) -> list[list[str]]:
    blocks, block = [], []
    for line in open(trace, encoding="utf-8").read().splitlines():
        if line.startswith("=== "):
            if block:
                blocks.append(block)
            block = [line]
        else:
            block.append(line)
    if block:
        blocks.append(block)
    return blocks


def invite_finals(trace: str) -> tuple[list[int], list[int]]:
    """(statuses of responses to the caller's INVITE, ms arrival times of its final)."""
    statuses, times = [], []
    for block in trace_blocks(trace):
        head = block[0]
        if " RECV " not in head or not head.endswith("endpoint=uac") or len(block) < 2:
            continue
        if not block[1].startswith("SIP/2.0 ") or "CSeq: 1 INVITE" not in block:
            continue
        status = int(block[1].split()[1])
        statuses.append(status)
        if status >= 200:
            times.append(int(head.split()[1].lstrip("+")))
    return statuses, times


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--leaf", required=True, help="leaf peer-facing host:port")
    ap.add_argument("--caller-host", required=True, help="address the caller binds to (must be in the leaf's address.list)")
    ap.add_argument("--pbx-host", required=True, help="the PBX's address, the only valid RTP source")
    ap.add_argument("--rtp-port", type=int, default=40000)
    ap.add_argument("--cases", default=os.path.join(HERE, "real_pbx_cases.json"))
    ap.add_argument("--case", required=True)
    ap.add_argument("--out", default="out/real-pbx")
    args = ap.parse_args()

    case = json.load(open(args.cases, encoding="utf-8"))["cases"][args.case]
    expect = case["expect"]
    os.makedirs(args.out, exist_ok=True)

    rtp = {"count": 0, "sources": set()}
    sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    sock.bind((args.caller_host, args.rtp_port))
    sock.settimeout(0.2)
    stop = threading.Event()

    def rtp_loop() -> None:
        while not stop.is_set():
            try:
                _, peer = sock.recvfrom(2048)
            except socket.timeout:
                continue
            rtp["count"] += 1
            rtp["sources"].add(peer[0])

    threading.Thread(target=rtp_loop, daemon=True).start()

    harness = Harness(float(case.get("duration", 8)))
    uac = Uac(harness, number=case["number"], leaf=split_addr(args.leaf),
              host=args.caller_host, body=sdp(args.caller_host, args.rtp_port))
    harness.add(uac)
    harness.at(0, uac.send_invite)
    for at, action in case.get("steps", []):
        harness.at(at, getattr(uac, action))
    harness.run()
    stop.set()
    time.sleep(0.3)
    sock.close()

    trace = os.path.join(args.out, f"{args.case}.sip.txt")
    harness.collector.write(trace)
    statuses, final_times = invite_finals(trace)

    errors = []
    if uac.final_status != expect["final"]:
        errors.append(f"final {uac.final_status}, expected {expect['final']}")
    if expect.get("ringing") and not any(180 <= s < 190 for s in statuses):
        errors.append("no 18x relayed")
    if expect.get("ack_absorbed"):
        # A PBX may legitimately send a final twice (e.g. it saw a duplicated
        # INVITE) and the leaf may relay the copy at once. What must not
        # happen is the leaf retransmitting it on its 500 ms timer, which
        # would mean the caller's ACK was not absorbed.
        late = [t for t in final_times if t - final_times[0] >= 450]
        if late:
            errors.append(f"final retransmitted at {late} ms: ACK not absorbed")
    if expect.get("pbx_bye") and "BYE" not in uac.received_requests:
        errors.append("the PBX's BYE did not reach the caller")
    if "caller_bye" in expect and uac.dialog_responses.get("BYE") != expect["caller_bye"]:
        errors.append(f"caller's BYE got {uac.dialog_responses.get('BYE')}, expected {expect['caller_bye']}")
    if expect.get("rtp_direct") and (rtp["count"] == 0 or rtp["sources"] != {args.pbx_host}):
        errors.append(f"RTP: {rtp['count']} packets from {sorted(rtp['sources'])}, "
                      f"expected some, all from {args.pbx_host}")
    if "final_after" in expect:
        low, high = expect["final_after"]
        if uac.final_at is None or not low <= uac.final_at <= high:
            errors.append(f"final at {uac.final_at} s, expected {low}-{high} s")

    summary = {
        "case": args.case, "description": case.get("description", ""), "number": case["number"],
        "ok": not errors, "errors": errors, "final": uac.final_status, "final_at": uac.final_at,
        "invite_responses": statuses, "received": uac.received_requests,
        "dialog_responses": uac.dialog_responses,
        "record_routes": len(uac.dialog.route_set) if uac.dialog else 0,
        "rtp_packets": rtp["count"], "rtp_from": sorted(rtp["sources"]), "trace": os.path.basename(trace),
    }
    with open(os.path.join(args.out, f"{args.case}.summary.json"), "w", encoding="utf-8") as fh:
        json.dump(summary, fh, indent=2)
        fh.write("\n")

    seen: list[int] = []
    for status in statuses:
        if not seen or seen[-1] != status:
            seen.append(status)
    notes = []
    if "BYE" in uac.received_requests:
        notes.append("PBX hung up, BYE relayed")
    if "BYE" in uac.dialog_responses:
        notes.append(f"caller BYE -> {uac.dialog_responses['BYE']}")
    if rtp["count"]:
        notes.append(f"RTP {rtp['count']} pkts direct from {','.join(sorted(rtp['sources']))}")
    final_at = f"{uac.final_at:6.3f}" if uac.final_at is not None else "  none"
    print(f"{'PASS' if not errors else 'FAIL'}  {args.case:<22} {case['number']}  "
          f"{' '.join(map(str, seen)):<15} final {uac.final_status} at {final_at} s"
          + ("  | " + "; ".join(notes) if notes else "")
          + ("  !! " + "; ".join(errors) if errors else ""))
    return 0 if not errors else 1


if __name__ == "__main__":
    raise SystemExit(main())
