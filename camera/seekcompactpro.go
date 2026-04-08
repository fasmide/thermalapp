package camera

import (
	"encoding/binary"
	"fmt"
	"image"
	"log"
	"math"

	"github.com/google/gousb"
)

// Seek CompactPRO USB identifiers.
const (
	seekVID = 0x289d
	seekPID = 0x0011
)

// Seek CompactPRO frame dimensions.
const (
	seekRawW = 342 // raw frame columns (including metadata margin)
	seekRawH = 260 // raw frame rows (including metadata header/footer)

	seekImageW = 320 // usable image width
	seekImageH = 240 // usable image height

	// ROI offset within raw frame.
	seekROIX = 1 // first usable column
	seekROIY = 4 // first usable row

	seekRawPixels = seekRawW * seekRawH // total raw uint16 values per frame
	seekRawBytes  = seekRawPixels * 2   // total raw bytes per frame (177,840)

	// Bulk transfer chunk size (342 * 20 * 2 = 13,680 bytes per read).
	seekBulkChunkSize = 13680

	// USB control transfer bmRequestType values.
	seekCtrlOut = 0x41 // OUT | VENDOR | INTERFACE
	seekCtrlIn  = 0xC1 // IN  | VENDOR | INTERFACE

	// Bulk IN endpoint address for streaming.
	seekBulkEndpoint = 0x81
)

// Seek vendor-specific USB command codes (bRequest values).
const (
	seekCmdTargetPlatform          = 84 // 0x54
	seekCmdSetOperationMode        = 60 // 0x3C
	seekCmdGetFirmwareInfo         = 78 // 0x4E
	seekCmdReadChipID              = 54 // 0x36
	seekCmdSetFactorySettings      = 86 // 0x56  SET_FACTORY_SETTINGS_FEATURES
	seekCmdGetFactorySettings      = 88 // 0x58
	seekCmdSetFirmwareInfoFeatures = 85 // 0x55
	seekCmdSetImageProcessingMode  = 62 // 0x3E
	seekCmdStartGetImageTransfer   = 83 // 0x53
	seekCmdToggleShutter           = 55 // 0x37
)

// Vendor command response/payload sizes (bytes).
const (
	seekFWInfoShortLen    = 4   // GET_FIRMWARE_INFO short response
	seekChipIDLen         = 12  // READ_CHIP_ID response
	seekFactorySettings6  = 12  // GET_FACTORY_SETTINGS response for 6-word request
	seekFactorySettings1  = 2   // GET_FACTORY_SETTINGS response for 1-word request
	seekFWInfoLongLen     = 64  // GET_FIRMWARE_INFO long response (features 0x17/0x15)
	seekCalAddrByteLen    = 2   // byte length of a calibration table address
	seekTransferSizeBytes = 4   // byte length of the image transfer size field
	seekMaxUint8          = 255 // maximum uint8 value for IR normalization
)

// Seek frame ID values (stored at raw_data[2]).
const (
	seekFrameIDCalibration = 4 // dead-pixel calibration frame (first frame after init)
	seekFrameIDFFC         = 1 // flat-field calibration (shutter) frame
	seekFrameIDNormal      = 3 // normal thermal image frame

	// Maximum grab attempts before giving up.
	seekMaxGrabAttempts = 40

	// Maximum init retry attempts (first frame sometimes incomplete).
	seekMaxInitRetries = 3

	// FFC offset bias: output = raw + seekFFCOffset - ffc_frame.
	seekFFCOffset = 0x4000

	// Dead-pixel detection histogram bins.
	seekHistBins = 0x4000

	// Dead-pixel marker value used during filtering.
	seekDeadPixelMarker = 0xFFFF
)

// Seek CompactPRO metadata and calibration constants.
const (
	// seekMetaWordCount is the number of metadata uint16 words in the frame
	// header rows (rows 0..seekROIY-1) that are logged for diagnostics.
	seekMetaWordCount = 8

	// seekCalStructSize is the size in bytes of the thermography parameter struct
	// embedded somewhere in the factory calibration table.
	// Reverse-engineering shows this small v5 parameter block is present in the
	// factory blob, but the vendor thermography path also expects additional
	// file-based calibration assets outside that blob. Its location within the
	// table is not fixed at offset 0; readCalibrationTable scans the full table
	// to find it.
	seekCalStructSize = 0x60

	// Byte offsets within the seekCalStructSize-byte thermography parameter struct.
	// Source: reverse-engineering libseekip.so thermography_init (0x3e610).
	seekCalOffVersion  = 0x00 // uint32: struct version (valid 1..6)
	seekCalOffPlanckR  = 0x08 // float32: R constant; used for struct plausibility check
	seekCalOffPlanckF  = 0x0c // float32: F constant; used for struct plausibility check
	seekCalOffOutPoly1 = 0x28 // float32: output-poly z¹ coefficient; encodes counts per 100 °C (v5)

	// seekCalBytesPerWord is the number of bytes per uint16 word in the factory
	// calibration table. Used when converting a byte offset to a word address.
	seekCalBytesPerWord = 2

	// seekCalFieldBytes is the size in bytes of each float32/uint32 field in the cal struct.
	seekCalFieldBytes = 4

	// seekCalVersionMin and seekCalVersionMax define the valid version range
	// for the factory calibration struct (libseekip.so: cal[0]-1 <= 5).
	seekCalVersionMin = 1
	seekCalVersionMax = 6

	// seekCalPlanckRMin / seekCalPlanckRMax are the plausibility bounds for the
	// planckR field used when scanning the full calibration table for the
	// thermography struct. planckR (default 4300.0) must have |planckR| > 50;
	// values outside [50..1e6] are noise.
	seekCalPlanckRMin = 50.0
	seekCalPlanckRMax = 1e6

	// seekCalPlanckFMin / seekCalPlanckFMax are the plausibility bounds for the
	// planckF field (default 17.5). Values outside [0.01..1000] are noise.
	seekCalPlanckFMin = 0.01
	seekCalPlanckFMax = 1000.0

	// seekTempClampLow / seekTempClampHigh are the output temperature clamp limits.
	seekTempClampLow  = -40.0
	seekTempClampHigh = 330.0

	// seekCalVersion5 is the factory-cal struct version used by the newer vendor
	// thermography path. The full vendor implementation is not reproduced here;
	// this driver only uses the small factory struct plus a linear fallback.
	// For this version the Planck polynomial fields (coeff50/54/58/5c) are all
	// zero and must not be used.
	seekCalVersion5 = 5

	// seekFFCHeaderIdxShutterPixel is the index into rawBuf[] of the shutter
	// ADC reading encoded in the FFC frame header.
	seekFFCHeaderIdxShutterPixel = 5

	// seekFFCHeaderIdxShutterTemp is the index into rawBuf[] of the shutter
	// temperature word encoded in the FFC frame header.
	// Encoding: temperature_kelvin * seekFFCShutterTempKelvinScale (uint16).
	seekFFCHeaderIdxShutterTemp = 6

	// seekFFCShutterTempKelvinScale is the multiplier used to encode the shutter
	// temperature in the FFC frame header: raw_word = kelvin * 50.
	// Dividing by this scale and subtracting kelvinOffset gives degrees Celsius.
	// Evidence: header[6] = 15000 → 15000/50 = 300.0 K = 26.85 °C (room temp).
	seekFFCShutterTempKelvinScale = 50.0

	// seekCalV5SlopeScale is the scale applied to outPoly1 to compute the v5
	// linear slope: outPoly1 encodes raw ADC counts per 100 °C, so
	// slope_degC_per_count = seekCalV5SlopeScale / outPoly1.
	seekCalV5SlopeScale = 100.0
)

// seekCalStruct holds the thermography parameter struct extracted from the
// factory calibration table. Only the fields used by the current v5 fallback
// are stored; the remaining bytes in the 0x60-byte struct are ignored.
//
// Byte layout (little-endian unless noted):
//
//	0x00  version  uint32  (valid range 1..6; must be seekCalVersion5 = 5)
//	0x08  planckR  float32 R constant; used only for struct plausibility scanning
//	0x0c  planckF  float32 F constant; used only for struct plausibility scanning
//	0x28  outPoly1 float32 encodes raw ADC counts per 100 °C (v5 slope denominator)
type seekCalStruct struct {
	version  uint32
	planckR  float32 // used for struct plausibility check during table scan
	planckF  float32 // used for struct plausibility check during table scan
	outPoly1 float32 // v5 slope denominator: slope = seekCalV5SlopeScale / outPoly1
}

// SeekCamera drives the Seek Thermal CompactPRO USB thermal camera.
type SeekCamera struct {
	ctx  *gousb.Context
	dev  *gousb.Device
	cfg  *gousb.Config
	intf *gousb.Interface // interface 0 (control + bulk)
	ep   *gousb.InEndpoint

	rawBuf []uint16 // raw frame buffer (seekRawPixels)

	// Calibration state.
	ffcFrame       []uint16      // last FFC (shutter) frame, ROI-cropped
	deadPixelMask  []bool        // true = alive pixel (ROI dimensions)
	deadPixelOrder []image.Point // dead pixels in correction order

	// factoryCal holds the parsed thermography parameter struct from the camera's
	// factory calibration table. Non-nil after a successful readCalibrationTable().
	factoryCal *seekCalStruct

	// shutterTempC is the shutter temperature in degrees Celsius, read from
	// FFC frame header word seekFFCHeaderIdxShutterTemp (kelvin × 50).
	shutterTempC float32

	// celsiusLUT is a 65536-entry lookup table mapping FFC-corrected uint16 pixel
	// values to degrees Celsius. Rebuilt on each FFC frame. Non-nil only after
	// the first FFC frame with valid v5 factory cal.
	// Indexed by FFC-corrected values (seekFFCOffset = 0x4000 maps to shutterTempC).
	celsiusLUT []float32

	streaming bool
}

var _ Camera = (*SeekCamera)(nil)

// NewSeekCamera creates a new Seek CompactPRO camera driver.
func NewSeekCamera() *SeekCamera {
	return &SeekCamera{}
}

func (c *SeekCamera) SensorSize() image.Point {
	return image.Pt(seekImageW, seekImageH)
}

// Connect opens the USB device and claims interface 0.
func (c *SeekCamera) Connect() error {
	c.ctx = gousb.NewContext()

	dev, err := c.ctx.OpenDeviceWithVIDPID(seekVID, seekPID)
	if err != nil {
		hint := usbDevPathHint(seekVID, seekPID)
		c.ctx.Close()

		return fmt.Errorf("open Seek device: %w%s", err, hint)
	}
	if dev == nil {
		c.ctx.Close()

		return fmt.Errorf("Seek CompactPRO not found (VID=%04x PID=%04x)", seekVID, seekPID)
	}

	if err := dev.SetAutoDetach(true); err != nil {
		dev.Close()
		c.ctx.Close()

		return fmt.Errorf("set auto-detach: %w", err)
	}

	c.dev = dev

	cfg, err := dev.Config(1)
	if err != nil {
		dev.Close()
		c.ctx.Close()

		return fmt.Errorf("get config 1: %w", err)
	}
	c.cfg = cfg

	intf, err := cfg.Interface(0, 0)
	if err != nil {
		cfg.Close()
		dev.Close()
		c.ctx.Close()

		return fmt.Errorf("claim interface 0: %w", err)
	}
	c.intf = intf

	bulkEP, err := intf.InEndpoint(seekBulkEndpoint)
	if err != nil {
		intf.Close()
		cfg.Close()
		dev.Close()
		c.ctx.Close()

		return fmt.Errorf("get endpoint 0x%02x: %w", seekBulkEndpoint, err)
	}

	c.ep = bulkEP
	c.rawBuf = make([]uint16, seekRawPixels)

	return nil
}

// Init performs the CompactPRO initialization sequence and reads the first
// calibration frame for dead-pixel detection.
func (c *SeekCamera) Init() (DeviceInfo, error) {
	var info DeviceInfo

	// The init sequence may need a retry if the camera wasn't cleanly closed.
	var initErr error
	for attempt := range seekMaxInitRetries {
		log.Printf("seek init: attempt %d/%d", attempt+1, seekMaxInitRetries)
		initErr = c.initSequence()
		if initErr != nil {
			return info, fmt.Errorf("init sequence: %w", initErr)
		}

		// First frame should be a dead-pixel calibration frame (id=4).
		log.Println("seek init: waiting for calibration frame (id=4)...")
		if err := c.fetchFrame(); err != nil {
			log.Printf("seek init: first frame fetch failed (attempt %d): %v", attempt+1, err)

			continue
		}

		frameID := c.rawBuf[2]
		log.Printf("seek init: first frame id=%d", frameID)
		if frameID != seekFrameIDCalibration {
			return info, fmt.Errorf("expected first frame id=4, got %d", frameID)
		}

		c.buildDeadPixelMap()

		// Grab the first normal frame (also captures FFC frame).
		log.Println("seek init: grabbing first normal frame...")
		if err := c.grabNormalFrame(); err != nil {
			log.Printf("seek init: first grab failed (attempt %d): %v", attempt+1, err)

			continue
		}

		log.Println("seek init: complete")
		c.streaming = true
		info.Model = "Seek CompactPRO"

		return info, nil
	}

	return info, fmt.Errorf("max init retries exceeded: %w", initErr)
}

// initSequence sends the full CompactPRO vendor command init sequence.
func (c *SeekCamera) initSequence() error {
	// Step 1: TARGET_PLATFORM (retry with shutdown if camera wasn't cleanly closed).
	log.Println("seek init: step 1 — target platform")
	if err := c.initTargetPlatform(); err != nil {
		return err
	}

	// Steps 2-12: Interrogate firmware, chip ID, and factory settings.
	log.Println("seek init: steps 2-12 — read device info")
	if err := c.initReadDeviceInfo(); err != nil {
		return err
	}

	// Step 13: Read factory calibration table.
	log.Println("seek init: step 13 — read calibration table")
	if err := c.readCalibrationTable(); err != nil {
		return fmt.Errorf("read calibration table: %w", err)
	}
	log.Printf("seek init: step 13 done — factoryCal valid=%v", c.factoryCal != nil)

	// Steps 14-17: Final firmware query, set image processing mode, start running.
	log.Println("seek init: steps 14-17 — start running")

	return c.initStartRunning()
}

// initTargetPlatform sends TARGET_PLATFORM, retrying after a shutdown if needed.
func (c *SeekCamera) initTargetPlatform() error {
	if err := c.vendorSet(seekCmdTargetPlatform, []byte{0x01}); err != nil {
		c.shutdownDevice()

		if retryErr := c.vendorSet(seekCmdTargetPlatform, []byte{0x01}); retryErr != nil {
			return fmt.Errorf("target platform (retry): %w", retryErr)
		}
	}

	return nil
}

// initReadDeviceInfo queries firmware info, chip ID, and factory settings
// (init steps 2-12).
func (c *SeekCamera) initReadDeviceInfo() error {
	log.Println("seek init: 2 — set operation mode idle")
	if err := c.vendorSet(seekCmdSetOperationMode, []byte{0x00, 0x00}); err != nil {
		return fmt.Errorf("set operation mode idle: %w", err)
	}

	log.Println("seek init: 3 — get firmware info (short)")
	if err := c.vendorGet(seekCmdGetFirmwareInfo, seekFWInfoShortLen); err != nil {
		return fmt.Errorf("get firmware info: %w", err)
	}

	log.Println("seek init: 4 — read chip ID")
	if err := c.vendorGet(seekCmdReadChipID, seekChipIDLen); err != nil {
		return fmt.Errorf("read chip id: %w", err)
	}

	log.Println("seek init: 5-12 — read factory settings")

	return c.initReadFactorySettings()
}

// initReadFactorySettings reads factory settings and firmware features
// (init steps 5-12).
func (c *SeekCamera) initReadFactorySettings() error {
	// Factory settings: offset 0x0008, length 6.
	log.Println("seek init: 5 — set factory settings (1)")
	if err := c.vendorSet(seekCmdSetFactorySettings, []byte{0x06, 0x00, 0x08, 0x00, 0x00, 0x00}); err != nil {
		return fmt.Errorf("set factory features 1: %w", err)
	}

	log.Println("seek init: 6 — get factory settings (1)")
	if err := c.vendorGet(seekCmdGetFactorySettings, seekFactorySettings6); err != nil {
		return fmt.Errorf("get factory settings 1: %w", err)
	}

	// Firmware info feature 0x17.
	log.Println("seek init: 7 — set fw info features 0x17")
	if err := c.vendorSet(seekCmdSetFirmwareInfoFeatures, []byte{0x17, 0x00}); err != nil {
		return fmt.Errorf("set fw info features 0x17: %w", err)
	}

	log.Println("seek init: 8 — get firmware info (64 bytes)")
	if err := c.vendorGet(seekCmdGetFirmwareInfo, seekFWInfoLongLen); err != nil {
		return fmt.Errorf("get firmware info 64: %w", err)
	}

	// Factory settings: offset 0x0600, length 1.
	log.Println("seek init: 9 — set factory settings (2)")
	if err := c.vendorSet(seekCmdSetFactorySettings, []byte{0x01, 0x00, 0x00, 0x06, 0x00, 0x00}); err != nil {
		return fmt.Errorf("set factory features 2: %w", err)
	}

	log.Println("seek init: 10 — get factory settings (2)")
	if err := c.vendorGet(seekCmdGetFactorySettings, seekFactorySettings1); err != nil {
		return fmt.Errorf("get factory settings 2: %w", err)
	}

	// Factory settings: offset 0x0601, length 1.
	log.Println("seek init: 11 — set factory settings (3)")
	if err := c.vendorSet(seekCmdSetFactorySettings, []byte{0x01, 0x00, 0x01, 0x06, 0x00, 0x00}); err != nil {
		return fmt.Errorf("set factory features 3: %w", err)
	}

	log.Println("seek init: 12 — get factory settings (3)")
	if err := c.vendorGet(seekCmdGetFactorySettings, seekFactorySettings1); err != nil {
		return fmt.Errorf("get factory settings 3: %w", err)
	}

	log.Println("seek init: 12 done")

	return nil
}

// initStartRunning sends the final firmware query, sets image processing mode,
// and switches to run mode (init steps 14-17).
func (c *SeekCamera) initStartRunning() error {
	log.Println("seek init: 14 — set fw info features 0x15")
	if err := c.vendorSet(seekCmdSetFirmwareInfoFeatures, []byte{0x15, 0x00}); err != nil {
		return fmt.Errorf("set fw info features 0x15: %w", err)
	}

	log.Println("seek init: 15 — get firmware info 0x15")
	if err := c.vendorGet(seekCmdGetFirmwareInfo, seekFWInfoLongLen); err != nil {
		return fmt.Errorf("get firmware info 0x15: %w", err)
	}

	log.Println("seek init: 16 — set image processing mode")
	if err := c.vendorSet(seekCmdSetImageProcessingMode, []byte{0x08, 0x00}); err != nil {
		return fmt.Errorf("set image processing mode: %w", err)
	}

	log.Println("seek init: 17 — set operation mode run")
	if err := c.vendorSet(seekCmdSetOperationMode, []byte{0x01, 0x00}); err != nil {
		return fmt.Errorf("set operation mode run: %w", err)
	}

	log.Println("seek init: 17 done — camera in run mode")

	return nil
}

// readCalibrationTable reads the factory calibration area in 32-word chunks.
//
// The camera exposes at least the first 2560 words (5120 bytes), which contain
// the factory thermography struct used by this driver. Reverse-engineering shows
// the remaining vendor v5 calibration path depends on separate external assets,
// so we keep the runtime read bounded to the known-good factory table.
//
// The thermography parameter struct (seekCalStructSize bytes) is not guaranteed
// to start at byte 0; on this camera the first 96 bytes appear to be a sensor-
// config struct (words 32/33 = 342/260 = frame dimensions). Scanning the full
// blob is robust against any firmware-defined placement of the thermography
// struct.
func (c *SeekCamera) readCalibrationTable() error {
	const (
		calTableWords = 2560 // known-good factory table size
		calChunkWords = 32   // words per read
		calChunkBytes = calChunkWords * 2
	)

	// calBuf accumulates all raw bytes from the calibration table.
	calBuf := make([]byte, 0, calTableWords*seekCalBytesPerWord)

	for addr := uint16(0); addr < calTableWords; addr += calChunkWords {
		addrLE := make([]byte, seekCalAddrByteLen)
		binary.LittleEndian.PutUint16(addrLE, addr)

		data := []byte{0x20, 0x00, addrLE[0], addrLE[1], 0x00, 0x00}
		if err := c.vendorSet(seekCmdSetFactorySettings, data); err != nil {
			return fmt.Errorf("set cal addr %d: %w", addr, err)
		}

		chunk, err := c.vendorGetData(seekCmdGetFactorySettings, calChunkBytes)
		if err != nil {
			return fmt.Errorf("get cal data at %d: %w", addr, err)
		}

		calBuf = append(calBuf, chunk...)
	}

	log.Printf("seek cal: table read complete (%d bytes), scanning for thermography struct", len(calBuf))

	// Scan every 4-byte-aligned offset for a valid thermography struct.
	// The struct starts with a uint32 version field in [1..6] and then has
	// plausible float32 values at the planckR/planckF offsets.
	for scanOff := 0; scanOff+seekCalStructSize <= len(calBuf); scanOff += seekCalFieldBytes {
		candidate := parseCalStruct(calBuf[scanOff:])
		if candidate != nil {
			log.Printf("seek cal: thermography struct found at byte offset 0x%04x (word %d)",
				scanOff, scanOff/seekCalBytesPerWord)
			log.Printf("seek: factory cal v=%d R=%.4g F=%.4g outPoly1=%.4g",
				candidate.version, candidate.planckR, candidate.planckF, candidate.outPoly1)
			c.factoryCal = candidate

			break
		}
	}

	if c.factoryCal == nil {
		log.Printf("seek cal: no valid thermography struct found in %d-byte table", len(calBuf))
	}

	log.Printf("seek cal: factoryCal valid=%v", c.factoryCal != nil)

	return nil
}

// shutdownDevice sends the triple SET_OPERATION_MODE(0) shutdown sequence.
func (c *SeekCamera) shutdownDevice() {
	idle := []byte{0x00, 0x00}
	_ = c.vendorSet(seekCmdSetOperationMode, idle)
	_ = c.vendorSet(seekCmdSetOperationMode, idle)
	_ = c.vendorSet(seekCmdSetOperationMode, idle)
}

// fetchFrame requests and reads one raw frame from the camera.
func (c *SeekCamera) fetchFrame() error {
	// Request frame: send raw_data_size as little-endian uint32.
	sizeBytes := make([]byte, seekTransferSizeBytes)
	binary.LittleEndian.PutUint32(sizeBytes, seekRawPixels)

	if err := c.vendorSet(seekCmdStartGetImageTransfer, sizeBytes); err != nil {
		return fmt.Errorf("start image transfer: %w", err)
	}

	// Read frame data via bulk transfers.
	buf := make([]byte, seekRawBytes)
	done := 0

	for done < seekRawBytes {
		remaining := seekRawBytes - done
		chunkReq := seekBulkChunkSize
		if chunkReq > remaining {
			chunkReq = remaining
		}

		bytesRead, err := c.ep.Read(buf[done : done+chunkReq])
		if err != nil {
			return fmt.Errorf("bulk read at offset %d: %w", done, err)
		}

		done += bytesRead
	}

	// Convert from little-endian to host byte order.
	for idx := range seekRawPixels {
		c.rawBuf[idx] = binary.LittleEndian.Uint16(buf[idx*2 : idx*2+2])
	}

	return nil
}

// grabNormalFrame reads frames until a normal frame (id=3) is received.
// Any FFC frames (id=1) encountered are stored for calibration.
func (c *SeekCamera) grabNormalFrame() error {
	for attempt := range seekMaxGrabAttempts {
		if err := c.fetchFrame(); err != nil {
			return fmt.Errorf("grab: %w", err)
		}

		frameID := c.rawBuf[2]
		log.Printf("seek grab: attempt %d frame id=%d", attempt+1, frameID)

		switch frameID {
		case seekFrameIDNormal:
			return nil
		case seekFrameIDFFC:
			c.storeFFCFrame()
		}
	}

	return fmt.Errorf("no normal frame after %d attempts", seekMaxGrabAttempts)
}

// extractROI returns the usable image region from the raw buffer as a
// row-major []uint16 slice of seekImageW * seekImageH values.
func (c *SeekCamera) extractROI() []uint16 {
	roi := make([]uint16, seekImageW*seekImageH)

	for row := range seekImageH {
		srcStart := (row+seekROIY)*seekRawW + seekROIX
		dstStart := row * seekImageW
		copy(roi[dstStart:dstStart+seekImageW], c.rawBuf[srcStart:srcStart+seekImageW])
	}

	return roi
}

// storeFFCFrame saves the current raw frame's ROI as the FFC reference,
// reads the v5 shutter pixel and temperature from the frame header, and
// rebuilds the celsius LUT.
func (c *SeekCamera) storeFFCFrame() {
	c.ffcFrame = c.extractROI()
	log.Printf("seek: FFC frame — header[0..%d]: %v", seekMetaWordCount-1, c.rawBuf[:seekMetaWordCount])

	if c.factoryCal == nil || c.factoryCal.version != seekCalVersion5 {
		log.Printf("seek: no v5 factory cal available — temperature output unavailable")

		return
	}

	// Version 5: shutter pixel and temperature are encoded directly in the
	// FFC frame header. header[5] = raw ADC at shutter; header[6] = K×50.
	c.shutterTempC = float32(c.rawBuf[seekFFCHeaderIdxShutterTemp])/seekFFCShutterTempKelvinScale - kelvinOffset
	log.Printf("seek: v5 FFC - shutterPixel=%d shutterTempC=%.2fC",
		int16(c.rawBuf[seekFFCHeaderIdxShutterPixel]), c.shutterTempC)
	c.celsiusLUT = c.buildCelsiusLUTV5(c.factoryCal)
}

// applyFFC applies flat-field correction: output = raw + 0x4000 - ffc.
func (c *SeekCamera) applyFFC(raw []uint16) []uint16 {
	count := len(raw)
	result := make([]uint16, count)

	if c.ffcFrame == nil || len(c.ffcFrame) != count {
		// No FFC frame yet — pass through raw values.
		copy(result, raw)

		return result
	}

	for idx := range count {
		val := int(raw[idx]) + seekFFCOffset - int(c.ffcFrame[idx])
		if val < 0 {
			val = 0
		} else if val > math.MaxUint16 {
			val = math.MaxUint16
		}

		result[idx] = uint16(val)
	}

	return result
}

// buildDeadPixelMap analyzes the calibration frame to detect dead pixels
// and builds the correction order list.
func (c *SeekCamera) buildDeadPixelMap() {
	roi := c.extractROI()

	threshold := c.computeDeadPixelThreshold(roi)

	// Build dead pixel mask: pixels above threshold are alive.
	count := seekImageW * seekImageH
	c.deadPixelMask = make([]bool, count)

	for idx, val := range roi {
		c.deadPixelMask[idx] = float64(val) > threshold
	}

	c.buildDeadPixelOrder()

	log.Printf("seek: detected %d dead pixels", len(c.deadPixelOrder))
}

// computeDeadPixelThreshold calculates the dead-pixel detection threshold
// using histogram analysis of the calibration ROI.
func (c *SeekCamera) computeDeadPixelThreshold(roi []uint16) float64 {
	hist := make([]int, seekHistBins)

	var maxVal uint16

	for _, val := range roi {
		if val < uint16(seekHistBins) {
			hist[val]++
		}

		if val > maxVal {
			maxVal = val
		}
	}

	// Suppress bin 0 (usually the highest, but we don't want it).
	hist[0] = 0

	// Find the bin with the maximum count.
	maxBinIdx := 0
	maxBinCount := 0

	for binIdx, binCount := range hist {
		if binCount > maxBinCount {
			maxBinCount = binCount
			maxBinIdx = binIdx
		}
	}

	// Threshold: hist_max_bin_index - (max_pixel_value - hist_max_bin_index)
	return float64(maxBinIdx) - (float64(maxVal) - float64(maxBinIdx))
}

// buildDeadPixelOrder constructs the dead pixel correction list in dependency
// order: a dead pixel is only added when at least one 4-connected neighbor is
// alive (or already listed).
func (c *SeekCamera) buildDeadPixelOrder() {
	count := seekImageW * seekImageH
	listed := make([]bool, count)
	copy(listed, c.deadPixelMask)
	c.deadPixelOrder = nil

	for {
		found := false

		for pixY := range seekImageH {
			for pixX := range seekImageW {
				idx := pixY*seekImageW + pixX

				if listed[idx] {
					continue // not dead, or already listed
				}

				if c.hasListedNeighbor(listed, pixX, pixY) {
					c.deadPixelOrder = append(c.deadPixelOrder, image.Pt(pixX, pixY))
					listed[idx] = true
					found = true
				}
			}
		}

		if !found {
			break
		}
	}
}

// hasListedNeighbor returns true if any 4-connected neighbor of (pixX,pixY) is marked in listed.
func (c *SeekCamera) hasListedNeighbor(listed []bool, pixX, pixY int) bool {
	if pixX > 0 && listed[pixY*seekImageW+pixX-1] {
		return true
	}

	if pixX < seekImageW-1 && listed[pixY*seekImageW+pixX+1] {
		return true
	}

	if pixY > 0 && listed[(pixY-1)*seekImageW+pixX] {
		return true
	}

	if pixY < seekImageH-1 && listed[(pixY+1)*seekImageW+pixX] {
		return true
	}

	return false
}

// applyDeadPixelFilter replaces dead pixel values with the mean of their
// alive 4-connected neighbors.
func (c *SeekCamera) applyDeadPixelFilter(pixels []uint16) []uint16 {
	count := len(pixels)
	dst := make([]uint16, count)

	// Fill with marker, then copy alive pixels.
	for idx := range count {
		if c.deadPixelMask[idx] {
			dst[idx] = pixels[idx]
		} else {
			dst[idx] = seekDeadPixelMarker
		}
	}

	// Replace each dead pixel with mean of non-dead neighbors.
	for _, pt := range c.deadPixelOrder {
		dst[pt.Y*seekImageW+pt.X] = c.calcNeighborMean(dst, pt.X, pt.Y)
	}

	return dst
}

// calcNeighborMean computes the mean of non-dead 4-connected neighbors.
func (c *SeekCamera) calcNeighborMean(img []uint16, pixX, pixY int) uint16 {
	var sum uint32

	var count uint32

	if pixX > 0 {
		val := img[pixY*seekImageW+pixX-1]
		if val != seekDeadPixelMarker {
			sum += uint32(val)
			count++
		}
	}

	if pixX < seekImageW-1 {
		val := img[pixY*seekImageW+pixX+1]
		if val != seekDeadPixelMarker {
			sum += uint32(val)
			count++
		}
	}

	if pixY > 0 {
		val := img[(pixY-1)*seekImageW+pixX]
		if val != seekDeadPixelMarker {
			sum += uint32(val)
			count++
		}
	}

	if pixY < seekImageH-1 {
		val := img[(pixY+1)*seekImageW+pixX]
		if val != seekDeadPixelMarker {
			sum += uint32(val)
			count++
		}
	}

	if count == 0 {
		return 0
	}

	return uint16(sum / count)
}

// ReadFrame reads one frame from the Seek camera, applies FFC and dead pixel
// correction, and returns the processed frame.
//
// Frame.Celsius is populated with per-pixel temperatures derived from
// FFC-corrected pixels via the current v5 linear fallback. FFC removes
// fixed-pattern noise before temperature conversion, which is essential for a
// cleaner temperature image even though the absolute calibration is still
// incomplete.
func (c *SeekCamera) ReadFrame() (*Frame, error) {
	if !c.streaming {
		return nil, fmt.Errorf("not streaming")
	}

	// Read frames until we get a normal one (id=3).
	// Store any FFC frames (id=1) along the way.
	if err := c.grabNormalFrame(); err != nil {
		return nil, err
	}

	// Extract ROI (pre-FFC raw pixels).
	roi := c.extractROI()

	// Apply FFC correction — removes fixed-pattern noise for both display and
	// temperature conversion.
	corrected := c.applyFFC(roi)

	if len(c.deadPixelOrder) > 0 {
		corrected = c.applyDeadPixelFilter(corrected)
	}

	frame := &Frame{
		Width:  seekImageW,
		Height: seekImageH,
	}

	// Pre-compute per-pixel Celsius from the FFC-corrected (dead-pixel-filtered)
	// pixels using the current v5 fallback LUT. Using FFC-corrected values here
	// is critical:
	// it removes per-pixel fixed-pattern noise that would otherwise appear as
	// temperature noise in Percentile AGC mode. The LUT is anchored so that
	// seekFFCOffset (0x4000) maps to shutterTempC.
	celsius := make([]float32, len(corrected))
	if c.celsiusLUT != nil {
		for idx, px := range corrected {
			celsius[idx] = c.celsiusLUT[px]
		}
	}

	frame.Celsius = celsius

	// Generate 8-bit IR plane from the corrected thermal data for hardware-AGC mode.
	frame.IR = c.thermalToIR(corrected)

	return frame, nil
}

// thermalToIR converts 16-bit thermal values to 8-bit using a simple linear
// min/max stretch.
func (c *SeekCamera) thermalToIR(thermal []uint16) []uint8 {
	count := len(thermal)
	result := make([]uint8, count)

	// Find min/max.
	var tMin, tMax uint16 = math.MaxUint16, 0

	for _, val := range thermal {
		if val < tMin {
			tMin = val
		}

		if val > tMax {
			tMax = val
		}
	}

	if tMax <= tMin {
		return result // all same value
	}

	// Simple linear stretch to 8-bit for now.
	span := float64(tMax) - float64(tMin)

	for idx, val := range thermal {
		norm := (float64(val) - float64(tMin)) / span
		if norm < 0 {
			norm = 0
		} else if norm > 1 {
			norm = 1
		}

		result[idx] = uint8(norm * seekMaxUint8)
	}

	return result
}

func (c *SeekCamera) StartStreaming() error {
	// Streaming is started during Init() (the CompactPRO starts streaming
	// as part of its init sequence). This is a no-op.
	return nil
}

func (c *SeekCamera) StopStreaming() error {
	c.streaming = false
	c.shutdownDevice()

	return nil
}

func (c *SeekCamera) Close() {
	if c.streaming {
		if err := c.StopStreaming(); err != nil {
			log.Printf("seek stop streaming on close: %v", err)
		}
	}

	if c.intf != nil {
		c.intf.Close()
	}

	if c.cfg != nil {
		c.cfg.Close()
	}

	if c.dev != nil {
		c.dev.Close()
	}

	if c.ctx != nil {
		c.ctx.Close()
	}
}

// TriggerShutter sends the shutter toggle command.
func (c *SeekCamera) TriggerShutter() error {
	return c.vendorSet(seekCmdToggleShutter, []byte{0x00, 0x00})
}

// SetGain is a no-op for the Seek CompactPRO (no gain modes).
func (c *SeekCamera) SetGain(_ GainMode) error {
	return nil
}

// --- factory calibration helpers ---

// buildCelsiusLUTV5 constructs a 65536-entry lookup table for version-5
// factory calibration, using the global linear fallback approximation of the
// vendor TLUT algorithm (thermography_process_algorithm_v5_float in
// libseekip.so at 0x3e24c). The table is indexed by FFC-corrected pixel
// values.
//
// The native v5 algorithm uses more calibration data than the small factory
// struct alone provides. Reverse-engineering of libseekip.so shows a file-based
// loader for assets such as CintLut.bin, HG_Delta.bin, FlatField.bin, and
// ThermAdjust.bin, so this fallback remains a stopgap until that external
// calibration path is reconstructed:
//
//	slope = seekCalV5SlopeScale / outPoly1
//	tempC = shutterTempC + (float32(ffcPx) - seekFFCOffset) * slope
//
// We now keep the slope positive because both the live camera data and the
// vendor simulation asset move in that direction after FFC correction:
//   - live cup scene: hotter cup region had HIGHER corrected counts than wall,
//     and the negative-slope fallback inverted hot/cold ordering
//   - vendor simulation asset thermal.bin fits thermography ~= 0.02122*filtered
//   - 39.81 with correlation 0.99967, i.e. higher filtered counts map hotter
//
// The LUT anchor remains exact:
//
//	index 0x4000 (=seekFFCOffset) → shutterTempC
func (c *SeekCamera) buildCelsiusLUTV5(cal *seekCalStruct) []float32 {
	// Positive fallback slope: hotter FFC-corrected pixels map hotter.
	slope := seekCalV5SlopeScale / cal.outPoly1 // °C per corrected count

	lut := make([]float32, math.MaxUint16+1)

	for idx := range math.MaxUint16 + 1 {
		tempC := c.shutterTempC + (float32(idx)-seekFFCOffset)*slope

		if tempC < seekTempClampLow {
			tempC = seekTempClampLow
		} else if tempC > seekTempClampHigh {
			tempC = seekTempClampHigh
		}

		lut[idx] = tempC
	}

	log.Printf(
		"seek: v5 celsiusLUT built - slope=%.6f C/count (fallback, warmer->higher), lut[0x4000]=%.2fC (shutterTempC=%.2fC)",
		slope, lut[seekFFCOffset], c.shutterTempC,
	)

	return lut
}

// parseCalStruct parses a seekCalStructSize-byte little-endian buffer into a
// seekCalStruct. The buffer must be at least seekCalStructSize bytes long.
// Returns nil when:
//   - the version field is outside [seekCalVersionMin, seekCalVersionMax], or
//   - the planckR/planckF fields are outside their plausibility bounds
//     (seekCalPlanckRMin/Max, seekCalPlanckFMin/Max). These checks prevent
//     false positives when scanning the full calibration table for the struct.
func parseCalStruct(buf []byte) *seekCalStruct {
	if len(buf) < seekCalStructSize {
		return nil
	}

	f32 := func(off int) float32 {
		return math.Float32frombits(binary.LittleEndian.Uint32(buf[off : off+seekCalFieldBytes]))
	}

	version := binary.LittleEndian.Uint32(buf[seekCalOffVersion : seekCalOffVersion+seekCalFieldBytes])
	if version < seekCalVersionMin || version > seekCalVersionMax {
		return nil
	}

	// Plausibility checks to reject false positives during full-table scan.
	planckR := f32(seekCalOffPlanckR)
	planckF := f32(seekCalOffPlanckF)

	absR := planckR
	if absR < 0 {
		absR = -absR
	}

	absF := planckF
	if absF < 0 {
		absF = -absF
	}

	if absR < seekCalPlanckRMin || absR > seekCalPlanckRMax {
		return nil
	}

	if absF < seekCalPlanckFMin || absF > seekCalPlanckFMax {
		return nil
	}

	return &seekCalStruct{
		version:  version,
		planckR:  planckR,
		planckF:  planckF,
		outPoly1: f32(seekCalOffOutPoly1),
	}
}

// --- internal USB helpers ---

// vendorSet sends a vendor-specific OUT control transfer.
func (c *SeekCamera) vendorSet(request uint8, data []byte) error {
	_, err := c.dev.Control(seekCtrlOut, request, 0, 0, data)
	if err != nil {
		return fmt.Errorf("vendor SET 0x%02x: %w", request, err)
	}

	return nil
}

// vendorGet sends a vendor-specific IN control transfer, discarding the response data.
func (c *SeekCamera) vendorGet(request uint8, length int) error {
	buf := make([]byte, length)

	if _, err := c.dev.Control(seekCtrlIn, request, 0, 0, buf); err != nil {
		return fmt.Errorf("vendor GET 0x%02x: %w", request, err)
	}

	return nil
}

// vendorGetData sends a vendor-specific IN control transfer and returns the response bytes.
func (c *SeekCamera) vendorGetData(request uint8, length int) ([]byte, error) {
	buf := make([]byte, length)

	if _, err := c.dev.Control(seekCtrlIn, request, 0, 0, buf); err != nil {
		return nil, fmt.Errorf("vendor GET 0x%02x: %w", request, err)
	}

	return buf, nil
}
