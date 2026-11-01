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

Find two things first:

- **The Kiosk Satellite intercom key:** on any kiosk, **Settings > Intercom > Intercom key** (or the
  remote admin's Intercom page). Every kiosk has the same one.
- **The TECHO5 house word:** on a TECHO5 device's setup page, **Sound & Voice > Announcements > House
  word**. If none is set yet, set one (16 characters or more) on every TECHO5 device: their intercom stays
  off without it.

Then make a folder with this `docker-compose.yml`, put in the two secrets and the IP address of the
machine it runs on, and start it:

```yaml
services:
  intercom-bridge:
    image: ghcr.io/josephyancey/intercom-bridge:latest
    container_name: intercom-bridge
    network_mode: host          # required: mDNS, and devices connect straight to its ports
    restart: unless-stopped
    environment:
      KS_INTERCOM_KEY: "paste-the-kiosk-intercom-key-here"
      TECHO5_HOUSE_WORD: "paste-the-techo5-house-word-here"
      ADVERTISE_ADDRESS: 192.168.1.10   # this machine's IP address
      # Optional:
      # MDNS_INTERFACE: eth0            # when the machine has several network interfaces
      # SILENCE_HANGUP: 15s             # end calls nobody is talking on
      # NAME_SUFFIX: " (Echo)"          # how TECHO5 devices are named in the kiosks' list
      # TZ: America/New_York            # timestamps in the log
    volumes:
      - ./data:/data                    # keeps each stand-in on the same port across restarts
```

```sh
docker compose up -d
docker compose logs -f
```

To keep the secrets out of the compose file, put them in a file next to it instead, say `bridge.env`:

```sh
KS_INTERCOM_KEY=paste-the-kiosk-intercom-key-here
TECHO5_HOUSE_WORD=paste-the-techo5-house-word-here
ADVERTISE_ADDRESS=192.168.1.10
```

and replace the `environment:` block with `env_file: bridge.env`. With Docker secrets, the bridge also
reads `KS_INTERCOM_KEY_FILE` and `TECHO5_HOUSE_WORD_FILE`.

Before starting it, you can check that the machine hears both kinds of device:

```sh
docker run --rm --network host ghcr.io/josephyancey/intercom-bridge:latest discover
```

It lists every kiosk and TECHO5 device it can hear in five seconds, without needing the secrets or
announcing anything. A side missing from the list means mDNS is not reaching this machine; see
[Networks and VLANs](#networks-and-vlans).

Within a minute the log shows each kiosk and TECHO5 device it found. Open **Call a kiosk** on a kiosk:
the TECHO5 devices are listed there. On a TECHO5 device, the kiosks are on the call page, and
"call the kitchen" works by voice.

The status page is at `http://<bridge>:8790/status`: whether this bridge is the one running, the
devices found, their stand-in ports, calls in progress and the last error.

## More than one bridge

Only one bridge runs on a network at a time. A running bridge advertises itself over mDNS
(`_intercom-bridge._tcp`), and a bridge that finds another one running stands by, then looks again
every `BROWSE_INTERVAL`. When the running one stops, a standby takes over within a minute or so;
the stand-ins keep their ids, so kiosks see the same devices at a new address. If two start at
once, the one that started first keeps running.

That makes a spare safe: run the container on two machines, or run it beside the
[Kiosk Satellite plugin](https://github.com/josephyancey/intercom-bridge-plugin), which does the
same. Each needs its status port reachable, since that is how the others tell it is still running.

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
| `STATUS_PORT` | `8790` | The status page, which other bridges also check. `0` turns it off, and then other bridges cannot tell this one runs. |
| `STATE_DIR` | `/data` | Where the bridge keeps each stand-in's port, so they stay the same across restarts. |
| `NAME_SUFFIX` | none | Added to TECHO5 device names in the kiosks' list, like ` (Echo)`. |
| `BROWSE_INTERVAL` | `30s` | How often the bridge looks for devices. |
| `FORGET_AFTER` | `10m` | A device not heard from for this long loses its stand-in. Some tablets answer mDNS only now and then. |
| `MAX_CALL` | `2h` | The longest a call through the bridge may last. |
| `SILENCE_HANGUP` | `0` (off) | End a call once neither side has said anything for this long, like `15s`. |
| `SILENCE_LEVEL_DB` | `-55` | What counts as quiet for `SILENCE_HANGUP`, in dBFS. Raise it (`-45`) if a noisy room keeps calls up; lower it (`-65`) if calls end while people are talking quietly. |
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

## Announcements

Announcements cross over too. A TECHO5 device's announcement plays on every kiosk as a Kiosk Satellite
broadcast, with the kiosk's chime. A kiosk's **Announce to all** plays on every TECHO5 device as a TECHO5
announcement. Each side's own rules apply: a kiosk with *Accept announcements* off or in do not disturb
turns it away, and a TECHO5 device in quiet hours plays it the way it plays any announcement.

## Limits

- **Announcements of words only** (a TECHO5 announcement with text and no recorded voice, like one sent
  from Home Assistant) are not carried to kiosks: a kiosk broadcast needs a voice to play. Reminders a
  TECHO5 device shares with the house are not carried either.
- **A kiosk's *Announce to all* reaches TECHO5 devices once it ends**, not live: TECHO5 plays
  announcements as recordings, up to 20 seconds.
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
| Kiosk Satellite | 2026.10.40 and 2026.10.45 (Meta Portal, Fire tablets, Echo Show 5 on LineageOS) |
| TECHO5 | TECHO5-Dot v1.0.45 on an Echo 2nd gen (radar) |

Calls placed and answered both ways, with audio both ways and hangup from either end.

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
