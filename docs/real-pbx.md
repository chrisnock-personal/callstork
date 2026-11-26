# Testing against a real PBX

The wire scenarios in `tests/scenarios/` use fake PBXs. This guide runs the
Kamailio reference against a **real** PBX instead, with seven calls that cover
answering, both hang-ups, busy, an unknown number, failover from a dead pool
member, CANCEL while ringing, and the 8 s setup deadline. It was written while
testing against FreePBX 17 (Asterisk 22); any Asterisk with PJSIP works the
same way, and other PBXs need the equivalent endpoint and dialplan.

Addresses below are placeholders: `192.0.2.5` is the host running the leaf and
the test caller, `192.0.2.20` is the PBX. Use your own.

## What you need

- A Linux host with Podman, Python 3 and the repository, on a network the PBX
  can reach. The leaf listens on `LEAF_ADDR:6060` and `:6061`; the test caller
  uses `LEAF_ADDR` too (or `CALLER_ADDR`), with RTP on port 40000.
- Root on the PBX, or its admin UI, to add an endpoint and a dialplan context.
- Optional: `sudo` for `tcpdump`, to capture the calls and draw them.

## 1. Prepare the PBX

Back up the two files first; you'll put them back at the end.

```sh
cp -p /etc/asterisk/pjsip_custom_post.conf /root/pjsip_custom_post.conf.before-leaf-test
cp -p /etc/asterisk/extensions_custom.conf /root/extensions_custom.conf.before-leaf-test
```

### An inbound endpoint for the leaf

Append to `/etc/asterisk/pjsip_custom_post.conf` (on FreePBX this file is
included last and is empty by default):

```ini
; BEGIN leaf-test
[leaf-test]
type=endpoint
context=leaf-test
transport=0.0.0.0-udp
disallow=all
allow=ulaw,alaw
dtmf_mode=rfc4733
direct_media=no

[leaf-test-ip]
type=identify
endpoint=leaf-test
match=192.0.2.5:6060,192.0.2.5:6061
; END leaf-test
```

`transport=` must name one of the PBX's existing transports
(`pjsip show transports`).

**Identify the leaf by address and port, not by header.** The leaf adds an
`X-Inbound-Rule` header, and PJSIP can match on it (`match_header`), but
FreePBX sets `endpoint_identifier_order` to `ip,username,anonymous,header`.
With anonymous inbound SIP allowed, the `anonymous` endpoint claims the call
before `header` is consulted, and the call lands in `from-trunk` as `s` ("the
number you have dialled…"). `match=<ip>:<port>` is handled by `ip`, which runs
first. Including the ports keeps other SIP traffic from the same host (a
softphone, another test tool) out of this endpoint.

### A dialplan context with one number per behaviour

The leaf's `inbound-dids` rule rewrites `44XXXXXXXXXX` to `0XXXXXXXXXX`, and
`lab-failover` rewrites `447700900999` to `07700900999`. Append to
`/etc/asterisk/extensions_custom.conf`:

```ini
; BEGIN leaf-test
[leaf-test]
; ring 2 s, answer, play a prompt, the PBX hangs up
exten => 07700900100,1,Ringing()
 same => n,Wait(2)
 same => n,Answer()
 same => n,Playback(hello-world)
 same => n,Wait(1)
 same => n,Hangup()
; busy (486)
exten => 07700900101,1,Busy(5)
; ring until cancelled
exten => 07700900102,1,Ringing()
 same => n,Wait(60)
 same => n,Hangup()
; answer at once and stay up until the caller hangs up
exten => 07700900103,1,Answer()
 same => n,Playback(hello-world)
 same => n,Wait(30)
 same => n,Hangup()
; 100 Trying only, no 18x: the leaf's setup deadline must end it
exten => 07700900104,1,Wait(20)
 same => n,Hangup()
; lab-failover rewrites 447700900999 to this
exten => 07700900999,1,Goto(07700900100,1)
; END leaf-test
```

Any number not listed (the `unknown_404` case dials `07700900555`) gets a 404.

Load both and check:

```sh
asterisk -rx "module reload res_pjsip.so"
asterisk -rx "dialplan reload"
asterisk -rx "pjsip show identifies"       # leaf-test-ip with the match
asterisk -rx "dialplan show leaf-test"     # 6 extensions
```

If the PBX runs a firewall (FreePBX does), the leaf's address must be trusted
or allowed for SIP.

## 2. Run the cases

```sh
LEAF_ADDR=192.0.2.5 PBX=192.0.2.20:5060 tests/run_real_pbx.sh out/real-pbx
```

Each case gets a fresh Kamailio container (host networking) with its own
`address.list` (the caller) and `dispatcher.list`: pool 1 is the PBX, pool 2
is an unreachable member (`DEAD_MEMBER`, default `192.0.2.1:5060`) followed by
the PBX. Expected output:

```
PASS  answer_pbx_bye         447700900100  100 180 200     final 200 at  2.025 s  | PBX hung up, BYE relayed; RTP 71 pkts direct from 192.0.2.20
PASS  answer_uac_bye         447700900103  100 200         final 200 at  0.008 s  | caller BYE -> 200; RTP 71 pkts direct from 192.0.2.20
PASS  busy_486               447700900101  100 486         final 486 at  0.016 s
PASS  unknown_404            447700900555  100 404         final 404 at  0.003 s
PASS  failover_dead_member   447700900999  100 180 200     final 200 at  4.007 s  | PBX hung up, BYE relayed; RTP 71 pkts direct from 192.0.2.20
PASS  cancel_ringing         447700900102  100 180 487     final 487 at  2.007 s
PASS  deadline_503           447700900104  100 503         final 503 at  7.954 s
0 case(s) failed
```

`out/real-pbx/` holds each case's caller-side trace (`<case>.sip.txt`), a JSON
summary and the leaf's script log. To run some cases only:
`CASES="busy_486 deadline_503"`. The cases and their numbers are in
`tests/real_pbx_cases.json`.

After the run, the PBX should have no channels left: `asterisk -rx "core show
channels"`.

## 3. Optional: capture and draw a call

```sh
CAPTURE=1 LEAF_ADDR=192.0.2.5 PBX=192.0.2.20:5060 CASES=failover_dead_member \
  tests/run_real_pbx.sh out/real-pbx
python3 tests/tools/ladder.py out/real-pbx/failover_dead_member.tcpdump.txt \
  --leaf 192.0.2.5 --label 192.0.2.1:5060="Dead member" --label 192.0.2.20:5060="PBX" \
  --summary out/real-pbx/failover_dead_member.summary.json --rtp-peer 192.0.2.20 \
  --title "Failover to the PBX" --out failover.html
```

The capture is taken on the leaf's host, so it shows each packet once as it
left. `ladder.py` draws the caller, the leaf and every other address as lanes,
marks gaps over 0.4 s, and labels packets repeated within 20 ms as copies.

## 4. Remove it again

```sh
cp -p /root/pjsip_custom_post.conf.before-leaf-test /etc/asterisk/pjsip_custom_post.conf
cp -p /root/extensions_custom.conf.before-leaf-test /etc/asterisk/extensions_custom.conf
asterisk -rx "module reload res_pjsip.so"
asterisk -rx "dialplan reload"
asterisk -rx "pjsip show endpoint leaf-test"   # "Unable to find object"
rm /root/*.before-leaf-test
```

The script removes its own container and temporary files.

## Things to know

- **Duplicated packets in some labs.** With the leaf in WSL2 (mirrored
  networking) and the PBX in a VMware bridged VM on the same Windows host,
  every packet from WSL to the VM arrived twice. SIP treats the copies as
  retransmissions: the PBX answers each INVITE twice and the leaf may relay a
  duplicate final response at once. The cases allow that, but fail if the leaf
  retransmits a final response on its 500 ms timer, which would mean the
  caller's ACK wasn't absorbed. Capture on the PBX side to see the copies.
- **Media goes direct.** The leaf only carries signalling. The caller offers
  real SDP and counts RTP; every packet must come from the PBX's own address.
- **What isn't covered:** TCP/TLS, a registering trunk, and a real carrier in
  front of the leaf (#10, #11, #12).
