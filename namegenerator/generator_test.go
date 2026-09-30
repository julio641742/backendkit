package namegenerator

import (
	"regexp"
	"testing"
)

func TestRandom(t *testing.T) {
	re := regexp.MustCompile(`^[a-z]+-[a-z]+$`)
	for range 100 {
		if name := Random("-"); !re.MatchString(name) {
			t.Fatalf("Random = %q", name)
		}
	}
}

func TestRandomWithSuffix(t *testing.T) {
	tests := []struct {
		digits int
		re     string
	}{
		{4, `^[a-z]+_[a-z]+_[0-9]{4}$`},
		{0, `^[a-z]+_[a-z]+_[0-9]$`},
		{99, `^[a-z]+_[a-z]+_[0-9]{18}$`},
	}
	for _, tt := range tests {
		re := regexp.MustCompile(tt.re)
		for range 100 {
			if name := RandomWithSuffix("_", tt.digits); !re.MatchString(name) {
				t.Fatalf("RandomWithSuffix(%d) = %q, want %s", tt.digits, name, tt.re)
			}
		}
	}
}
