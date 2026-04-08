# thermalapp

A real-time thermal camera viewer for the **InfiRay Thermal Master P3** and an unfinished **Seek Thermal CompactPRO** path, written in Go with a [Gio](https://gioui.org) UI.

![Go](https://img.shields.io/badge/Go-1.26-blue) ![Gio](https://img.shields.io/badge/Gio-v0.9-purple) ![Platform](https://img.shields.io/badge/Linux-USB-green)

> **No formal verification.** This application has no test suite and has not been validated against calibrated reference hardware. It works well for our own use cases — visualizing thermal data, recording scenes, and spot-measuring relative temperatures — but accuracy guarantees cannot be made. Use accordingly.

> **Seek CompactPRO status:** not currently usable for reliable thermography. The driver can stream frames, but the temperature calibration path is incomplete and absolute readings are wrong. Prefer the InfiRay P3 for any real use.

![screenshot](https://github.com/user-attachments/assets/0c6b6638-c229-45a5-86cd-c4d2bb1f3cf9)

## Features

- **Live thermal imaging** — 256×192 from the InfiRay P3; 320×240 from the Seek CompactPRO (incomplete, not reliable)
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
# USB permissions (run once per camera, then replug)
sudo cp udev/99-p3thermal.rules /etc/udev/rules.d/      # InfiRay P3
sudo cp udev/99-seek-thermal.rules /etc/udev/rules.d/   # Seek CompactPRO (if applicable)
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

## AGC Modes

Press `A` to cycle between modes. The mode only affects how pixel colours are computed — temperature readings, spot measurements, and graph values are identical in all modes.

| Mode | Source data | How the colour scale is set |
|------|-------------|----------------------------|
| **Percentile** | Temperature plane (°C) | 1st–99th percentile of all pixel temperatures per frame. Clips the coldest and hottest 1% so outliers don't dominate the scale. Colorbar labels show real °C values. |
| **HW AGC** | IR brightness plane (8-bit) | Uses the 8-bit brightness values provided directly by the camera driver, whatever processing the camera applied to produce them. Colours reflect the camera's own gain decision, not a calibrated temperature range. Colorbar labels are not meaningful in this mode. |
| **Fixed** | Temperature plane (°C) | Same as Percentile but uses a user-defined min/max instead of auto-computed bounds. |

**When to use which:**

- **Percentile** is the default and most useful for radiometric work. The colour scale adapts to whatever is in the scene each frame, and it maps directly to temperature — so a warmer colour always means a warmer surface.
- **HW AGC** can look subjectively better in some scenes because the camera's ISP applies detail enhancement (DDE) and gamma correction on top of its gain mapping. It can also be useful when the scene has a very wide temperature range that causes Percentile to compress contrast on the objects you care about. The tradeoff is that colours no longer have a direct temperature meaning.
- **Fixed** is useful when comparing recordings or switching between scenes where you want a consistent colour scale rather than per-frame auto-scaling.

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
- **Per frame**: 4-byte compressed size prefix + deflate-compressed block containing timestamp, flags (shutter state), per-pixel temperature as `float32` Celsius, and uint8 IR (hardware AGC) data
- Typical compression ratio: ~3:1 (deflate level 1 for real-time performance)

Recordings are camera-agnostic — the format stores normalized `Frame` data (Celsius + IR) without assumptions about the source device.

## Architecture

```
USB Camera ──> camera/p3.go             ──┐
               camera/seekcompactpro.go ──┤──> camera.Frame ──┬──> recording/recorder.go ──> .tha file
                                          │                   │
.tha file ──> recording/player.go ────────┘                   │
                                                              v
                                                       colorize.Colorize()
                                                       (AGC + palette + emissivity)
                                                              │
                                                              v
                                                        colorize.Result
                                                       (RGBA + Celsius[])
                                                              │
                              ┌──────────────────────────────┼──────────────────────────┐
                              v                              v                          v
                         ui/app.go                     ui/spot.go                ui/graph.go
                        (main window)                 (measurement)             (graph windows)
```

## Camera Support

### InfiRay Thermal Master P3 (primary)

VID `0x3474`, PID `0x45A2` — 256×192 sensor. Full support: live streaming, shutter/NUC triggering, gain switching, radiometric recording and playback. 

### Seek Thermal CompactPRO (unfinished)

VID `0x289d`, PID `0x0011` — 320×240 usable image from a 342×260 raw frame.

Current status: the app can talk to the camera and display video, but the Seek temperature calibration path is incomplete. Hot/cold ordering is improved, yet absolute temperatures are still wrong because the vendor v5 thermography pipeline depends on external calibration assets that are not implemented here yet.

Treat the Seek driver as a reverse-engineering work in progress, not a usable feature. Recording and playback still work at the app level, but the underlying Seek temperature data should not be trusted.

### Adding other cameras

The `camera.Camera` interface is the extension point:

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

Implement the interface so that `ReadFrame()` returns a fully populated `Frame` — with `Celsius[]` in °C and `IR[]` as 8-bit AGC brightness — and the rest of the application requires no changes.

## License

This project is released under the [Unlicense](LICENSE) — public domain, no conditions.

## Acknowledgements

Hardware support would not have been possible without studying the prior work of:

- [jvdillon/p3-ir-camera](https://github.com/jvdillon/p3-ir-camera) — reverse-engineered P3 USB protocol and frame format
- [OpenThermal/libseek-thermal](https://github.com/OpenThermal/libseek-thermal) — Seek Thermal USB protocol implementation

Thank you for making your research public.

A significant portion of the Seek CompactPRO driver was additionally reverse-engineered from the official Seek Thermal Android application. As a result, some of the protocol details are inferred rather than documented, and we are not fully confident in the correctness of all edge cases.
