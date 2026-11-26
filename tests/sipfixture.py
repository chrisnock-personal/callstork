"""Dependency-light SIP fixtures for CallStork wire tests.

The fixtures are deliberately not a SIP stack. They parse enough headers to
drive the wire scenarios in tests/scenarios/ and write verbatim message traces
(a timestamped `===` header line before each SIP message).

No third-party packages are used. All endpoints are UDP; IPv6, TLS and TCP are
out of scope for Phase 0.
"""

from __future__ import annotations

import heapq
import itertools
import re
import select
import socket
import time
import uuid
from dataclasses import dataclass, field
from typing import Callable, Iterable, Optional

CRLF = "\r\n"

REASONS = {
    100: "Trying",
    180: "Ringing",
    183: "Session Progress",
    200: "OK",
    404: "Not Found",
    405: "Method Not Allowed",
    407: "Proxy Authentication Required",
    408: "Request Timeout",
    413: "Request Entity Too Large",
    481: "Call/Transaction Does Not Exist",
    483: "Too Many Hops",
    486: "Busy Here",
    487: "Request Terminated",
    488: "Not Acceptable Here",
    500: "Internal Server Error",
    501: "Not Implemented",
    503: "Service Unavailable",
    513: "Message Too Large",
}

SDP = CRLF.join(
    [
        "v=0",
        "o=- 1 1 IN IP4 127.0.0.1",
        "s=-",
        "c=IN IP4 127.0.0.1",
        "t=0 0",
        "m=audio 40000 RTP/AVP 0",
        "a=rtpmap:0 PCMU/8000",
    ]
) + CRLF


def split_addr(value: str) -> tuple[str, int]:
    """Parse 'host:port' into (host, port)."""
    host, _, port = value.rpartition(":")
    if not host:
        raise ValueError(f"expected host:port, got {value!r}")
    return host, int(port)


def addr_str(addr: tuple[str, int]) -> str:
    return f"{addr[0]}:{addr[1]}"


def uri_user(uri: str) -> str:
    """Return the user part of a sip: URI, or '' when absent."""
    match = re.match(r"^\s*<?sip:[^>@:]*?([^@:>]*)(?::[^@>]*)?@", uri, re.I)
    if not match:
        match = re.match(r"^\s*<?sip:([^>@;>]+)", uri, re.I)
    return match.group(1) if match else ""


def uri_hostport(uri: str) -> str:
    """Return 'host:port' of a sip: URI (port defaults to 5060)."""
    match = re.match(r"^\s*<?sip:(?:[^@>]*@)?([^:;>]+)(?::(\d+))?", uri, re.I)
    if not match:
        return ""
    return f"{match.group(1)}:{match.group(2) or 5060}"


def header_value(header: str) -> str:
    """Return the URI inside a name-addr header, falling back to the raw value."""
    match = re.search(r"<([^>]+)>", header)
    return match.group(1) if match else header.split(";", 1)[0].strip()


def build_message(
    start_line: str, headers: Iterable[tuple[str, str]], body: str = ""
) -> bytes:
    lines = [start_line]
    lines.extend(f"{name}: {value}" for name, value in headers)
    lines.extend(["", ""])
    text = CRLF.join(lines)
    if body:
        text += body
    return text.encode("utf-8")


@dataclass
class SipMessage:
    raw: bytes
    is_response: bool
    method: str = ""
    uri: str = ""
    status: int = 0
    reason: str = ""
    headers: list[tuple[str, str]] = field(default_factory=list)
    body: str = ""

    @classmethod
    def parse(cls, raw: bytes) -> "SipMessage":
        text = raw.decode("utf-8", "replace").replace("\r\n", "\n").replace("\r", "\n")
        head, separator, body = text.partition("\n\n")
        if not separator:
            head, body = text, ""
        physical = head.split("\n")
        logical: list[str] = []
        for line in physical:
            if line[:1] in (" ", "\t") and logical:
                logical[-1] += line
            else:
                logical.append(line)
        if not logical:
            raise ValueError("empty SIP message")
        start = logical[0].strip()
        headers: list[tuple[str, str]] = []
        for line in logical[1:]:
            name, sep, value = line.partition(":")
            if sep:
                headers.append((name.strip(), value.strip()))
        if start.lower().startswith("sip/2.0 "):
            parts = start.split(None, 2)
            status = int(parts[1])
            reason = parts[2] if len(parts) > 2 else ""
            return cls(raw, True, status=status, reason=reason, headers=headers, body=body)
        parts = start.split(None, 2)
        if len(parts) != 3:
            raise ValueError(f"bad SIP start line: {start!r}")
        return cls(
            raw,
            False,
            method=parts[0].upper(),
            uri=parts[1],
            headers=headers,
            body=body,
        )

    def header(self, name: str, default: str = "") -> str:
        wanted = name.lower()
        for key, value in self.headers:
            if key.lower() == wanted:
                return value
        return default

    def headers_all(self, name: str) -> list[str]:
        wanted = name.lower()
        return [value for key, value in self.headers if key.lower() == wanted]

    @property
    def call_id(self) -> str:
        return self.header("Call-ID")

    @property
    def cseq(self) -> tuple[int, str]:
        parts = self.header("CSeq").split(None, 1)
        try:
            number = int(parts[0])
        except (IndexError, ValueError):
            number = 0
        return number, parts[1].upper() if len(parts) > 1 else ""

    @property
    def from_header(self) -> str:
        return self.header("From")

    @property
    def to_header(self) -> str:
        return self.header("To")

    @property
    def branch(self) -> str:
        via = self.header("Via")
        match = re.search(r";branch=([^;,\s]+)", via)
        return match.group(1) if match else ""

    @property
    def to_tag(self) -> str:
        match = re.search(r";tag=([^;,\s]+)", self.to_header)
        return match.group(1) if match else ""

    @property
    def from_tag(self) -> str:
        match = re.search(r";tag=([^;,\s]+)", self.from_header)
        return match.group(1) if match else ""

    @property
    def is_provisional(self) -> bool:
        return self.is_response and 100 <= self.status < 200

    @property
    def is_final(self) -> bool:
        return self.is_response and self.status >= 200


def new_tag() -> str:
    return uuid.uuid4().hex[:12]


def with_tag(header: str, tag: str) -> str:
    if re.search(r";tag=", header, re.I):
        return header
    return f"{header};tag={tag}"


def copy_response_headers(
    request: SipMessage,
    to_tag: str,
    extra: Optional[Iterable[tuple[str, str]]] = None,
    body: str = "",
) -> list[tuple[str, str]]:
    headers: list[tuple[str, str]] = []
    for name in ("Via", "From", "Call-ID", "CSeq", "Record-Route"):
        values = request.headers_all(name)
        headers.extend((name, value) for value in values)
    headers.append(("To", with_tag(request.to_header, to_tag)))
    if extra:
        headers.extend(extra)
    headers.append(("Content-Length", str(len(body.encode("utf-8")))))
    return headers


class TraceCollector:
    """Collect timestamped verbatim SIP messages from every fixture."""

    def __init__(self) -> None:
        self.start = time.monotonic()
        self.events: list[tuple[float, str]] = []
        self._sequence = itertools.count()

    def add(
        self,
        endpoint: str,
        direction: str,
        source: tuple[str, int],
        destination: tuple[str, int],
        raw: bytes,
    ) -> None:
        moment = time.monotonic()
        offset_ms = int((moment - self.start) * 1000)
        unix_ms = int(time.time() * 1000)
        header = (
            f"=== +{offset_ms:05d} ms (unix {unix_ms}) {direction} udp "
            f"{addr_str(source)} -> {addr_str(destination)} "
            f"len={len(raw)} redacted=no endpoint={endpoint}"
        )
        text = header + CRLF + raw.decode("utf-8", "replace").rstrip("\r\n") + CRLF
        self.events.append((moment, text))

    def write(self, path: str) -> None:
        with open(path, "w", encoding="utf-8") as handle:
            for _, text in sorted(self.events, key=lambda event: event[0]):
                handle.write(text)


class Harness:
    """Single-threaded UDP event loop with deterministic timer callbacks."""

    def __init__(self, duration: float = 8.0) -> None:
        self.duration = duration
        self.start = time.monotonic()
        self.collector = TraceCollector()
        self.endpoints: dict[str, "SipEndpoint"] = {}
        self._timers: list[tuple[float, int, Callable[[], None]]] = []
        self._counter = itertools.count()
        self._stop = False

    @property
    def now(self) -> float:
        return time.monotonic() - self.start

    def at(self, delay: float, callback: Callable[[], None]) -> None:
        """Schedule a callback `delay` seconds from now.

        Scenario steps are scheduled before the loop starts, so this is
        equivalent to scenario time. Fixtures scheduling a response after
        receiving a request rely on the relative behaviour.
        """
        if delay < 0:
            delay = 0
        heapq.heappush(
            self._timers,
            (time.monotonic() + delay, next(self._counter), callback),
        )

    def add(self, endpoint: "SipEndpoint") -> None:
        self.endpoints[endpoint.name] = endpoint

    def stop(self) -> None:
        self._stop = True

    def run(self) -> None:
        deadline = self.start + self.duration
        while not self._stop:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                break
            wait = remaining
            if self._timers:
                wait = min(wait, max(0.0, self._timers[0][0] - time.monotonic()))
            wait = min(wait, 0.1)
            readable = [ep.socket for ep in self.endpoints.values() if ep.socket is not None]
            if not readable:
                wait = min(wait, 0.01)
            ready, _, _ = select.select(readable, [], [], wait)
            for sock in ready:
                for ep in self.endpoints.values():
                    if ep.socket is sock:
                        data, peer = sock.recvfrom(65535)
                        ep.receive(data, peer)
                        break
            while self._timers and self._timers[0][0] <= time.monotonic():
                _, _, callback = heapq.heappop(self._timers)
                callback()


class SipEndpoint:
    def __init__(
        self,
        harness: Harness,
        name: str,
        bind_host: str = "127.0.0.1",
        port: int = 0,
        enabled: bool = True,
    ) -> None:
        self.harness = harness
        self.name = name
        self.address = (bind_host, port)
        self.socket: Optional[socket.socket] = None
        if enabled:
            sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
            sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            sock.bind((bind_host, port))
            sock.setblocking(False)
            self.socket = sock
            self.address = sock.getsockname()

    def send(self, raw: bytes, destination: tuple[str, int]) -> None:
        if self.socket is None:
            return
        self.socket.sendto(raw, destination)
        self.harness.collector.add(self.name, "SEND", self.address, destination, raw)

    def receive(self, raw: bytes, peer: tuple[str, int]) -> None:
        self.harness.collector.add(self.name, "RECV", peer, self.address, raw)
        message = SipMessage.parse(raw)
        self.handle(message, peer)

    def handle(self, message: SipMessage, peer: tuple[str, int]) -> None:
        raise NotImplementedError


@dataclass
class Dialog:
    local_uri: str
    remote_target: str
    route_set: list[str]
    call_id: str
    local_tag: str
    remote_tag: str
    cseq: int = 1


class Uac(SipEndpoint):
    """Caller-side fixture. Records responses and can drive in-dialog requests."""

    def __init__(
        self,
        harness: Harness,
        number: str,
        leaf: tuple[str, int],
        name: str = "uac",
        from_user: str = "1000",
        host: str = "127.0.0.1",
        port: int = 0,
        to_number: Optional[str] = None,
        max_forwards: int = 70,
        body: str = SDP,
        auto_ack: bool = True,
    ) -> None:
        super().__init__(harness, name, host, port)
        self.number = number
        self.to_number = to_number if to_number is not None else number
        self.leaf = leaf
        self.from_user = from_user
        self.local_uri = f"sip:{from_user}@{addr_str(self.address)}"
        self.max_forwards = max_forwards
        self.body = body
        self.auto_ack = auto_ack
        self.call_id = f"{new_tag()}@{host}"
        self.from_tag = new_tag()
        self.branch = "z9hG4bK" + new_tag()
        self.invite_message: Optional[bytes] = None
        self.responses: list[tuple[int, str]] = []
        self.final_status: Optional[int] = None
        # Seconds after harness start when the INVITE's final response arrived.
        self.final_at: Optional[float] = None
        # How many times the INVITE's final response arrived (retransmissions
        # mean the leaf never saw, or never absorbed, our ACK).
        self.final_count = 0
        self.final_to: Optional[str] = None
        # Headers of the INVITE's first final response.
        self.final_headers: list[tuple[str, str]] = []
        # 2xx responses to the INVITE that should never reach a caller: after
        # a non-2xx final, or from a second PBX (another To tag).
        self.stray_2xx = 0
        # Final status of each in-dialog request the UAC sent, by method
        # ("re-INVITE" for an in-dialog INVITE).
        self.dialog_responses: dict[str, int] = {}
        self.dialog: Optional[Dialog] = None
        self.cancelled = False
        self.acked_2xx = False
        self.received_requests: list[str] = []

    def _base_headers(self, method: str, branch: Optional[str] = None) -> list[tuple[str, str]]:
        return [
            (
                "Via",
                f"SIP/2.0/UDP {addr_str(self.address)};branch="
                f"{branch or self.branch};rport",
            ),
            ("Max-Forwards", str(self.max_forwards)),
            (
                "From",
                f"<sip:{self.from_user}@{self.address[0]}>;tag={self.from_tag}",
            ),
            ("To", f"<sip:{self.to_number}@127.0.0.1>"),
            ("Call-ID", self.call_id),
            ("CSeq", f"1 {method}"),
        ]

    def send_invite(self, extra_headers: Optional[Iterable[tuple[str, str]]] = None) -> None:
        headers = self._base_headers("INVITE")
        headers.append(("Contact", f"<{self.local_uri}>"))
        headers.append(("Content-Type", "application/sdp"))
        if extra_headers:
            headers.extend(extra_headers)
        self.invite_message = build_message(
            f"INVITE sip:{self.number}@{addr_str(self.leaf)} SIP/2.0",
            headers,
            self.body,
        )
        self.send(self.invite_message, self.leaf)

    def retransmit_invite(self) -> None:
        if self.invite_message is not None:
            self.send(self.invite_message, self.leaf)

    def cancel(self) -> None:
        headers = self._base_headers("CANCEL")
        self.send(build_message(f"CANCEL sip:{self.number}@{addr_str(self.leaf)} SIP/2.0", headers), self.leaf)
        self.cancelled = True

    def ack_non2xx(self, to_header: Optional[str] = None) -> None:
        """ACK a non-2xx final: same branch as the INVITE, To tag from the
        response (RFC 3261 §17.1.1.3). Defaults to the last final received."""
        headers = self._base_headers("ACK")
        to_header = to_header or self.final_to
        if to_header:
            headers = [(n, to_header if n == "To" else v) for n, v in headers]
        self.send(
            build_message(f"ACK sip:{self.number}@{addr_str(self.leaf)} SIP/2.0", headers),
            self.leaf,
        )

    def _send_dialog_request(self, method: str) -> None:
        dialog = self.dialog
        if dialog is None:
            return
        dialog.cseq += 1
        branch = "z9hG4bK" + new_tag()
        headers: list[tuple[str, str]] = [
            ("Via", f"SIP/2.0/UDP {addr_str(self.address)};branch={branch};rport"),
            ("Max-Forwards", str(self.max_forwards)),
            ("From", f"<{dialog.local_uri}>;tag={dialog.local_tag}"),
            ("To", f"<sip:{self.to_number}@127.0.0.1>;tag={dialog.remote_tag}"),
            ("Call-ID", self.call_id),
            ("CSeq", f"{dialog.cseq} {method}"),
        ]
        for route in dialog.route_set:
            headers.append(("Route", route))
        headers.append(("Contact", f"<{dialog.local_uri}>"))
        if method == "INVITE":
            headers.append(("Content-Type", "application/sdp"))
        self.send(
            build_message(f"{method} {dialog.remote_target} SIP/2.0", headers, self.body if method == "INVITE" else ""),
            self.leaf,
        )

    def bye(self) -> None:
        self._send_dialog_request("BYE")

    def reinvite(self) -> None:
        self._send_dialog_request("INVITE")

    def in_dialog_options(self) -> None:
        self._send_dialog_request("OPTIONS")

    def ack_2xx(self) -> None:
        dialog = self.dialog
        if dialog is None:
            return
        branch = "z9hG4bK" + new_tag()
        headers: list[tuple[str, str]] = [
            ("Via", f"SIP/2.0/UDP {addr_str(self.address)};branch={branch};rport"),
            ("Max-Forwards", str(self.max_forwards)),
            ("From", f"<{dialog.local_uri}>;tag={dialog.local_tag}"),
            ("To", f"<sip:{self.to_number}@127.0.0.1>;tag={dialog.remote_tag}"),
            ("Call-ID", self.call_id),
            ("CSeq", f"1 ACK"),
        ]
        for route in dialog.route_set:
            headers.append(("Route", route))
        self.send(
            build_message(f"ACK {dialog.remote_target} SIP/2.0", headers),
            self.leaf,
        )

    def _learn_dialog(self, message: SipMessage) -> None:
        contact = message.header("Contact")
        if not contact:
            return
        remote_target = header_value(contact)
        # RFC 3261 §12.1.2: the UAC stores the route set in reverse order.
        routes = [value for value in reversed(message.headers_all("Record-Route"))]
        self.dialog = Dialog(
            local_uri=self.local_uri,
            remote_target=remote_target,
            route_set=routes,
            call_id=self.call_id,
            local_tag=self.from_tag,
            remote_tag=message.to_tag,
            cseq=1,
        )

    def handle(self, message: SipMessage, peer: tuple[str, int]) -> None:
        if not message.is_response:
            self.received_requests.append(message.method)
            body = ""
            extra: list[tuple[str, str]] = []
            if message.method in ("BYE", "OPTIONS", "NOTIFY"):
                pass
            raw = build_message(
                f"SIP/2.0 200 {REASONS[200]}",
                copy_response_headers(message, new_tag(), extra, body),
                body,
            )
            self.send(raw, peer)
            return

        self.responses.append((message.status, message.reason))
        # Responses to CANCEL, BYE, re-INVITE etc. are not the initial call
        # outcome. Only the original INVITE (CSeq 1) determines final_status.
        if message.cseq[1] != "INVITE" or message.cseq[0] != 1:
            if message.status >= 200 and message.cseq[1] not in ("CANCEL", "ACK"):
                method = "re-INVITE" if message.cseq[1] == "INVITE" else message.cseq[1]
                self.dialog_responses.setdefault(method, message.status)
            return
        if message.status < 200:
            return
        if self.final_status is None:
            self.final_status = message.status
            self.final_at = round(self.harness.now, 3)
            self.final_headers = list(message.headers)
        if message.status == self.final_status:
            self.final_count += 1
        if 200 <= message.status < 300:
            # A 2xx after a final non-2xx belongs to an abandoned branch.
            if self.final_status >= 300:
                self.stray_2xx += 1
                return
            if self.dialog is not None and message.to_tag != self.dialog.remote_tag:
                self.stray_2xx += 1
                return
            self._learn_dialog(message)
            if self.auto_ack and not self.acked_2xx:
                self.acked_2xx = True
                self.ack_2xx()
        else:
            self.final_to = message.to_header
            # RFC 3261 §17.1.1.2: ACK the final and every retransmission of it.
            if self.auto_ack:
                self.ack_non2xx(message.to_header)


class FakePbx(SipEndpoint):
    """Controllable UAS standing in for a PBX pool member."""

    BEHAVIORS: dict[str, list[tuple[float, int]]] = {
        "answer_200": [(0.0, 200)],
        "answer_500": [(0.0, 500)],
        "answer_503": [(0.0, 503)],
        "reply_486": [(0.0, 486)],
        "reply_404": [(0.0, 404)],
        "silent": [],
        "delayed_200": [(0.8, 200)],
        "ring_until_cancel": [(0.15, 180)],
        "ring_then_200": [(0.15, 180), (0.8, 200)],
        "ring_then_500": [(0.15, 180), (0.8, 500)],
        "late_200": [(0.15, 180), (2.8, 200)],
        # No response until 200 at 2.5 s, after the leaf's 2 s attempt timer
        # has moved on: a late 2xx on an abandoned attempt.
        "silent_then_late_200": [(2.5, 200)],
        # 100 Trying then nothing: stops the attempt timer, so only the setup
        # deadline (no 18x within 8 s of the INVITE) ends the call.
        "trying_then_silent": [(0.0, 100)],
        # 100 Trying, then a 200 just after the 8 s setup deadline: it crosses
        # the leaf's CANCEL (so no 487), after the caller already has the
        # local 503.
        "trying_then_late_200": [(0.0, 100), (8.3, 200)],
        # Rings past the 8 s setup deadline; ringing must not be cut off.
        "ring_long_then_200": [(0.15, 180), (12.0, 200)],
    }

    def __init__(
        self,
        harness: Harness,
        name: str,
        behavior: str = "answer_200",
        port: int = 0,
        host: str = "127.0.0.1",
        enabled: bool = True,
        retransmit_200_after: Optional[float] = None,
    ) -> None:
        super().__init__(harness, name, host, port, enabled)
        if behavior not in self.BEHAVIORS:
            raise ValueError(f"unknown PBX behavior {behavior!r}")
        self.behavior = behavior
        self.retransmit_200_after = retransmit_200_after
        self.invites_received: list[str] = []
        # (Request-URI, in_dialog) for every new INVITE transaction received.
        self.invite_uris: list[tuple[str, bool]] = []
        # Methods of every other request received (BYE, OPTIONS, ...), and
        # "re-INVITE" for in-dialog INVITEs.
        self.requests_received: list[str] = []
        self.transactions: dict[tuple[str, str, str], dict] = {}
        self.dialogs: dict[str, dict] = {}
        self.local_uri = f"sip:{name}@{addr_str(self.address)}"

    def _key(self, message: SipMessage) -> tuple[str, str, str]:
        return (message.call_id, str(message.cseq[0]), message.branch)

    def _send_response(
        self,
        transaction: dict,
        status: int,
        extra: Optional[Iterable[tuple[str, str]]] = None,
    ) -> None:
        if transaction["cancelled"] and status != 487 and self.behavior not in ("late_200", "trying_then_late_200"):
            return
        request: SipMessage = transaction["request"]
        peer: tuple[str, int] = transaction["peer"]
        body = ""
        headers_extra: list[tuple[str, str]] = []
        if status == 200 and request.method == "INVITE":
            headers_extra.append(("Contact", f"<{self.local_uri}>"))
            headers_extra.append(("Content-Type", "application/sdp"))
            body = SDP
        if extra:
            headers_extra.extend(extra)
        raw = build_message(
            f"SIP/2.0 {status} {REASONS.get(status, 'Unknown')}",
            copy_response_headers(request, transaction["tag"], headers_extra, body),
            body,
        )
        transaction["sent"].append(raw)
        if status >= 200:
            transaction["final_status"] = status
        self.send(raw, peer)
        if status == 200 and request.method == "INVITE":
            self._store_dialog(request, peer, transaction["tag"])

    def _store_dialog(
        self, request: SipMessage, peer: tuple[str, int], local_tag: str
    ) -> None:
        contact = request.header("Contact")
        remote_target = header_value(contact) if contact else addr_str(peer)
        self.dialogs[request.call_id] = {
            "peer": peer,
            "remote_target": remote_target,
            # A UAS learns its route set from the INVITE's Record-Route
            # headers, stored in reverse order for subsequent requests.
            "route_set": list(reversed(request.headers_all("Record-Route"))),
            "from": request.from_header,
            "to": with_tag(request.to_header, local_tag),
            "call_id": request.call_id,
            "cseq": 0,
            "local_uri": self.local_uri,
        }

    def send_bye(self, call_id: str) -> None:
        dialog = self.dialogs.get(call_id)
        if dialog is None:
            return
        dialog["cseq"] += 1
        branch = "z9hG4bK" + new_tag()
        from_header = dialog["to"]
        to_header = dialog["from"]
        headers: list[tuple[str, str]] = [
            ("Via", f"SIP/2.0/UDP {addr_str(self.address)};branch={branch};rport"),
            ("Max-Forwards", "70"),
            ("From", from_header),
            ("To", to_header),
            ("Call-ID", dialog["call_id"]),
            ("CSeq", f"{dialog['cseq']} BYE"),
        ]
        for route in dialog["route_set"]:
            headers.append(("Route", route))
        self.send(
            build_message(f"BYE {dialog['remote_target']} SIP/2.0", headers),
            dialog["peer"],
        )

    def handle(self, message: SipMessage, peer: tuple[str, int]) -> None:
        if message.is_response:
            return
        if message.method == "OPTIONS":
            # Out-of-dialog OPTIONS are the dispatcher's health probes; only
            # in-dialog ones (To tag present) count as received requests.
            if message.to_tag:
                self.requests_received.append("OPTIONS")
            extra = [("Allow", "INVITE, ACK, CANCEL, BYE, OPTIONS")]
            raw = build_message(
                f"SIP/2.0 200 {REASONS[200]}",
                copy_response_headers(message, new_tag(), extra),
            )
            self.send(raw, peer)
            return

        key = self._key(message)
        if message.method == "INVITE":
            if key in self.transactions:
                for raw in self.transactions[key]["sent"]:
                    self.send(raw, peer)
                return
            self.invites_received.append(message.call_id)
            self.invite_uris.append((message.uri, bool(message.to_tag)))
            if message.to_tag:
                self.requests_received.append("re-INVITE")
            transaction = {
                "request": message,
                "peer": peer,
                "tag": new_tag(),
                "sent": [],
                "cancelled": False,
                "final_status": 0,
            }
            self.transactions[key] = transaction
            if self.behavior == "silent":
                return
            for delay, status in self.BEHAVIORS[self.behavior]:
                self.harness.at(delay, lambda s=status, t=transaction: self._send_response(t, s))
            if self.retransmit_200_after is not None:
                self.harness.at(
                    self.retransmit_200_after,
                    lambda t=transaction: self._send_response(t, 200),
                )
            return

        if message.method == "CANCEL":
            raw = build_message(
                f"SIP/2.0 200 {REASONS[200]}",
                copy_response_headers(message, new_tag()),
            )
            self.send(raw, peer)
            for transaction in self.transactions.values():
                request: SipMessage = transaction["request"]
                if (
                    request.call_id == message.call_id
                    and not transaction["cancelled"]
                    and transaction["final_status"] == 0
                    # Its 200 is already on the wire: no 487 (RFC 3261 9.2).
                    and self.behavior != "trying_then_late_200"
                ):
                    transaction["cancelled"] = True
                    self.harness.at(
                        0.0,
                        lambda t=transaction: self._send_response(
                            t, 487, [("Reason", "SIP;cause=200;text=Call completed elsewhere")]
                        ),
                    )
            return

        if message.method == "ACK":
            # The ACK for a non-2xx final reuses the INVITE's transaction
            # (same branch); the ACK for a 2xx is a new transaction.
            self.requests_received.append("ACK" if key in self.transactions else "ACK-2xx")
            return

        self.requests_received.append(message.method)
        if message.method in ("BYE", "OPTIONS", "INFO", "UPDATE"):
            raw = build_message(
                f"SIP/2.0 200 {REASONS[200]}",
                copy_response_headers(message, new_tag()),
            )
            self.send(raw, peer)
            return

        raw = build_message(
            f"SIP/2.0 501 {REASONS[501]}",
            copy_response_headers(message, new_tag()),
        )
        self.send(raw, peer)
