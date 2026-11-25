import os
import tempfile
import unittest

from sipfixture import (
    CRLF,
    SipMessage,
    TraceCollector,
    build_message,
    header_value,
    new_tag,
    split_addr,
    uri_hostport,
    uri_user,
    with_tag,
)


class FixtureTests(unittest.TestCase):
    def test_split_addr(self):
        self.assertEqual(split_addr("127.0.0.1:5060"), ("127.0.0.1", 5060))

    def test_uri_and_header_helpers(self):
        self.assertEqual(uri_user("<sip:447700900123@leaf.example>"), "447700900123")
        self.assertEqual(uri_user("sip:447700900123@127.0.0.1:5060"), "447700900123")
        self.assertEqual(
            header_value('<sip:1000@127.0.0.1:5000>;transport=udp'),
            "sip:1000@127.0.0.1:5000",
        )
        self.assertEqual(with_tag("<sip:a@b>", "xyz"), "<sip:a@b>;tag=xyz")

    def test_uri_hostport(self):
        self.assertEqual(uri_hostport("sip:07700900999@127.0.0.1:5072"), "127.0.0.1:5072")
        self.assertEqual(uri_hostport("sip:127.0.0.1:5072"), "127.0.0.1:5072")
        self.assertEqual(uri_hostport("<sip:pbx_a@127.0.0.1:5071;lr>"), "127.0.0.1:5071")
        self.assertEqual(uri_hostport("sip:leaf.example;transport=udp"), "leaf.example:5060")
        self.assertEqual(with_tag("<sip:a@b>;tag=old", "xyz"), "<sip:a@b>;tag=old")

    def test_parse_request(self):
        raw = build_message(
            "INVITE sip:+447700900123@127.0.0.1:5060 SIP/2.0",
            [
                ("Via", "SIP/2.0/UDP 127.0.0.1:5000;branch=z9hG4bKabc;rport"),
                ("Max-Forwards", "70"),
                ("From", "<sip:1000@127.0.0.1>;tag=from1"),
                ("To", "<sip:+447700900123@127.0.0.1>"),
                ("Call-ID", "call1@example"),
                ("CSeq", "1 INVITE"),
                ("Record-Route", "<sip:127.0.0.1:5060;lr>"),
                ("Record-Route", "<sip:127.0.0.1:5061;lr>"),
            ],
            "v=0" + CRLF,
        )
        msg = SipMessage.parse(raw)
        self.assertFalse(msg.is_response)
        self.assertEqual(msg.method, "INVITE")
        self.assertEqual(msg.branch, "z9hG4bKabc")
        self.assertEqual(msg.call_id, "call1@example")
        self.assertEqual(msg.cseq, (1, "INVITE"))
        self.assertEqual(msg.from_tag, "from1")
        self.assertEqual(msg.to_tag, "")
        self.assertEqual(len(msg.headers_all("Record-Route")), 2)
        self.assertEqual(msg.body, "v=0\n")  # parser normalises CRLF to LF

    def test_parse_response(self):
        raw = (
            "SIP/2.0 486 Busy Here" + CRLF
            + "Via: SIP/2.0/UDP 127.0.0.1:5000;branch=z9hG4bKabc;rport" + CRLF
            + "From: <sip:1000@127.0.0.1>;tag=from1" + CRLF
            + "To: <sip:447700900123@127.0.0.1>;tag=to1" + CRLF
            + "Call-ID: call1@example" + CRLF
            + "CSeq: 1 INVITE" + CRLF
            + "Content-Length: 0" + CRLF + CRLF
        ).encode()
        msg = SipMessage.parse(raw)
        self.assertTrue(msg.is_response)
        self.assertTrue(msg.is_final)
        self.assertEqual(msg.status, 486)
        self.assertEqual(msg.reason, "Busy Here")
        self.assertEqual(msg.cseq, (1, "INVITE"))
        self.assertEqual(msg.to_tag, "to1")

    def test_trace_uses_expected_shape(self):
        collector = TraceCollector()
        collector.add(
            "uac", "SEND", ("127.0.0.1", 5000), ("127.0.0.1", 5060),
            b"INVITE sip:447700900123@127.0.0.1:5060 SIP/2.0\r\n\r\n",
        )
        with tempfile.TemporaryDirectory() as directory:
            path = os.path.join(directory, "trace.sip.txt")
            collector.write(path)
            with open(path, encoding="utf-8") as handle:
                text = handle.read()
        self.assertRegex(
            text,
            r"^=== \+\d{5} ms \(unix \d+\) SEND udp 127\.0\.0\.1:5000 -> 127\.0\.0\.1:5060 len=\d+ redacted=no endpoint=uac\n",
        )
        self.assertIn("INVITE sip:447700900123@127.0.0.1:5060 SIP/2.0", text)

    def test_new_tag_is_unique(self):
        self.assertNotEqual(new_tag(), new_tag())


if __name__ == "__main__":
    unittest.main()
