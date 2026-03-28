# P3 Metadata Row Protocol — Reverse Engineering Notes

This document records findings from reverse-engineering the 2 metadata rows
embedded in each frame of the P3 thermal camera's USB stream.

## Background

Each frame contains **2 metadata rows** of 256 × uint16 LE values (1024 bytes
total), positioned between the IR brightness rows and the thermal data rows in
the raw USB frame.

- **Row 0** (indices 0–255): Active data region at indices ~64–180.
- **Row 1** (indices 256–511): Entirely filled with `0x00ff`; appears unused.

## Row 0 Register Map

### Confirmed Registers

| Index | Name              | Description |
|-------|-------------------|-------------|
| 64    | `FrameCounter`    | Camera's internal frame counter. Increments by 1 each unique frame. **Freezes** when the shutter is closed (NUC in progress). Skips several counts after NUC completes (typically +6). |
| 72    | `Register72`      | Initially suspected to be a shutter countdown — values range from ~`0x0400` to ~`0xf000` and are low right before NUC and high right after. However, values **oscillate significantly** frame-to-frame and do not decrease monotonically. Not a reliable countdown timer. Purpose unclear; may be a sensor measurement that correlates with NUC need. |
| 75    | `Register75`      | Similar pattern to index 72 — noisy, oscillating. Not a smooth countdown. |
| 180   | `FrameCounter2`   | Secondary frame counter. Increments by 1 every frame **even during shutter** (unlike index 64). Continues counting during NUC. |

### Probable Temperature Registers

These indices change slowly and drift in a direction consistent with sensor
warm-up. They are likely FPA (focal plane array) or housing temperature sensors,
in some internal unit (possibly 1/64 K or similar).

| Index | Label   | Typical Values | Notes |
|-------|---------|----------------|-------|
| 165   | `temp1` | `0x1420–0x1490` | Slowly decreasing at start-up, stabilises. Likely FPA temp. |
| 168   | `temp2` | `0x1290–0x1300` | Similar slow-changing pattern. |
| 173   | `temp3` | `0x1300–0x1330` | |
| 174   | `temp4` | `0x1420–0x1490` | Close to temp1 range. |
| 175   | `temp5` | `0x1290–0x1300` | Similar to temp2 range. |
| 176   | `temp6` | `0x1260–0x12D0` | |

### Oscillating / Noisy Registers

These change every frame by small amounts and may be integration time, signal
statistics, or calibration coefficients.

| Index | Label  | Notes |
|-------|--------|-------|
| 65    | `r65`  | Oscillates around `0x21C4–0x223E`. |
| 68    | `r68`  | Varies `0x4B–0x4F` range (high byte). |
| 70    | `r70`  | Varies frame to frame. |
| 71    | `r71`  | Alternates with r70. |
| 73    | `r73`  | Large jumps after NUC (`0x054c` → `0x354c`). |
| 74    | `r74`  | Large jumps after NUC (`0x0300` → `0x4e00`). |
| 166   | `r166` | Small changes, possibly related to temp1. |
| 167   | `r167` | Small changes. |
| 169   | `r169` | **Zero during NUC** (`0x0000`), nonzero otherwise (`0x00E8–0x00F8`). Alternative shutter detection method. |
| 170   | `r170` | **Zero during NUC** (`0x0000`), nonzero otherwise (`0x00BE–0x00BF`). Same pattern as r169. |
| 179   | `r179` | Moderate variation per frame. |

### Unused Regions

| Range     | Value  |
|-----------|--------|
| 0–63      | `0x0000` |
| 152–164   | `0x0000` |
| 181–255   | `0x00ff` |
| 256–511   | `0x00ff` (entire row 1) |

## Shutter (NUC) Detection Criteria

During an automatic or manual NUC event, approximately 20 frames are affected:

1. **`FrameCounter` (idx 64) freezes** — the most reliable indicator. The
   camera's frame counter stops incrementing during the NUC calibration.
2. **Registers 169 and 170 become `0x0000`** — an alternative detection method.
   These registers are always nonzero during normal operation.
3. **`FrameCounter2` (idx 180) keeps incrementing** — this counter does not
   freeze, confirming it is a USB-transfer counter rather than a sensor counter.
4. **After NUC completes**, FrameCounter jumps by ~6 (the frozen frames),
   ShutterCountdown (idx 72) resets to a high value (~`0xf000`), and registers
   73/74 undergo large value changes (likely new calibration coefficients).

## Automatic NUC Timing

- The camera auto-triggers NUC approximately every **90 seconds** (~2250 frames
  at 25 fps).
- Register 72 shows low values (~`0x0400`) just before NUC and resets to high
  values (~`0xf000`) after, but oscillates too much to be used as a countdown.
- No reliable countdown register has been found yet.
- After NUC, the shutter-frozen frames resume with updated calibration.

## Current Implementation

We use **FrameCounter freeze detection** in `P3Camera.ReadFrame()`:
- Track the previous frame's `Metadata[64]` value.
- If `Metadata[64]` hasn't changed since the last frame, set `Frame.ShutterActive = true`.
- `Frame.ShutterCountdown` is read directly from `Metadata[72]`.

## Future Research

- **Decode temperature registers**: Convert the values at indices 165, 168,
  173–176 to actual temperatures. Likely 1/64 Kelvin like thermal pixels.
- **Decode registers 73/74**: These have large swings after NUC — may be
  offset/gain calibration coefficients applied by the sensor.
- **Investigate P1 camera**: The P1 has 160×120 resolution. Metadata rows
  may use a different column range or have different register assignments.
- **Extended status register**: Reading >1 byte from USB register 0x22 returns
  debug log messages including `I/shutter: === Shutter close ===`. Could be
  polled as a secondary detection method.
