package processjob

import "testing"

func TestParseMemorySize(t *testing.T) {
	good := map[string]uint64{
		"8G": 8 << 30, "8g": 8 << 30, "8GiB": 8 << 30, "8GB": 8 << 30, "8Gi": 8 << 30,
		"512M": 512 << 20, "2t": 2 << 40, "16777216": 16 << 20, "65536K": 64 << 20,
		"0": 0, "off": 0, "None": 0, "unlimited": 0, "infinity": 0,
	}
	for in, want := range good {
		got, err := ParseMemorySize(in)
		if err != nil || got != want {
			t.Errorf("ParseMemorySize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "abc", "-1G", "1.5G", "8X", "1024", "99999999999999999999T", "G"} {
		if _, err := ParseMemorySize(in); err == nil {
			t.Errorf("ParseMemorySize(%q) accepted", in)
		}
	}
}

func TestSandboxOptionsDefaultAndOverride(t *testing.T) {
	t.Setenv(MemoryLimitEnv, "")
	opts, err := SandboxOptions()
	if err != nil || uint64(opts.MemoryBytes) != DefaultMemoryBytes || opts.MaxProcesses != DefaultMaxProcesses {
		t.Fatalf("default options = %+v, %v", opts, err)
	}
	t.Setenv(MemoryLimitEnv, "2G")
	if opts, err = SandboxOptions(); err != nil || opts.MemoryBytes != 2<<30 {
		t.Fatalf("override options = %+v, %v", opts, err)
	}
	t.Setenv(MemoryLimitEnv, "lots")
	if _, err = SandboxOptions(); err == nil {
		t.Fatal("invalid override must fail closed")
	}
}
