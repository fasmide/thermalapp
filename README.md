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
- **Rotation, gain toggle, shutter/NUC, colorbar, screenshots**

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
| `Space` | Save screenshot (PNG) |
| `H` | Toggle help overlay |
| `Q` / `Esc` | Quit |

| Mouse | Action |
|-------|--------|
| Click | Place or remove a measurement point |
| Shift+Click | Select a spot for per-spot emissivity |
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

## Architecture

```
USB Camera ──> camera/p3.go ──> camera.Frame
                                    │
                                    v
                             colorize.Colorize()
                             (AGC + palette + emissivity)
                                    │
                                    v
                              colorize.Result
                             (RGBA + Celsius[])
                                    │
                    ┌───────────────┼───────────────┐
                    v               v               v
               ui/app.go      ui/spot.go      ui/graph.go
              (main window)   (measurement)   (graph windows)
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

## License

See individual files for license information. The P3 protocol reference implementation in `p3-ir-camera/` has its own [LICENSE](p3-ir-camera/LICENSE).
