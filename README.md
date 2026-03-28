# thermalapp

A real-time thermal camera viewer for the InfiRay Thermal Master P3 (and similar) USB thermal cameras, written in Go with a [Gio](https://gioui.org) UI.

![Go](https://img.shields.io/badge/Go-1.26-blue) ![Gio](https://img.shields.io/badge/Gio-v0.9-purple) ![Platform](https://img.shields.io/badge/Linux-USB-green)

## Features

- **Live thermal imaging** at 256x192 from the P3 USB camera
- **4 color palettes** — Inferno, Iron, Jet, Grayscale (cycle with `C`)
- **AGC modes** — Percentile (auto-contrast) and Hardware (camera-side)
- **Emissivity correction** — 39 material presets across 7 categories with per-spot override support
- **Measurement spots** — auto-tracking min/max, cursor readout, and click-to-place user points
- **Temperature graphs** — per-spot live graph windows with min/max/mean/stddev statistics
- **Per-spot emissivity** — each measurement point can have its own emissivity setting
- **Rotation, gain toggle, shutter/NUC detection, colorbar, screenshots**
- **Radiometric recording** — record raw thermal frames to `.tha` files with full re-colorization on playback
- **Single-frame dump** — save one raw frame for offline analysis
- **Playback mode** — play back recordings without camera hardware, with slider, frame stepping, and pause

## Quick Start

```bash
# USB permissions (run once, then replug camera)
sudo cp udev/99-p3thermal.rules /etc/udev/rules.d/
sudo udevadm control --reload-rules

# Build and run
go build -o thermalapp .
./thermalapp
```

Requires `libusb` development headers:
```bash
# Debian/Ubuntu
sudo apt install libusb-1.0-0-dev

# Fedora
sudo dnf install libusb1-devel
```

## Controls

| Key | Action |
|-----|--------|
| `C` | Cycle colormap |
| `A` | Cycle AGC mode |
| `G` | Toggle gain (High/Low) |
| `S` | Trigger shutter / NUC |
| `R` | Rotate 90 degrees |
| `T` | Toggle temperature labels |
| `E` / `Shift+E` | Cycle emissivity forward / backward |
| `V` | Toggle colorbar |
| `X` | Clear all user measurement points |
| `0`-`9` | Toggle graph window for spot N |
| `D` | Dump current frame to `.tha` file |
| `F5` | Start/stop recording |
| `P` | Pause / resume (live or playback) |
| `Left` / `Right` | Step one frame (playback, when paused) |
| `Space` | Save screenshot (PNG) |
| `H` | Toggle help overlay |
| `Q` / `Esc` | Quit |

| Mouse | Action |
|-------|--------|
| Click | Place or remove a measurement point |
| Shift+Click | Select a spot for per-spot emissivity |
| Scroll wheel | Step frames (playback mode) |
| Click emissivity in status bar | Open emissivity preset picker |

## Emissivity

All infrared cameras measure *apparent* temperature assuming a perfect blackbody (emissivity = 1.0). Real surfaces emit less IR, so readings must be corrected:

```
T_object = (T_measured - (1 - e) * T_reflected) / e
```

The app includes 39 sourced presets organized into categories:

| Category | Examples | Range |
|----------|----------|-------|
| Reference | Blackbody | 1.00 |
| Organic / Biological | Human skin, water, wood, fabric | 0.92 - 0.98 |
| Construction | Concrete, brick, glass, paint | 0.91 - 0.95 |
| Plastics | ABS/PVC, epoxy/PCB | 0.91 - 0.92 |
| Oxidised Metals | Oxidised steel, cast iron, anodised aluminium | 0.28 - 0.81 |
| Polished Metals | Stainless steel, aluminium, copper, gold | 0.02 - 0.16 |
| Tapes / Coatings | Electrical tape, Kapton tape | 0.95 |

Preset values are sourced from Modest *Radiative Heat Transfer*, Touloukian *Thermophysical Properties of Matter*, FLIR/ITC reference tables, and Omega Engineering reference data.

Global emissivity applies to the entire frame. Individual spots can override it — useful when measuring different materials in the same scene (e.g. a copper heatsink next to a PCB).

## Recording & Playback

thermalapp can record raw radiometric data — not just screenshots — so that recordings can be re-colorized, re-analyzed with different palettes, AGC modes, and emissivity settings after the fact.

### Single Frame Dump

Press `D` to save the current frame as a `.tha` file. This captures one complete radiometric frame (thermal + IR data) for offline analysis.

### Continuous Recording

Press `F5` to start recording. All incoming frames are written to a timestamped `.tha` file with per-frame deflate compression. Press `F5` again to stop. The status bar shows `REC <count>` while recording.

### Playback

Play back a recording without needing the camera:

```bash
./thermalapp -play recording_20260328_143000.tha
```

Playback provides:
- A **slider** to scrub through frames
- **Play/pause** with `P`
- **Frame stepping** with `Left`/`Right` arrow keys or scroll wheel
- **Absolute timestamps** — each frame's original capture time is preserved
- Full access to all viewer features (palettes, AGC, emissivity, spots, graphs)
- **NUC detection** — frames captured during shutter calibration are flagged and display a red "NUC" badge

### File Format (.tha)

The `.tha` format is a compact, seekable binary format:

- **32-byte header**: magic (`THERMAP`), version, sensor dimensions, frame count, start time
- **Per frame**: 4-byte compressed size prefix + deflate-compressed block containing timestamp, flags (shutter state), hardware frame counter, raw uint16 thermal data, and uint8 IR data
- Typical compression ratio: ~3:1 (deflate level 1 for real-time performance)
- A 10-second recording at 25fps is roughly 26 MB

Recordings are camera-agnostic — the format stores raw sensor data without assumptions about the source device.

## Architecture

```
USB Camera ──> camera/p3.go ──> camera.Frame ──┬──> recording/recorder.go ──> .tha file
                                               │
.tha file ──> recording/player.go ─────────────┘
                                               │
                                               v
                                        colorize.Colorize()
                                        (AGC + palette + emissivity)
                                               │
                                               v
                                         colorize.Result
                                        (RGBA + Celsius[])
                                               │
                    ┌──────────────────────────┼──────────────────────────┐
                    v                          v                          v
               ui/app.go                 ui/spot.go                ui/graph.go
              (main window)             (measurement)             (graph windows)
```

## Camera Support

Currently supports the **InfiRay Thermal Master P3** (VID `0x3474`, PID `0x45A2`). The `camera.Camera` interface is designed for adding other models:

```go
type Camera interface {
    Connect() error
    Init() error
    StartStreaming() error
    ReadFrame() (*Frame, error)
    StopStreaming() error
    Close() error
    TriggerShutter() error
    SetGain(GainMode) error
    SensorSize() image.Point
}
```

## Protocol Reference

See [p3-ir-camera/P3_PROTOCOL.md](p3-ir-camera/P3_PROTOCOL.md) for the complete USB protocol documentation, including frame layout, command structure, and metadata fields.

See [METADATA-PROTOCOL.md](METADATA-PROTOCOL.md) for reverse-engineered metadata row register assignments (shutter detection, frame counters, sensor temperatures).

## License

See individual files for license information. The P3 protocol reference implementation in `p3-ir-camera/` has its own [LICENSE](p3-ir-camera/LICENSE).
