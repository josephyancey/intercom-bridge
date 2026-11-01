# Third-party notices

## TECHO5

`internal/t5/sealed.go` is copied from TECHO5 (https://github.com/HuskerMinion/techo5),
`echod/internal/lib/sealed/sealed.go`, so the bridge performs the same handshake TECHO5 devices do.

```
MIT License

Copyright (c) 2026 TECHO5 contributors

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

The TECHO5 intercom protocol in `internal/t5/link.go` follows TECHO5's
`echod/internal/feature/phone/intercom.go` (same license).

## Kiosk Satellite

No Kiosk Satellite code is included. The Kiosk Satellite intercom protocol is implemented from
Kiosk Satellite's published documentation and from the protocol notes of Kiosk Phone
(https://github.com/mpjanofsky/kiosk-phone, Apache-2.0), which verified it against real kiosks.

## Go modules

- github.com/flynn/noise (BSD-3-Clause)
- github.com/gorilla/websocket (BSD-2-Clause)
- github.com/libp2p/zeroconf/v2 (MIT)
