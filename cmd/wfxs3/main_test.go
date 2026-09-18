//go:build windows && cgo

package main

import (
	"testing"
	"time"
)

func TestDecodeANSI(t *testing.T) {
	const windows1251 = 1251
	tests := []struct {
		name  string
		value []byte
		want  string
	}{
		{
			// "Юрий" in Windows-1251, followed by the rest of a MAX_PATH buffer.
			name:  "cyrillic",
			value: append([]byte(`C:\Users\`+"\xDE\xF0\xE8\xE9"+`\wincmd.ini`), 0, 'x', 'y'),
			want:  `C:\Users\Юрий\wincmd.ini`,
		},
		{name: "ascii", value: []byte(`C:\totalcmd\wincmd.ini`), want: `C:\totalcmd\wincmd.ini`},
		{name: "empty", value: make([]byte, 260), want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := decodeANSI(windows1251, test.value); got != test.want {
				t.Fatalf("decodeANSI = %q, want %q", got, test.want)
			}
		})
	}
}

// Once FsExecuteFileW is exported, Total Commander calls it for Enter as well:
// "open" must still make it download and open the object.
func TestClassifyVerb(t *testing.T) {
	tests := map[string]verbKind{
		"open":        verbOpen,
		"OPEN":        verbOpen,
		"properties":  verbProperties,
		"Properties":  verbProperties,
		"quote debug": verbOther,
		"chmod 755":   verbOther,
		"":            verbOther,
	}
	for verb, want := range tests {
		if got := classifyVerb(verb); got != want {
			t.Errorf("classifyVerb(%q) = %d, want %d", verb, got, want)
		}
	}
}

func TestInterfaceVersion(t *testing.T) {
	if got := interfaceVersion(1, 30); got != "1.30" {
		t.Fatalf("interfaceVersion(1, 30) = %q, want 1.30", got)
	}
	if got := interfaceVersion(2, 5); got != "2.05" {
		t.Fatalf("interfaceVersion(2, 5) = %q, want 2.05", got)
	}
}

func TestFileTime(t *testing.T) {
	if low, high := fileTime(time.Time{}); low != noFileTimeLow || high != noFileTimeHigh {
		t.Fatalf("zero time = %#x/%#x, want the WFX no-time value", low, high)
	}
	if low, high := fileTime(time.Unix(0, 0)); low != 0xD53E8000 || high != 0x019DB1DE {
		t.Fatalf("Unix epoch = %#x/%#x, want 0xd53e8000/0x19db1de", low, high)
	}
}
