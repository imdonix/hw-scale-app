package hagrid

import "math"

// Composition is the body composition derived from a Scale 3 measurement
// using openScale's independently derived adult four-electrode model.
type Composition struct {
	ImpedanceOhm      float64 `json:"impedance_ohm"`
	BodyFatPercent    float64 `json:"body_fat_percent"`
	WaterPercent      float64 `json:"water_percent"`
	SkeletalMuscleKg  float64 `json:"skeletal_muscle_kg"`
	SkeletalMusclePct float64 `json:"skeletal_muscle_percent"`
	BoneMineralKg     float64 `json:"bone_mineral_kg"`
	LeanBodyMassKg    float64 `json:"lean_body_mass_kg"`
	ProteinPercent    float64 `json:"protein_percent"`
	BMRKcal           int     `json:"bmr_kcal"`
	VisceralFatLevel  int     `json:"visceral_fat_level"`
}

type coefficients struct {
	water, ffm, muscle, bone, bmr, visceral [7]float64
}

// Feature order: h²/z, z, weight, height, age², age, 1.
var (
	femaleCoef = coefficients{
		water:    [7]float64{0.445999704, 0.0139999486, 0.167000103, -0.102999643, -0.00186499864, 0.172699790, 2.47318013},
		ffm:      [7]float64{-0.0000000591396, -0.0150000073, 0.198000056, 0.313999865, -0.00148397395, 0.124597440, -16.3761132},
		muscle:   [7]float64{-0.0000000596289, -0.0130000008, 0.180999914, 0.286000079, -0.00136797854, 0.115597450, -14.1328394},
		bone:     [7]float64{-0.00400158350, -0.00200033477, 0.0160000363, 0.0260015858, -0.000119001796, 0.00930013136, -1.67940940},
		bmr:      [7]float64{1.75784170, -0.181837584, 9.65817197, 5.89515491, 0.00782921597, -2.89574283, -203.303636},
		visceral: [7]float64{-0.00158317035, 0.00544003197, 0.314994178, -0.103662636, 0.000967108734, 0.00527136893, -1.33727631},
	}
	maleCoef = coefficients{
		water:    [7]float64{0.446000315, 0.0140000556, 0.166999963, -0.103000253, -0.00104299708, 0.0356998392, 9.22059951},
		ffm:      [7]float64{0.114000146, -0.0149999678, 0.322000075, 0.272999903, -0.000979001428, 0.0170001265, -13.0029150},
		muscle:   [7]float64{0.107999947, -0.0140000275, 0.305999991, 0.258999945, -0.000929995268, 0.0160995340, -12.5056628},
		bone:     [7]float64{-0.00400070678, -0.00200017483, 0.0160001373, 0.0260008239, -0.0000579943116, 0.00269952368, -1.50661300},
		bmr:      [7]float64{6.43629949, -0.0002299374, 10.9972143, 4.74191847, 0.0238549107, -6.20381101, -233.995162},
		visceral: [7]float64{-0.168283428, -0.0119657591, 0.377569276, -0.138136532, 0.000252403405, 0.134562343, 17.0432610},
	}
)

func eval(c [7]float64, x [7]float64) float64 {
	var r float64
	for i := range c {
		r += c[i] * x[i]
	}
	return r
}

// CalculateComposition returns nil when the inputs are outside the adult
// model's supported range.
func CalculateComposition(heightCm, weightKg float64, age int, male bool, impedanceOhm float64) *Composition {
	if heightCm <= 0 || weightKg <= 0 || impedanceOhm <= 0 || age < 18 {
		return nil
	}
	h, w, a, z := heightCm, weightKg, float64(age), impedanceOhm
	if h < 120 || h > 230 || w < 20 || w > 250 || z < 100 || z > 1000 {
		return nil
	}
	x := [7]float64{h * h / z, z, w, h, a * a, a, 1}
	c := femaleCoef
	if male {
		c = maleCoef
	}
	waterKg := eval(c.water, x)
	ffm := eval(c.ffm, x)
	muscle := eval(c.muscle, x)
	bone := eval(c.bone, x)
	smm := 0.605*muscle - 1.833
	fatPct := (1 - ffm/w) * 100
	waterPct := waterKg / w * 100
	proteinPct := (ffm - waterKg - bone) / w * 100
	bmr := int(eval(c.bmr, x))
	vfl := max(1, int(eval(c.visceral, x)))

	finite := func(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
	if !finite(fatPct) || fatPct < 0 || fatPct > 75 ||
		!finite(waterPct) || waterPct < 10 || waterPct > 85 ||
		!finite(smm) || smm < 0 || smm > w ||
		!finite(bone) || bone < 0 || bone > 10 ||
		!finite(proteinPct) || proteinPct < 0 || proteinPct > 40 ||
		bmr < 500 || bmr > 5000 || vfl < 1 || vfl > 60 {
		return nil
	}
	return &Composition{
		ImpedanceOhm:      z,
		BodyFatPercent:    fatPct,
		WaterPercent:      waterPct,
		SkeletalMuscleKg:  smm,
		SkeletalMusclePct: smm / w * 100,
		BoneMineralKg:     bone,
		LeanBodyMassKg:    ffm,
		ProteinPercent:    proteinPct,
		BMRKcal:           bmr,
		VisceralFatLevel:  vfl,
	}
}
