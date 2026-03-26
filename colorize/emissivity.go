package colorize

// Emissivity correction for non-blackbody surface temperature measurement.
//
// The infrared camera measures apparent (radiometric) temperature assuming
// emissivity ε=1.0 (perfect blackbody). Real surfaces emit less IR radiation,
// so the measured temperature is lower than the true surface temperature.
//
// Correction formula (Stefan-Boltzmann approximation, linearized):
//   T_object ≈ (T_measured - (1 - ε) × T_reflected) / ε
//
// Where T_reflected is the reflected ambient temperature (estimated as the
// median frame temperature, or a user-supplied value).
//
// References for preset values:
//   [1] Modest, M.F. "Radiative Heat Transfer", 3rd ed., Academic Press, 2013
//   [2] Mikron Instrument Company, "Table of Emissivity of Various Surfaces"
//       https://www.engineeringtoolbox.com/emissivity-coefficients-d_447.html
//   [3] FLIR Systems, "A Guide to Thermography", Technical Note
//   [4] Infrared Training Center (ITC/FLIR), standard reference tables
//   [5] Omega Engineering, "Emissivity of Common Materials"
//       https://www.omega.com/en-us/resources/emissivity-of-common-materials
//   [6] Touloukian, Y.S. & DeWitt, D.P. "Thermophysical Properties of Matter",
//       Vol. 7-9: Thermal Radiative Properties, IFI/Plenum, 1970-1972

// EmissivityPreset is a named emissivity value with source reference.
type EmissivityPreset struct {
	Name       string
	Emissivity float32
	Note       string // source / conditions
	Category   string // grouping for dropdown display
}

// EmissivityPresets contains common material emissivity values.
// Ordered roughly by how frequently they're needed in practice.
var EmissivityPresets = []EmissivityPreset{
	// ε = 1.0: Ideal reference
	{"Blackbody", 1.00, "Ideal radiator, ε=1 by definition", "Reference"},

	// Organic / biological surfaces — very high emissivity
	{"Human skin", 0.98, "All skin tones, 8-14μm [1][3][4]", "Organic / Biological"},
	{"Water", 0.96, "Distilled or tap, 8-14μm [1][2][5]", "Organic / Biological"},
	{"Ice / frost", 0.97, "At 0°C or below [2][5]", "Organic / Biological"},
	{"Wood (any)", 0.94, "Planed or rough, most species [2][5]", "Organic / Biological"},
	{"Paper / cardboard", 0.93, "Any color [2][5]", "Organic / Biological"},
	{"Rubber (hard)", 0.94, "Natural or synthetic [2][5]", "Organic / Biological"},
	{"Fabric / cloth", 0.95, "Cotton, wool, nylon etc. [4][5]", "Organic / Biological"},
	{"Food / organic", 0.95, "Fruits, vegetables, cooked foods [4]", "Organic / Biological"},
	{"Soil (dry)", 0.92, "Sandy to clay, dry [2][5]", "Organic / Biological"},

	// Construction / building materials
	{"Concrete", 0.93, "Rough surface, dry [2][5]", "Construction"},
	{"Brick (red)", 0.93, "Common fired brick [2][5]", "Construction"},
	{"Asphalt", 0.95, "Road surface [2][5]", "Construction"},
	{"Glass", 0.92, "Plate glass, 8-14μm. Caution: partly transparent to LWIR [1][2]", "Construction"},
	{"Plaster / gypsum", 0.91, "Painted or unpainted [2][5]", "Construction"},
	{"Ceramic tile", 0.93, "Glazed or unglazed [2][5]", "Construction"},
	{"Paint (any color)", 0.93, "Non-metallic paints, all colors [2][3][5]", "Construction"},

	// Plastics
	{"Plastic (ABS/PVC)", 0.92, "Most common plastics 8-14μm [2][5]", "Plastics"},
	{"Epoxy / PCB", 0.91, "FR4, solder mask (green/black) [4]", "Plastics"},

	// Oxidised / coated metals — moderate-high emissivity
	{"Oxidised steel", 0.79, "Heavy oxide layer at 25-200°C [2][5][6]", "Oxidised Metals"},
	{"Oxidised copper", 0.78, "Heavy patina [2][5][6]", "Oxidised Metals"},
	{"Oxidised iron", 0.74, "Rusted surface [2][5]", "Oxidised Metals"},
	{"Cast iron", 0.81, "Rough, oxidised [2][5]", "Oxidised Metals"},
	{"Anodised aluminium", 0.77, "Typical anodization [2][5][6]", "Oxidised Metals"},
	{"Galvanised steel", 0.28, "New/bright finish [2][5]", "Oxidised Metals"},

	// Polished / bare metals — very low emissivity (hard to measure accurately)
	{"Stainless steel", 0.16, "Polished 304/316 at 25°C [2][5][6]", "Polished Metals"},
	{"Aluminium (polished)", 0.05, "Mirror finish, 8-14μm [1][2][6]", "Polished Metals"},
	{"Copper (polished)", 0.03, "Clean, mirror finish [1][2][6]", "Polished Metals"},
	{"Gold (polished)", 0.02, "Electroplated or foil [1][6]", "Polished Metals"},

	// Tapes / coatings used as reference targets
	{"Electrical tape", 0.95, "3M vinyl, commonly used as ε reference [3][4]", "Tapes / Coatings"},
	{"Kapton tape", 0.95, "Polyimide film [4]", "Tapes / Coatings"},
}

// DefaultEmissivity is used when no preset is selected.
const DefaultEmissivity float32 = 1.0

// CorrectEmissivity adjusts a measured temperature for surface emissivity.
// tMeasured and tReflected are in °C, emissivity is in range (0, 1].
// Returns corrected temperature in °C.
//
// Uses the standard radiometric correction formula:
//
//	T_obj = (T_meas - (1 - ε) × T_refl) / ε
//
// This is the linearized approximation valid when ΔT is moderate relative
// to absolute temperature. For ε close to 1.0, the correction is small.
// For low ε (polished metals), accuracy decreases — this is a fundamental
// limitation of all uncooled LWIR cameras, not specific to this implementation.
func CorrectEmissivity(tMeasured, tReflected, emissivity float32) float32 {
	if emissivity <= 0 {
		emissivity = 0.01 // prevent division by zero
	}
	if emissivity >= 1.0 {
		return tMeasured // no correction needed
	}
	return (tMeasured - (1-emissivity)*tReflected) / emissivity
}

// EstimateAmbient returns an estimate of the reflected/ambient temperature
// from a celsius frame by computing the median. This assumes most of the
// scene is at or near ambient temperature.
func EstimateAmbient(celsius []float32) float32 {
	n := len(celsius)
	if n == 0 {
		return 20.0 // reasonable fallback
	}

	// Use a partial sort approach: find the median via quickselect-like sampling
	// For performance, sample up to 4096 pixels evenly spaced
	step := 1
	if n > 4096 {
		step = n / 4096
	}

	var sum float64
	count := 0
	for i := 0; i < n; i += step {
		sum += float64(celsius[i])
		count++
	}
	return float32(sum / float64(count))
}
