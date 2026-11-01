# intercom-bridge

Lets [Kiosk Satellite](https://kiosksatellite.com) kiosks and [TECHO5](https://github.com/HuskerMinion/techo5)
devices (rooted Amazon Echo Shows, Spots and Dots running TECHO5 Linux) call each other on their built-in
intercoms.

Neither app is changed. The bridge runs as a small container on your network and speaks both intercom
protocols:

- Every TECHO5 device appears to the kiosks as **one more kiosk**: in **Call a kiosk**, in the intercom
  roster, and to Home Assistant's intercom actions.
- Every kiosk appears to the TECHO5 devices as **one more TECHO5 device**: callable by voice, from the
  call page, or with the `intercom_call` action.

When one side calls the other's stand-in, the bridge calls the real device and carries the audio
between them. Each device still does its own microphone, echo cancellation, speaker, ringing and call
screen. Kiosk-to-kiosk and TECHO5-to-TECHO5 calls are untouched: they work as they always have.

```
 Kiosk ──Kiosk Satellite intercom──▶ [stand-in kiosk "Garage Echo"]   ┐
                                                                     ├─ bridge ──TECHO5 intercom──▶ Garage Echo
 Kiosk ◀──Kiosk Satellite intercom── [stand-in device "Kitchen"]     ┘
```

Any number of kiosks and TECHO5 devices work, and several calls can run at once. Each device still
takes one call at a time, as it does on its own.

> Not affiliated with or endorsed by Kiosk Satellite or TECHO5. Both projects change often, and
> Kiosk Satellite does not promise its intercom protocol is stable; see [Compatibility](#compatibility).

## What you need

- **Kiosk Satellite** with the intercom on, an intercom key set, and the same key on every kiosk
  (a fleet shares it for you). **Intercom encryption (HTTPS) must be off**: the bridge speaks plain HTTP
  for now.
- **TECHO5** devices with the same **house word** set on each (their setup page). The house word is what
  TECHO5's intercom is encrypted with.
- A machine on the same network running Docker with **host networking**, so it can hear and send mDNS
  and accept the devices' connections. A NAS, a Raspberry Pi or the Home Assistant host all work.
- mDNS between the bridge and every device. On one network this just works. If kiosks or TECHO5 devices
  sit on another VLAN, see [Networks and VLANs](#networks-and-vlans).

## Quick start

```sh
mkdir intercom-bridge && cd intercom-bridge
curl -O https://raw.githubusercontent.com/josephyancey/intercom-bridge/main/docker-compose.example.yml
curl -o .env https://raw.githubusercontent.com/josephyancey/intercom-bridge/main/.env.example
mv docker-compose.example.yml docker-compose.yml
# fill in .env: the intercom key, the house word and this machine's IP address
docker compose up -d
docker compose logs -f
```

Within a minute the log shows each kiosk and TECHO5 device it found. Open **Call a kiosk** on a kiosk:
the TECHO5 devices are listed there. On a TECHO5 device, the kiosks are on the call page, and
"call the kitchen" works by voice.

The status page is at `http://<bridge>:8790/status`: the devices found, their stand-in ports, calls in
progress and the last error.

## Settings

Everything is set with environment variables.

| Variable | Default | What it is |
|---|---|---|
| `KS_INTERCOM_KEY` | *required* | The Kiosk Satellite intercom key. `KS_INTERCOM_KEY_FILE` reads it from a file (Docker secrets). |
| `TECHO5_HOUSE_WORD` | *required* | The TECHO5 house word. `TECHO5_HOUSE_WORD_FILE` reads it from a file. |
| `ADVERTISE_ADDRESS` | the address of the default route | The IP address devices reach the bridge on. Set it when the machine has several. |
| `MDNS_INTERFACE` | all | Limit mDNS to one network interface, like `eth0`. Also picks the default `ADVERTISE_ADDRESS`. |
| `LISTEN_ADDRESS` | all | The address stand-ins listen on. |
| `PORT_RANGE` | `41000-41099` | Ports for stand-ins, one per device. Kiosks and TECHO5 devices connect to these. |
| `STATUS_PORT` | `8790` | The status page. `0` turns it off. |
| `STATE_DIR` | `/data` | Where the bridge keeps each stand-in's port, so they stay the same across restarts. |
| `NAME_SUFFIX` | none | Added to TECHO5 device names in the kiosks' list, like ` (Echo)`. |
| `BROWSE_INTERVAL` | `30s` | How often the bridge looks for devices. One that misses three looks is dropped. |
| `MAX_CALL` | `2h` | The longest a call through the bridge may last. |
| `GAIN_TO_ECHO_DB` | `0` | Gain on the kiosk's voice going to the TECHO5 device, in dB. |
| `GAIN_TO_KIOSK_DB` | `0` | Gain on the TECHO5 device's voice going to the kiosk, in dB. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |
| `LOG_FORMAT` | `text` | `text` or `json`. |

## Networks and VLANs

The bridge, the kiosks and the TECHO5 devices must hear each other's mDNS, and the devices must reach
the bridge's `PORT_RANGE`. Kiosk Satellite only calls devices it discovered, so mDNS is not optional.

- **One network:** nothing to do.
- **Devices on another VLAN:** either run the bridge on that VLAN (a macvlan network, or a host there),
  or turn on your router's mDNS reflector between the networks (UniFi: *Settings > Networks > Multicast
  DNS*; OPNsense/pfSense: the Avahi plugin) and allow the devices to reach the bridge on `PORT_RANGE`.
  The bridge must also reach each kiosk's admin port (2324 by default), its intercom port and each
  TECHO5 device's web port.

## How a call goes

**Kiosk calls a TECHO5 device:** the kiosk rings the stand-in kiosk. The bridge calls the device over
TECHO5's intercom with the kiosk's name as the caller, and passes the device's first answer back
(ringing, busy, do not disturb). When the device is answered, the bridge tells the kiosk, the kiosk
opens its audio socket, and the bridge relays audio both ways.

**TECHO5 device calls a kiosk:** the device calls the kiosk's stand-in. The bridge checks the kiosk's
identity (intercom on, same key, no encryption, not in do not disturb) and rings it as the device's
stand-in kiosk. When the kiosk answers (or answers by itself), the bridge tells the device and opens the
kiosk's audio socket.

Audio is 16 kHz mono on both sides, so nothing is resampled or encoded. The bridge regroups the kiosk's
80 ms frames into TECHO5's 20 ms frames, paces them, and sends silence when a kiosk is quiet (push to
talk): a TECHO5 device hangs up on a call that goes silent for 5 seconds. It adds at most 80 ms of delay.

## Limits

- **Calls only.** A kiosk's *Announce to all* is turned away by the stand-ins, and TECHO5 announcements
  are not carried to kiosks.
- **No intercom encryption on the Kiosk Satellite side.** The TECHO5 side is always encrypted.
- **One bridge per network.** Two would each make stand-ins of the other's stand-ins.
- **Drop In:** a TECHO5 device with Drop In on answers by itself only for devices it knows, calling
  from their own address. Every stand-in calls from the bridge's address, so think about whether Drop In
  should reach kiosks before turning it on.
- A kiosk shows a TECHO5 device as a kiosk, so kiosk-only actions on it (opening its admin page, fleet
  settings) do nothing.

## Compatibility

Tested against:

| | Version |
|---|---|
| Kiosk Satellite | not yet tested on hardware |
| TECHO5 | not yet tested on hardware |

The protocols are implemented from Kiosk Satellite's documentation, from
[Kiosk Phone](https://github.com/mpjanofsky/kiosk-phone)'s verified protocol notes, and from TECHO5's
source. The tests run both call directions against a fake kiosk and a fake TECHO5 device built on
TECHO5's own handshake code. If an update to either app breaks calls, please open an issue with the
bridge's log at `LOG_LEVEL=debug`.

## Building

```sh
go test ./...
docker build -t intercom-bridge .
```

Pushing a `v*` tag builds a multi-architecture image (amd64, arm64, armv7) to GitHub Container Registry.

## License

MIT. `internal/t5/sealed.go` is from TECHO5 (MIT). See [THIRD_PARTY.md](THIRD_PARTY.md).
