package capability

import "testing"

func TestParseNvidiaSMI(t *testing.T) {
	out := "NVIDIA GeForce RTX 3060, 12288, 1024, 37, 54\nNVIDIA T400, 2048, [N/A], [Not Supported], 40\n"
	g := parseNvidiaSMI(out)
	if len(g) != 2 {
		t.Fatalf("want 2 gpus, got %d", len(g))
	}
	if g[0].Name != "NVIDIA GeForce RTX 3060" || g[0].VRAMTotal != 12288<<20 || g[0].VRAMUsed != 1024<<20 {
		t.Fatalf("row 0: %+v", g[0])
	}
	if g[0].Usage == nil || *g[0].Usage != 37 || g[0].Temp == nil || *g[0].Temp != 54 {
		t.Fatalf("row 0 usage/temp: %+v", g[0])
	}
	if g[1].Usage != nil || g[1].VRAMUsed != 0 {
		t.Fatalf("row 1 should have unknown usage: %+v", g[1])
	}
}

func TestGPUUtilization(t *testing.T) {
	engines := map[string]float64{
		"pid_10_luid_0x00000000_0x0000C6F2_phys_0_eng_0_engtype_3D":          30,
		"pid_20_luid_0x00000000_0x0000C6F2_phys_0_eng_0_engtype_3D":          25,
		"pid_20_luid_0x00000000_0x0000C6F2_phys_0_eng_3_engtype_VideoDecode": 40,
		"pid_30_luid_0x00000000_0x0000ABCD_phys_0_eng_0_engtype_3D":          90,
		"pid_40_luid_0x00000000_0x0000ABCD_phys_0_eng_0_engtype_3D":          50,
		"garbage": 99,
	}
	u := gpuUtilization(engines)
	if u["00000000_0000C6F2"] != 55 {
		t.Fatalf("C6F2: want 55 (3D 30+25 beats decode 40), got %v", u["00000000_0000C6F2"])
	}
	if u["00000000_0000ABCD"] != 100 {
		t.Fatalf("ABCD: want clamp to 100, got %v", u["00000000_0000ABCD"])
	}
	if len(u) != 2 {
		t.Fatalf("want 2 adapters, got %v", u)
	}
}

func TestLUIDKeyMatchesInstance(t *testing.T) {
	k, ok := luidFromInstance("luid_0x00000000_0x0000c6f2_phys_0")
	if !ok || k != luidKey(0, 0xC6F2) {
		t.Fatalf("got %q, want %q", k, luidKey(0, 0xC6F2))
	}
}
