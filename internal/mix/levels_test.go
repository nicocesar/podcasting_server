package mix

import (
	"math"
	"strings"
	"testing"
	"time"
)

// These tests need no ffmpeg, deliberately. Everything in mix_test.go skips
// on a machine without one, and the rules that decide how loud a published
// episode is should not be the part of this package that goes untested
// there.

func speechAt(lufs, peak, mean, max float64) measurement {
	return measurement{
		LUFS: lufs, Peak: peak, Mean: mean, Max: max,
		HasLoudness: true, HasVolume: true,
	}
}

func closeTo(t *testing.T, got, want float64, what string) {
	t.Helper()
	if math.Abs(got-want) > 0.01 {
		t.Errorf("%s = %.3f, want %.3f", what, got, want)
	}
}

func TestBedSitsBedGainDBUnderTheNarration(t *testing.T) {
	// The whole point of measuring: the bed's own level cancels out, so
	// two beds that arrive 10dB apart still end up in the same place
	// relative to the narration. This is what the old code claimed to do
	// and could not, having never measured either side.
	speech := speechAt(-20, -5, -24, -6)
	for _, bed := range []measurement{
		{LUFS: -8, HasLoudness: true},
		{LUFS: -18, HasLoudness: true},
	} {
		landed := bed.LUFS + bedGain(speech, bed) // where the bed ends up
		closeTo(t, landed-speech.LUFS, BedGainDB, "bed relative to narration")
	}
}

func TestBedFallsBackToAFlatAttenuation(t *testing.T) {
	speech := speechAt(-20, -5, -24, -6)
	for _, tc := range []struct {
		name string
		bed  measurement
	}{
		{"never measured", measurement{}},
		{"digital silence", measurement{LUFS: math.Inf(-1), HasLoudness: true}},
		{"not a number", measurement{LUFS: math.NaN(), HasLoudness: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Not the relative placement, but not a wild gain either: the
			// flat attenuation is what the mixer did before it measured.
			closeTo(t, bedGain(speech, tc.bed), BedGainDB, "bed gain")
		})
	}
}

func TestEffectIsHeldToWhicheverRuleBindsHarder(t *testing.T) {
	speech := speechAt(-20, -5, -24, -6)

	t.Run("a hot effect is pulled down by its peak", func(t *testing.T) {
		// Peaks at -1 against narration peaking at -6: the peak rule wants
		// -5dB, the mean rule wants -3 - (-10) - 3 = less. Tighter wins.
		fx := measurement{Mean: -10, Max: -1, HasVolume: true}
		g := effectGain(speech, fx)
		if fx.Max+g > speech.Max+0.01 {
			t.Errorf("effect still peaks at %.2f against narration at %.2f", fx.Max+g, speech.Max)
		}
	})

	t.Run("a timid effect is brought up to be audible", func(t *testing.T) {
		fx := measurement{Mean: -50, Max: -40, HasVolume: true}
		g := effectGain(speech, fx)
		if g <= 0 {
			t.Errorf("gain = %.2f; a quiet effect should be raised, not left inaudible", g)
		}
		// But never past the peak rule.
		if fx.Max+g > speech.Max+0.01 {
			t.Errorf("effect raised to %.2f, above the narration's %.2f", fx.Max+g, speech.Max)
		}
	})

	t.Run("an unmeasurable effect is left alone", func(t *testing.T) {
		closeTo(t, effectGain(speech, measurement{}), 0, "gain")
		closeTo(t, effectGain(speech, measurement{Mean: math.NaN(), Max: -3, HasVolume: true}), 0, "gain")
	})
}

func TestFinalGainMovesTheNarrationToTarget(t *testing.T) {
	speech := speechAt(-24, -9, -28, -10)
	g, clamp := finalGain(speech)
	closeTo(t, speech.LUFS+g, TargetLUFS, "narration after gain")
	if clamp != "" {
		t.Errorf("clamped by %q, but neither guard should have bound here", clamp)
	}
}

func TestFinalGainWillNotClip(t *testing.T) {
	// Quiet on average but already peaking near full scale — a story with
	// one shout in it. Reaching the target would push it over.
	speech := speechAt(-24, -1.5, -28, -1.5)
	g, clamp := finalGain(speech)
	if clamp != "peak" {
		t.Errorf("clamp = %q, want %q", clamp, "peak")
	}
	if got := speech.Peak + g; got > TruePeakCeilingDB+0.01 {
		t.Errorf("true peak would land at %.2f dBFS, above the %.1f ceiling", got, TruePeakCeilingDB)
	}
}

func TestFinalGainWillNotAmplifyABrokenRender(t *testing.T) {
	// Measured, but only just: above the silence floor and far below the
	// target. The peak ceiling alone would happily allow +40dB here, which
	// would make a quiet failure into a loud one.
	speech := speechAt(-58, -50, -62, -52)
	g, clamp := finalGain(speech)
	if clamp != "boost" {
		t.Errorf("clamp = %q, want %q", clamp, "boost")
	}
	closeTo(t, g, MaxBoostDB, "gain")
}

func TestNarrationBelowTheFloorIsNotUsable(t *testing.T) {
	for _, tc := range []struct {
		name string
		m    measurement
	}{
		{"digital silence", speechAt(math.Inf(-1), math.Inf(-1), math.Inf(-1), math.Inf(-1))},
		{"below the floor", speechAt(SilenceFloorLUFS-1, -60, -70, -62)},
		{"not a number", speechAt(math.NaN(), -5, -24, -6)},
		{"loudness never taken", measurement{Mean: -24, Max: -6, HasVolume: true}},
		{"volume never taken", measurement{LUFS: -20, Peak: -5, HasLoudness: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.m.usable() {
				t.Error("measurement should not be trusted to place levels against")
			}
		})
	}
	if !speechAt(-20, -5, -24, -6).usable() {
		t.Error("an ordinary narration measurement should be usable")
	}
}

func TestUnmeasuredKeepsTheOldBehaviour(t *testing.T) {
	l := unmeasured("because")
	if l.Measured {
		t.Error("Measured should be false")
	}
	if l.Note == "" {
		t.Error("Note should say why")
	}
	// The flat attenuation the mixer used before it could measure.
	closeTo(t, l.BedGain, BedGainDB, "bed gain")
	closeTo(t, l.FinalGain, 0, "final gain")
}

func TestFadesShrinkToFitAShortStory(t *testing.T) {
	t.Run("a real story gets the full fades", func(t *testing.T) {
		in, out := fades(5 * time.Minute)
		if in != BedFadeIn || out != BedFadeOut {
			t.Errorf("fades = %v/%v, want %v/%v", in, out, BedFadeIn, BedFadeOut)
		}
	})

	t.Run("a short story still fades at both ends", func(t *testing.T) {
		total := 3 * time.Second
		in, out := fades(total)
		if in <= 0 || out <= 0 {
			t.Fatalf("fades = %v/%v, want both ends to fade", in, out)
		}
		if in+out > total {
			t.Errorf("fades total %v, longer than the %v story", in+out, total)
		}
		// Proportions preserved, so it is recognisably the same shape.
		wantRatio := float64(BedFadeIn) / float64(BedFadeOut)
		if got := float64(in) / float64(out); math.Abs(got-wantRatio) > 0.01 {
			t.Errorf("fade ratio = %.3f, want %.3f", got, wantRatio)
		}
	})

	t.Run("nothing to fade", func(t *testing.T) {
		if in, out := fades(0); in != 0 || out != 0 {
			t.Errorf("fades = %v/%v, want none", in, out)
		}
	})
}

// The parsers read ffmpeg's own output. mix_test.go exercises them against
// a live binary; these cases pin the shapes that only turn up when
// something has gone wrong, which is exactly when the guards have to work.
func TestParseLoudness(t *testing.T) {
	const good = `[Parsed_loudnorm_5 @ 0x55]
{
	"input_i" : "-32.35",
	"input_tp" : "-31.50",
	"input_lra" : "0.00",
	"input_thresh" : "-42.35",
	"normalization_type" : "dynamic"
}
`
	m, err := parseLoudness(good)
	if err != nil {
		t.Fatal(err)
	}
	closeTo(t, m.LUFS, -32.35, "LUFS")
	closeTo(t, m.Peak, -31.50, "peak")
	if !m.HasLoudness {
		t.Error("HasLoudness should be set")
	}

	t.Run("digital silence reports as -inf", func(t *testing.T) {
		m, err := parseLoudness(`{"input_i" : "-inf", "input_tp" : "-inf"}`)
		if err != nil {
			t.Fatal(err)
		}
		if !math.IsInf(m.LUFS, -1) {
			t.Errorf("LUFS = %v, want -Inf", m.LUFS)
		}
		// And -Inf has to fail the floor test rather than sail past it.
		if m.merge(measurement{Mean: -90, Max: -90, HasVolume: true}).usable() {
			t.Error("silence measured as -inf must not be usable")
		}
	})

	t.Run("no report at all", func(t *testing.T) {
		if _, err := parseLoudness("ffmpeg version 8.1\nnothing to see"); err == nil {
			t.Error("want an error when the report is missing")
		}
	})
}

func TestParseVolume(t *testing.T) {
	const good = `[Parsed_volumedetect_0 @ 0x61] n_samples: 441000
[Parsed_volumedetect_0 @ 0x61] mean_volume: -44.5 dB
[Parsed_volumedetect_0 @ 0x61] max_volume: -41.5 dB
[Parsed_volumedetect_0 @ 0x61] histogram_41db: 91908
`
	m, err := parseVolume(good)
	if err != nil {
		t.Fatal(err)
	}
	closeTo(t, m.Mean, -44.5, "mean")
	closeTo(t, m.Max, -41.5, "max")
	if !m.HasVolume {
		t.Error("HasVolume should be set")
	}

	t.Run("silence", func(t *testing.T) {
		m, err := parseVolume("mean_volume: -inf dB\nmax_volume: -inf dB")
		if err != nil {
			t.Fatal(err)
		}
		if !math.IsInf(m.Mean, -1) {
			t.Errorf("mean = %v, want -Inf", m.Mean)
		}
	})

	t.Run("no report at all", func(t *testing.T) {
		if _, err := parseVolume("Error while decoding stream"); err == nil {
			t.Error("want an error when the report is missing")
		}
	})
}

func TestMergeKeepsBothHalvesOfAMeasurement(t *testing.T) {
	loud := measurement{LUFS: -20, Peak: -5, HasLoudness: true}
	vol := measurement{Mean: -24, Max: -6, HasVolume: true}
	m := loud.merge(vol)
	if !m.HasLoudness || !m.HasVolume {
		t.Fatal("merge lost half the measurement")
	}
	closeTo(t, m.LUFS, -20, "LUFS")
	closeTo(t, m.Max, -6, "max")
}

func TestLastLinesTrimsAChattyLog(t *testing.T) {
	// The analysis filters print a line every 100ms, so an error message
	// that quoted the whole log would bury the useful part.
	var b strings.Builder
	for i := 0; i < 500; i++ {
		b.WriteString("noise\n")
	}
	b.WriteString("Error while decoding stream #3:0")
	got := lastLines(b.String(), 5)
	if !strings.Contains(got, "Error while decoding") {
		t.Errorf("trimmed away the error: %q", got)
	}
	if strings.Count(got, "noise") > 4 {
		t.Errorf("kept too much: %q", got)
	}
}
