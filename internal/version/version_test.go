package version

import "testing"

func TestStringIsNonEmpty(t *testing.T) {
	if String() == "" {
		t.Fatal("version.String() returned an empty string")
	}
}

func TestStringDefaultsToDev(t *testing.T) {
	if got := String(); got != "dev" {
		t.Fatalf("version.String() = %q, want %q (default build)", got, "dev")
	}
}
