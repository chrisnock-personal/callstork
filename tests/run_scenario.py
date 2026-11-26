#!/usr/bin/env python3
"""Run one CallStork wire scenario against a dispatcher (Kamailio or the leaf)."""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
from typing import Callable

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from sipfixture import (  # noqa: E402
    CRLF,
    FakePbx,
    Harness,
    Uac,
    split_addr,
    uri_hostport,
    uri_user,
)


def make_body(spec: object) -> str:
    if spec == "oversized":
        return "v=0" + CRLF + "x=" + ("x" * 1400) + CRLF
    if isinstance(spec, str):
        return spec
    return "v=0" + CRLF + "o=- 1 1 IN IP4 127.0.0.1" + CRLF


def build_fixtures(harness: Harness, scenario: dict, leaf: tuple[str, int]):
    uac_spec = scenario.get("uac", {})
    uac = Uac(
        harness,
        number=str(uac_spec.get("number", "447700900123")),
        leaf=leaf,
        from_user=str(uac_spec.get("from_user", "1000")),
        port=int(uac_spec.get("port", 0)),
        to_number=(
            str(uac_spec["to_number"]) if "to_number" in uac_spec else None
        ),
        max_forwards=int(uac_spec.get("max_forwards", 70)),
        body=make_body(uac_spec.get("body")),
        auto_ack=bool(uac_spec.get("auto_ack", True)),
    )
    harness.add(uac)

    pbx_spec = scenario.get("pbxs", {})
    pbxes: dict[str, FakePbx] = {}
    for key, default_port in (("a", 5071), ("b", 5072)):
        settings = pbx_spec.get(key, {})
        pbx = FakePbx(
            harness,
            name=f"pbx_{key}",
            behavior=settings.get("behavior", "answer_200"),
            port=int(settings.get("port", default_port)),
            enabled=bool(settings.get("enabled", True)),
            retransmit_200_after=(
                float(settings["retransmit_200_after"])
                if "retransmit_200_after" in settings
                else None
            ),
        )
        pbxes[key] = pbx
        harness.add(pbx)
    return uac, pbxes


def retry_until_dialog(harness: Harness, action: Callable[[], None], attempt: int = 0) -> None:
    """In-dialog actions may be scheduled before 200 arrives; retry briefly."""
    if action.__self__.dialog is not None:  # type: ignore[attr-defined]
        action()
        return
    if attempt >= 20:
        return
    harness.at(0.05, lambda: retry_until_dialog(harness, action, attempt + 1))


# Exit code for a scenario that could not run because a hook it needs was not
# given (the automake/ctest "skipped" convention).
EXIT_SKIP = 77

HOOK_ACTIONS = {"restart_leaf": "restart_cmd", "reload_config": "reload_cmd"}


def missing_hooks(scenario: dict, hooks: dict[str, str | None]) -> list[str]:
    """Hook actions the scenario needs that have no command configured."""
    return sorted(
        {
            step["action"]
            for step in scenario.get("steps", [])
            if step.get("action") in HOOK_ACTIONS
            and not hooks.get(HOOK_ACTIONS[step["action"]])
        }
    )


def run_hook(harness: Harness, action: str, command: str, log: list[dict]) -> None:
    """Run an operator hook synchronously; the event loop waits for it.

    Blocking is deliberate: nothing should reach the leaf while it restarts or
    reloads, and later steps simply run once the hook has returned.
    """
    started = harness.now
    result = subprocess.run(command, shell=True, capture_output=True, text=True)
    log.append(
        {
            "action": action,
            "command": command,
            "at": round(started, 3),
            "seconds": round(harness.now - started, 3),
            "returncode": result.returncode,
            "stderr": result.stderr.strip()[-500:],
        }
    )


def schedule_steps(
    harness: Harness,
    scenario: dict,
    uac: Uac,
    pbxes: dict[str, FakePbx],
    hooks: dict[str, str | None],
    hook_log: list[dict],
) -> None:
    for step in scenario.get("steps", []):
        at = float(step.get("at", 0))
        action = step.get("action", "note")

        if action == "invite":
            harness.at(at, uac.send_invite)
        elif action == "retransmit_invite":
            harness.at(at, uac.retransmit_invite)
        elif action == "cancel":
            harness.at(at, uac.cancel)
        elif action == "ack_final":
            harness.at(at, uac.ack_non2xx)
        elif action == "bye":
            harness.at(at, lambda: retry_until_dialog(harness, uac.bye))
        elif action == "reinvite":
            harness.at(at, lambda: retry_until_dialog(harness, uac.reinvite))
        elif action == "in_dialog_options":
            harness.at(at, lambda: retry_until_dialog(harness, uac.in_dialog_options))
        elif action == "pbx_bye":
            pbx = pbxes[step.get("pbx", "a")]
            harness.at(at, lambda p=pbx: p.send_bye(uac.call_id))
        elif action in HOOK_ACTIONS:
            command = hooks.get(HOOK_ACTIONS[action])
            if not command:
                raise ValueError(f"{action} needs --{HOOK_ACTIONS[action].replace('_', '-')}")
            harness.at(
                at,
                lambda a=action, c=command: run_hook(harness, a, c, hook_log),
            )
        elif action in ("note", "wait"):
            continue
        else:
            raise ValueError(f"unknown scenario action: {action!r}")


def as_list(value: object) -> list:
    if value is None:
        return []
    return value if isinstance(value, list) else [value]  # type: ignore[return-value]


def evaluate(
    scenario: dict, uac: Uac, pbxes: dict[str, FakePbx], hook_log: list[dict]
) -> tuple[bool, list[str], dict]:
    expected = scenario.get("expect", {})
    errors: list[str] = []
    observed = {
        "final_status": uac.final_status,
        "final_at": uac.final_at,
        "final_count": uac.final_count,
        "responses": [status for status, _ in uac.responses],
        "cancelled": uac.cancelled,
        "dialog": uac.dialog is not None,
        "record_routes": len(uac.dialog.route_set) if uac.dialog else 0,
        "received_requests": uac.received_requests,
        "pbx_invites": {key: len(pbx.invites_received) for key, pbx in pbxes.items()},
        "pbx_invite_uris": {
            key: [uri for uri, _ in pbx.invite_uris] for key, pbx in pbxes.items()
        },
        "dialog_responses": uac.dialog_responses,
        "pbx_received": {key: pbx.requests_received for key, pbx in pbxes.items()},
        "hooks": hook_log,
    }

    for hook in hook_log:
        if hook["returncode"] != 0:
            errors.append(
                f"{hook['action']} hook exited {hook['returncode']}: {hook['stderr']}"
            )

    if "final_after" in expected:
        low, high = expected["final_after"]
        if uac.final_at is None or not low <= uac.final_at <= high:
            errors.append(f"final response at {uac.final_at}s, expected {low}-{high}s")

    if "final_count" in expected and uac.final_count != expected["final_count"]:
        errors.append(
            f"final response arrived {uac.final_count} times, expected "
            f"{expected['final_count']} (retransmitted: ACK not absorbed?)"
        )

    for method, status in expected.get("dialog_responses", {}).items():
        actual = uac.dialog_responses.get(method)
        if actual != status:
            errors.append(f"in-dialog {method} got {actual}, expected {status}")

    for key, methods in expected.get("pbx_received", {}).items():
        for method in methods:
            if method not in pbxes[key].requests_received:
                errors.append(f"pbx_{key} did not receive {method}")

    # Always checked: an INVITE delivered to a PBX must be addressed to that
    # PBX. A failover attempt that keeps the original (leaf-addressed or
    # un-rewritten) Request-URI is a dispatcher bug even if the call completes.
    for key, pbx in pbxes.items():
        own = addr_str(pbx.address)
        for uri, _ in pbx.invite_uris:
            if uri_hostport(uri) != own:
                errors.append(f"pbx_{key} got INVITE {uri}, not addressed to {own}")

    if "ruri_user" in expected:
        for key, pbx in pbxes.items():
            for uri, in_dialog in pbx.invite_uris:
                if not in_dialog and uri_user(uri) != expected["ruri_user"]:
                    errors.append(
                        f"pbx_{key} got initial INVITE {uri}, "
                        f"expected user {expected['ruri_user']!r}"
                    )

    if "final_status" in expected:
        wanted = as_list(expected["final_status"])
        if uac.final_status not in wanted:
            errors.append(f"final status {uac.final_status} not in {wanted}")

    if "pbx_used" in expected:
        used = [key for key, pbx in pbxes.items() if pbx.invites_received]
        wanted = as_list(expected["pbx_used"])
        if used != wanted and not (wanted == [] and used == []):
            errors.append(f"PBXs used {used}, expected {wanted}")

    if "pbx_invites" in expected:
        for key, count in expected["pbx_invites"].items():
            actual = observed["pbx_invites"].get(key, 0)
            if actual != count:
                errors.append(f"pbx_{key} got {actual} INVITEs, expected {count}")

    if "cancelled" in expected and uac.cancelled != expected["cancelled"]:
        errors.append(f"cancelled={uac.cancelled}, expected {expected['cancelled']}")

    if "dialog" in expected and observed["dialog"] != expected["dialog"]:
        errors.append(f"dialog={observed['dialog']}, expected {expected['dialog']}")

    if "record_routes" in expected and observed["record_routes"] != expected["record_routes"]:
        errors.append(
            f"record_routes={observed['record_routes']}, "
            f"expected {expected['record_routes']}"
        )

    if "received_requests" in expected:
        for method in as_list(expected["received_requests"]):
            if method not in uac.received_requests:
                errors.append(f"UAC did not receive {method}")

    return not errors, errors, observed


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--leaf", default="127.0.0.1:5060", help="dispatcher host:port")
    parser.add_argument("--scenario", required=True, help="path to scenario JSON")
    parser.add_argument("--out", default="out/fixture", help="output directory")
    parser.add_argument("--duration", type=float, help="override scenario duration (seconds)")
    parser.add_argument(
        "--expect-record-routes",
        type=int,
        help="override the expected Record-Route count (e.g. 2 for double RR)",
    )
    parser.add_argument(
        "--restart-cmd",
        help="shell command that restarts the leaf (for restart_leaf steps)",
    )
    parser.add_argument(
        "--reload-cmd",
        help="shell command that reloads the leaf's config (for reload_config steps)",
    )
    parser.add_argument("--quiet", action="store_true")
    args = parser.parse_args()

    with open(args.scenario, encoding="utf-8") as handle:
        scenario = json.load(handle)
    if args.expect_record_routes is not None and "record_routes" in scenario.get("expect", {}):
        scenario["expect"]["record_routes"] = args.expect_record_routes

    name = scenario.get("name") or os.path.splitext(os.path.basename(args.scenario))[0]
    os.makedirs(args.out, exist_ok=True)
    leaf = split_addr(args.leaf)
    hooks = {"restart_cmd": args.restart_cmd, "reload_cmd": args.reload_cmd}

    missing = missing_hooks(scenario, hooks)
    if missing:
        # Without the hook the scenario would pass without testing anything.
        reason = "needs " + ", ".join(
            f"--{HOOK_ACTIONS[action].replace('_', '-')} ({action})" for action in missing
        )
        summary_path = os.path.join(args.out, f"{name}.summary.json")
        with open(summary_path, "w", encoding="utf-8") as handle:
            json.dump(
                {"scenario": name, "description": scenario.get("description", ""),
                 "leaf": addr_str(leaf), "result": "skip", "ok": False,
                 "skipped": reason},
                handle,
                indent=2,
            )
            handle.write("\n")
        if not args.quiet:
            print(f"SKIP {name}: {reason}")
        return EXIT_SKIP

    duration = float(args.duration or scenario.get("duration", 8))
    harness = Harness(duration)
    uac, pbxes = build_fixtures(harness, scenario, leaf)
    hook_log: list[dict] = []
    schedule_steps(harness, scenario, uac, pbxes, hooks, hook_log)
    harness.run()

    trace_path = os.path.join(args.out, f"{name}.sip.txt")
    summary_path = os.path.join(args.out, f"{name}.summary.json")
    harness.collector.write(trace_path)

    ok, errors, observed = evaluate(scenario, uac, pbxes, hook_log)
    summary = {
        "scenario": name,
        "description": scenario.get("description", ""),
        "leaf": addr_str(leaf),
        "result": "pass" if ok else "fail",
        "ok": ok,
        "errors": errors,
        "expected": scenario.get("expect", {}),
        "observed": observed,
        # Relative to the summary file, so summaries carry no host paths.
        "trace": os.path.basename(trace_path),
        "notes": scenario.get("notes", []),
    }
    with open(summary_path, "w", encoding="utf-8") as handle:
        json.dump(summary, handle, indent=2)
        handle.write("\n")

    if not args.quiet:
        status = "PASS" if ok else "FAIL"
        print(f"{status} {name}: {summary['description']}")
        print(f"  observed: {json.dumps(observed, sort_keys=True)}")
        if errors:
            for error in errors:
                print(f"  - {error}")
        print(f"  trace: {trace_path}")
        print(f"  summary: {summary_path}")
    return 0 if ok else 1


def addr_str(addr: tuple[str, int]) -> str:
    return f"{addr[0]}:{addr[1]}"


if __name__ == "__main__":
    raise SystemExit(main())
