package version

import (
	"runtime/debug"
	"testing"
)

func TestNormalize(t *testing.T) {
	cases := map[string]string{"": Devel, "(devel)": Devel, " v0.2.0 ": "v0.2.0", "v0.2.0-s10+dirty": "v0.2.0-s10+dirty", "v0.0.0-20260903085424-f3905017fae8": "v0.0.0-20260903085424-f3905017fae8"}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValueUsesBuildInfo(t *testing.T) {
	orig := readBuildInfo
	defer func() { readBuildInfo = orig }()
	readBuildInfo = func() (*debug.BuildInfo, bool) { return &debug.BuildInfo{Main: debug.Module{Version: "v0.2.0"}}, true }
	if got := Value(); got != "v0.2.0" {
		t.Fatalf("Value() = %q, want v0.2.0", got)
	}
	readBuildInfo = func() (*debug.BuildInfo, bool) { return nil, false }
	if got := Value(); got != Devel {
		t.Fatalf("Value() without build info = %q, want %q", got, Devel)
	}
}
