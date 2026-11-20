# Naive outbound

The optional `naive` outbound runs the real Chromium/Cronet TLS, HTTP/2 and
HTTP/3 stack through [cronet-go](https://github.com/SagerNet/cronet-go). It does
not emulate Chromium with Go HTTP CONNECT or uTLS. No Naive inbound is included.

Normal builds and release archives do not enable this package.

## Licensing

Enabling this feature links GPL-3.0-or-later components: the `cronet-go` Go
bindings are compiled into the binary; native `libcronet` is linked statically
or loaded at runtime according to the build mode. A Naive-enabled binary is a GPLv3 combined work. Distributing
one requires GPLv3 compliance while the original Xray MPL-2.0 notices remain in
place. The opt-in does not relicense other source files and does not affect
ordinary builds. See the repository `LICENSE` and the `cronet-go` license for the
full texts.

## Building

Tested Linux amd64 dynamic build:

```sh
CGO_ENABLED=0 go build -tags with_naive_outbound,with_purego -o xray ./main
go mod download github.com/sagernet/cronet-go/lib/linux_amd64
cronet_lib=$(go list -m -f '{{.Dir}}' github.com/sagernet/cronet-go/lib/linux_amd64)
cp "$cronet_lib/libcronet.so" ./libcronet.so
```

Keep the native library beside the executable or in the loader's system/library
search path. Use the module version pinned in `go.mod`, not an arbitrary newer
release: the Go binding and the C ABI must match. `with_purego` removes the CGO
build requirement, **not** the native runtime dependency.

Windows amd64/arm64 uses the same tags and the matching `libcronet.dll`. Other
targets, CGO or static builds follow the
[cronet-go platform instructions](https://github.com/SagerNet/cronet-go/tree/0d28acc44093).
The tagged build path covered by this repository's CI is `with_purego`; CGO and
static builds are not covered here.

The build tag in `client.go` is the source of truth for supported targets:
Linux (non-Android) 386/amd64/arm/arm64/loong64/mipsle/riscv64, plus mips64le
without musl; Android 386/amd64/arm/arm64; Darwin amd64/arm64; and Windows
amd64/arm64. Other builds compile a stub and fail explicitly when a Naive
outbound is instantiated.

## Configuration

```json
{
  "tag": "naive-out",
  "protocol": "naive",
  "settings": {
    "address": "proxy.example.org",
    "port": 443,
    "user": "username",
    "pass": "password",
    "level": 0,
    "quic": false,
    "udpOverTcp": false,
    "headers": {}
  },
  "streamSettings": {
    "network": "raw",
    "security": "tls",
    "tlsSettings": {
      "serverName": "proxy.example.org"
    }
  }
}
```

## Behavior

- `address` and `port` are required. `quic: false` uses HTTP/2 over TCP;
  `quic: true` uses HTTP/3 over UDP to the proxy. Chromium owns authentication,
  Naive padding and stream multiplexing.
- With `quic: true`, optional `quicCongestionControl` selects `bbr`, `bbr2`,
  `cubic` or `reno`. Omission retains Chromium's default. An unknown value or
  specifying an algorithm without QUIC is rejected.
- UDP destinations require `udpOverTcp: true` and a server supporting sing
  UoT v2. HTTP/3 alone does not enable UDP destinations.
- Proxy addresses are resolved by Xray DNS, respecting
  `sockopt.domainStrategy`. Raw sockets retain Xray routing, accounting and the
  supported `streamSettings.sockopt` options, including `dialerProxy`. They are
  pinned to the configured proxy endpoint, separately from the Chromium SNI.
- Chromium's fixed IPv6 reachability probe is pinned to the configured endpoint
  as well. Pinning it keeps every physical socket on the proxy path while still
  allowing Chromium to accept AAAA results for the proxy name.
- `sendThrough` is supported as a fixed IP or CIDR. `origin` and `srcip` are
  rejected because a pooled HTTP/2 or HTTP/3 socket can serve streams from
  different inbound connections and has no per-stream source context.
- TLS permits `serverName` and custom root certificates via
  `certificates: [{"usage":"verify","certificateFile":"/path/to/ca.pem"}]`.
  Roots are loaded once when the outbound is created and replace Cronet's
  default trust roots when supplied. Certificate verification remains enabled.
- Other TLS overrides are rejected: uTLS fingerprints, custom ALPN, ECH,
  certificate pins and client certificates. Non-raw transports, transport masks,
  Mux.Cool/XUDP, `sockopt.addressPortStrategy` and enabled Xray
  `sockopt.happyEyeballs` racing are also rejected rather than silently changing
  Chromium's wire behavior.
- The binding does not expose TCP `CloseWrite`. Uplink EOF retains the response
  direction until remote EOF or the configured downlink-only timeout.
