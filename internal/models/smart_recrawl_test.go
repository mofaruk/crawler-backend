package models

import (
	"testing"
	"time"
)

// An unset window must resolve to the default rather than to "no bound".
//
// This is the case that matters most: every site created before the field
// existed has a zero here, and reading the field directly would give those
// sites the old skip-forever behaviour — the exact bug the bound removes.
func TestSmartRecrawlMaxAgeDefaultsWhenUnset(t *testing.T) {
	var s Site

	got := s.SmartRecrawlMaxAge()
	want := time.Duration(SmartRecrawlDefaultMaxAgeHours) * time.Hour

	if got != want {
		t.Fatalf("unset window = %v, want the %v default", got, want)
	}

	if got == 0 {
		t.Fatal("unset window resolved to zero, which would reuse cached results forever")
	}
}

func TestSmartRecrawlMaxAgeUsesConfiguredWindow(t *testing.T) {
	for _, hours := range SmartRecrawlMaxAgeChoices {
		s := Site{SmartRecrawlMaxAgeHours: hours}

		want := time.Duration(hours) * time.Hour
		if got := s.SmartRecrawlMaxAge(); got != want {
			t.Errorf("%dh window = %v, want %v", hours, got, want)
		}
	}
}

// Out-of-range values must not widen the window past the largest option. A
// negative or absurd value stored by an older client would otherwise mean
// results are reused for far longer than any offered choice allows.
func TestSmartRecrawlMaxAgeClampsOutOfRange(t *testing.T) {
	max := SmartRecrawlMaxAgeChoices[len(SmartRecrawlMaxAgeChoices)-1]
	ceiling := time.Duration(max) * time.Hour

	cases := []struct {
		name  string
		hours int
		want  time.Duration
	}{
		{"negative", -5, time.Duration(SmartRecrawlDefaultMaxAgeHours) * time.Hour},
		{"zero", 0, time.Duration(SmartRecrawlDefaultMaxAgeHours) * time.Hour},
		{"above the largest choice", 24 * 365, ceiling},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := Site{SmartRecrawlMaxAgeHours: tc.hours}

			got := s.SmartRecrawlMaxAge()
			if got != tc.want {
				t.Fatalf("%d hours = %v, want %v", tc.hours, got, tc.want)
			}
			if got > ceiling {
				t.Fatalf("%d hours resolved to %v, past the %v ceiling", tc.hours, got, ceiling)
			}
		})
	}
}

// Every offered window must be short enough that a page falling out of cache
// is caught within a day. An option longer than that would reintroduce the
// "reported as cached for weeks" failure in a milder form.
func TestSmartRecrawlChoicesAreAllWithinADay(t *testing.T) {
	if len(SmartRecrawlMaxAgeChoices) == 0 {
		t.Fatal("no re-check windows offered")
	}

	for i, hours := range SmartRecrawlMaxAgeChoices {
		if hours <= 0 {
			t.Errorf("choice %d is %dh: a non-positive window means never re-check", i, hours)
		}
		if hours > 24 {
			t.Errorf("choice %d is %dh, longer than a day", i, hours)
		}
		if i > 0 && hours <= SmartRecrawlMaxAgeChoices[i-1] {
			t.Errorf("choices are not ascending: %dh follows %dh", hours, SmartRecrawlMaxAgeChoices[i-1])
		}
	}
}

// The default must itself be one of the offered windows, or the UI would show
// a value the dropdown cannot represent.
func TestSmartRecrawlDefaultIsAnOfferedChoice(t *testing.T) {
	for _, hours := range SmartRecrawlMaxAgeChoices {
		if hours == SmartRecrawlDefaultMaxAgeHours {
			return
		}
	}

	t.Fatalf("default %dh is not among the offered choices %v",
		SmartRecrawlDefaultMaxAgeHours, SmartRecrawlMaxAgeChoices)
}
