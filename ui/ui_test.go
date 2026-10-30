package ui

import "testing"

func TestDisplaySymbol(t *testing.T) {
	// The symbol alone: a caller naming the asset it configured itself.
	cases := map[string]string{"UARG": "uARG", "WARG": "wARG", "ARG": "ARG", "GOLD": "GOLD", "U": "U", "": ""}
	for in, want := range cases {
		if got := DisplaySymbol(in); got != want {
			t.Errorf("DisplaySymbol(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDisplaySymbolByID(t *testing.T) {
	const real = "aa00000000000000000000000000000000000000000000000000000000000001"
	const fake = "bb00000000000000000000000000000000000000000000000000000000000002"
	t.Cleanup(func() { SetCanonicalAsset("UARG", "") })

	// Nothing registered: every reserved symbol is unverified.
	SetCanonicalAsset("UARG", "")
	for sym, want := range map[string]string{"UARG": "UARG (unverified)", "WARG": "WARG (unverified)", "ARG": "ARG (unverified)", "GOLD": "GOLD"} {
		if got := DisplaySymbol(sym, real); got != want {
			t.Errorf("unregistered DisplaySymbol(%q, id) = %q, want %q", sym, got, want)
		}
	}
	SetCanonicalAsset("uarg", "AA00000000000000000000000000000000000000000000000000000000000001")
	if got := CanonicalAsset("UARG"); got != real {
		t.Fatalf("CanonicalAsset = %q", got)
	}
	for _, tc := range []struct{ sym, id, want string }{
		{"UARG", real, "uARG"},
		{"UARG", fake, "UARG (unverified)"},
		{"WARG", real, "WARG (unverified)"}, // the uARG id does not make another symbol genuine
		{"ARG", fake, "ARG (unverified)"},
		{"GOLD", fake, "GOLD"},
	} {
		if got := DisplaySymbol(tc.sym, tc.id); got != tc.want {
			t.Errorf("DisplaySymbol(%q, %s…) = %q, want %q", tc.sym, tc.id[:4], got, tc.want)
		}
		if Unverified(tc.sym, tc.id) != (tc.want != "uARG" && tc.want != "GOLD") {
			t.Errorf("Unverified(%q, %s…) wrong", tc.sym, tc.id[:4])
		}
	}
}
