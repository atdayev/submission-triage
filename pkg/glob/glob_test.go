package glob

import "testing"

func TestMatchAny(t *testing.T) {
	cases := []struct {
		patterns []string
		target   string
		want     bool
	}{
		{[]string{"*ACORD*125*"}, "ACORD_125_Acme.pdf", true},
		{[]string{"*ACORD*125*"}, "acord_125_acme.pdf", true},
		{[]string{"*loss*run*"}, "loss_run_5year.pdf", true},
		{[]string{"*loss*run*"}, "claims_history.pdf", false},
		{[]string{"*loss*run*", "*claims*history*"}, "claims_history.pdf", true},
		{[]string{}, "any.pdf", false},
		{[]string{"*ACORD*"}, "random.pdf", false},
		{[]string{"*ACORD*"}, "x/ACORD_125.pdf", true},
		{[]string{"[*ACORD*"}, "ACORD_125.pdf", false},
	}
	for _, tc := range cases {
		got := MatchAny(tc.patterns, tc.target)
		if got != tc.want {
			t.Fatalf("MatchAny(%v, %q) = %v, want %v", tc.patterns, tc.target, got, tc.want)
		}
	}
}

func TestContainsAny(t *testing.T) {
	if !ContainsAny([]string{"Loss Run"}, "this is a Loss Run file") {
		t.Fatal("should match case-insensitively")
	}
	if ContainsAny([]string{"Loss Run"}, "claims history only") {
		t.Fatal("should not match")
	}
	if ContainsAny([]string{""}, "anything") {
		t.Fatal("empty keyword should not match")
	}
	if ContainsAny([]string{"x"}, "") {
		t.Fatal("empty body should not match")
	}
}
