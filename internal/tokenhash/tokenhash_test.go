package tokenhash

import "testing"

func TestHashAndCheck(t *testing.T) {
	h := Hash("PLACEHOLDER-not-a-secret-1")
	if len(h) != 64 || !Valid(h) {
		t.Fatalf("Hash = %q", h)
	}
	if !Check("PLACEHOLDER-not-a-secret-1", h) {
		t.Fatal("right token rejected")
	}
	for _, c := range []struct{ tok, hash string }{
		{"other", h}, {"PLACEHOLDER-not-a-secret-1", ""}, {"", h}, {"x", "zz"},
	} {
		if Check(c.tok, c.hash) {
			t.Fatalf("Check(%q,%q) accepted", c.tok, c.hash)
		}
	}
}

func TestValid(t *testing.T) {
	good := Hash("a")
	for s, want := range map[string]bool{
		good:             true,
		"":               false,
		good[:63]:        false,
		good + "0":       false,
		"G" + good[1:]:   false,
		"A" + good[1:]:   false, // lowercase only
		" " + good[1:]:   false,
		good[:63] + "\n": false,
	} {
		if got := Valid(s); got != want {
			t.Errorf("Valid(%q) = %v, want %v", s, got, want)
		}
	}
}
