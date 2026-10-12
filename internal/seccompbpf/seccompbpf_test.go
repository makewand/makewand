package seccompbpf

import (
	"encoding/binary"
	"io"
	"testing"
)

type insn struct {
	code   uint16
	jt, jf uint8
	k      uint32
}

func decode(t *testing.T, b []byte) []insn {
	t.Helper()
	if len(b)%8 != 0 {
		t.Fatalf("filter length %d not a multiple of 8", len(b))
	}
	out := make([]insn, 0, len(b)/8)
	for i := 0; i < len(b); i += 8 {
		out = append(out, insn{binary.LittleEndian.Uint16(b[i:]), b[i+2], b[i+3], binary.LittleEndian.Uint32(b[i+4:])})
	}
	return out
}

// run interprets the program for one (arch, nr) pair.
func run(t *testing.T, prog []insn, arch, nr uint32) uint32 {
	t.Helper()
	var acc uint32
	for pc := 0; pc < len(prog); pc++ {
		in := prog[pc]
		switch in.code {
		case 0x20:
			if in.k == 4 {
				acc = arch
			} else {
				acc = nr
			}
		case 0x15: // jeq
			if acc == in.k {
				pc += int(in.jt)
			} else {
				pc += int(in.jf)
			}
		case 0x35: // jge
			if acc >= in.k {
				pc += int(in.jt)
			} else {
				pc += int(in.jf)
			}
		case 0x06:
			return in.k
		default:
			t.Fatalf("unexpected opcode %#x", in.code)
		}
	}
	t.Fatal("program fell off the end")
	return 0
}

func TestFilterSemantics(t *testing.T) {
	const allow, eperm = 0x7fff0000, 0x00050001
	cases := map[string]struct {
		arch    uint32
		blocked []uint32
		allowed []uint32
		x32     bool
	}{
		"amd64": {0xc000003e, []uint32{101, 321, 250, 310, 311, 323}, []uint32{0, 1, 59, 257, 435}, true},
		"arm64": {0xc00000b7, []uint32{117, 280, 219, 270, 271, 282}, []uint32{63, 64, 221, 56}, false},
	}
	for goarch, c := range cases {
		prog := decode(t, generate(goarch))
		for _, nr := range c.blocked {
			if got := run(t, prog, c.arch, nr); got != eperm {
				t.Errorf("%s nr %d = %#x, want EPERM", goarch, nr, got)
			}
		}
		for _, nr := range c.allowed {
			if got := run(t, prog, c.arch, nr); got != allow {
				t.Errorf("%s nr %d = %#x, want allow", goarch, nr, got)
			}
		}
		if got := run(t, prog, 0x40000003, 0); got != eperm {
			t.Errorf("%s foreign arch not denied: %#x", goarch, got)
		}
		if c.x32 {
			if got := run(t, prog, c.arch, 0x40000000|1); got != eperm {
				t.Errorf("x32 syscall not denied: %#x", got)
			}
		}
	}
	if generate("riscv64") != nil {
		t.Fatal("unsupported arch must return nil")
	}
}

func TestNewFilterPipe(t *testing.T) {
	if !Supported() {
		if _, err := NewFilterPipe(); err != ErrUnsupportedArch {
			t.Fatalf("want ErrUnsupportedArch, got %v", err)
		}
		return
	}
	r, err := NewFilterPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil || string(data) != string(GenerateFilter()) {
		t.Fatalf("pipe content mismatch: %v", err)
	}
}
