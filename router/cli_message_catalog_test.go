package router

import (
	"bytes"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
)

// findCLIMessages finds what health.py's find_cli_messages finds - every kind,
// on the same line - in every case of the parity corpus, for every CLI of the
// catalog and however the streams were captured.
func TestFindCLIMessagesMatchesPythonOnParityCorpus(t *testing.T) {
	corpus := loadCLIParityCorpus(t)
	providers := slices.Sorted(maps.Keys(cliMessages.providers))
	if want := catalogProviders(t, cliMessageCatalogJSON); !slices.Equal(providers, want) {
		t.Fatalf("compiled providers %v, catalog %v", providers, want)
	}
	found := 0
	for _, c := range corpus.Cases {
		prompt := ""
		if c.Prompt != nil {
			prompt = string(*c.Prompt)
		}
		modes := map[string]cliStreams{}
		if c.Text != nil {
			text := string(*c.Text)
			modes["merged"] = cliStreams{output: text}
			modes["stderr"] = cliStreams{stderr: text, apart: true}
			modes["stdout"] = cliStreams{output: text, apart: true}
		}
		if c.Stdout != nil || c.Stderr != nil {
			streams := cliStreams{apart: true}
			if c.Stdout != nil {
				streams.output = string(*c.Stdout)
			}
			if c.Stderr != nil {
				streams.stderr = string(*c.Stderr)
			}
			modes["apart"] = streams
		}
		for _, provider := range providers {
			kinds := catalogKinds(provider)
			for mode, streams := range modes {
				got := findCLIMessages(provider, kinds, streams, prompt)
				if want := c.expected(provider, mode); !maps.Equal(got, want) {
					t.Errorf("%s: %s %s: Go finds %q, health.py %q", c.ID, provider, mode, got, want)
				}
				found += len(got)
			}
		}
	}
	if found < 1500 {
		t.Fatalf("parity corpus too thin: %d messages found", found)
	}
}

// catalogKinds lists the kinds of provider's catalog entry (without ".marked").
func catalogKinds(provider string) []string {
	var kinds []string
	for kind := range cliMessages.providers[provider].kinds {
		kind, _, _ = strings.Cut(kind, ".")
		if !slices.Contains(kinds, kind) {
			kinds = append(kinds, kind)
		}
	}
	slices.Sort(kinds)
	return kinds
}

// Any output, invalid UTF-8 included, is classified without a panic.
func FuzzFindCLIMessages(f *testing.F) {
	for _, seed := range []string{
		"You've hit your limit · resets 8pm", "codex\nok\nERROR: You've hit your usage limit.",
		"E1012 08:11:57.633062  188 x.go:1] {\n\"a\",\n}\nError: RESOURCE_EXHAUSTED",
		"Error: x\nCaused by:\n    0: Rate limited (429)", "\x1b]0;\xff\xfe\x07  \x1b[31m[ERROR] usage_limit_reached\r\x85",
	} {
		f.Add(seed, "")
	}
	f.Fuzz(func(t *testing.T, output, prompt string) {
		for provider := range cliMessages.providers {
			kinds := catalogKinds(provider)
			for _, streams := range []cliStreams{{output: output}, {stderr: output, apart: true}, {output: prompt, stderr: output, apart: true}} {
				findCLIMessages(provider, kinds, streams, prompt)
			}
		}
	})
}

func TestCLIMessageCatalogEmbedsPythonCatalog(t *testing.T) {
	python, err := os.ReadFile(pythonCLIMessageCatalogFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cliMessageCatalogJSON, python) {
		t.Fatal("the embedded catalog differs from makewand/cli_message_catalog.json")
	}
}

// Names select catalog entries: "gemini" is Gemini CLI's, "antigravity" a
// name agy prints for itself; a name the catalog does not know (a custom
// command) is read with the generic entry. A CLI provider reads the entry of
// the CLI it runs: agy and the older Gemini CLI both serve the router name
// "gemini".
func TestCLICatalogNameForRouterProviders(t *testing.T) {
	for provider, want := range map[string]string{
		"claude": "claude", "codex": "codex", "grok": "grok", "muse": "muse", "agy": "agy",
		"gemini": "gemini", "antigravity": "agy", "aider": "aider", "": "",
	} {
		if got := cliCatalogName(provider); got != want {
			t.Errorf("cliCatalogName(%q) = %q, want %q", provider, got, want)
		}
	}
	for _, tc := range []struct {
		p    *CLIProvider
		want string
	}{
		{NewClaudeCLI("claude"), "claude"},
		{NewCodexCLI("codex"), "codex"},
		{NewAgyCLI("agy"), "agy"},
		{NewGeminiCLI("gemini"), "gemini"},
		{NewCommandCLI("private", "private-cli", nil, PromptModeArg), "private"},
		{NewCommandCLI("claude", "claude-wrapper", nil, PromptModeArg), "claude"},
		{NewCommandCLI("antigravity", "agy-wrapper", nil, PromptModeArg), "agy"},
	} {
		if got := tc.p.messageCatalogName(); got != tc.want {
			t.Errorf("%s (%s) reads catalog entry %q, want %q", tc.p.name, tc.p.provider, got, tc.want)
		}
	}
	refusal := "Error: You have exhausted your quota on this model."
	for provider, want := range map[string]bool{"agy": true, "gemini": true, "antigravity": true, "codex": false, "aider": true} {
		if got := looksLikeQuotaExhaustion(provider, refusal); got != want {
			t.Errorf("looksLikeQuotaExhaustion(%q, agy refusal) = %t, want %t", provider, got, want)
		}
	}
	agyOnly := "Error: Individual quota reached."
	for provider, want := range map[string]bool{"agy": true, "antigravity": true, "gemini": false, "codex": false} {
		if got := looksLikeQuotaExhaustion(provider, agyOnly); got != want {
			t.Errorf("looksLikeQuotaExhaustion(%q, %q) = %t, want %t", provider, agyOnly, got, want)
		}
	}
}

// Catalog patterns match what Python's re matches with re.IGNORECASE |
// re.ASCII: ASCII letters in both cases, nothing that only Unicode case
// folding relates to them.
func TestCLICatalogRegexpFoldsASCIIOnly(t *testing.T) {
	for _, tc := range []struct {
		pattern, text string
		want          bool
	}{
		{`usage limit`, "USAGE Limit", true},
		{`usage limit`, "u\u017fage limit", false}, // long s: Go's (?i) folds it into s
		{`weekly`, "wee\u212aly", false},           // Kelvin sign: Go's (?i) folds it into k
		{`[a-z]+`, "\u017f", false},
		{`[a-zµ]{1,3}:`, "\u00b5s:", true},
		{`[a-zµ]{1,3}:`, "\u03bcs:", false}, // Greek mu: Go's (?i) folds it into µ
		{`[a-zµ]{1,3}:`, "MS:", true},
		{`[^a-z]`, "Q", false},
		{`[^a-z]`, "\u212a", true},
		{`(?-i:ERROR): x`, "ERROR: X", true},
		{`(?-i:ERROR): x`, "error: x", false},
		{`(?-i:[IWEF])\d`, "e1", false},
		{`(?P<level>(?-i:WARN)) +t`, "WARN T", true},
		{`a\sb`, "a\vb", true}, // Python's ASCII \s has \v, Go's does not
		{`a\Sb`, "a\vb", false},
		{`[^\s\]]+$`, "x\vy", false},
		{`\bquota\b`, "QUOTA", true},
		{`[\w'$-]+ cap`, "TEAM'S cap", true},
		{`a{2}`, "AA", true},
		{`\(status 429\)`, "(STATUS 429)", true},
	} {
		re, err := cliCatalogRegexp(tc.pattern)
		if err != nil {
			t.Fatalf("%q: %v", tc.pattern, err)
		}
		if got := re.MatchString(tc.text); got != tc.want {
			t.Errorf("%q matching %q = %t, want %t (compiled %s)", tc.pattern, tc.text, got, tc.want, re)
		}
	}
}

// Syntax the two engines read differently, or that RE2 lacks, is refused
// rather than compiled into a different pattern.
func TestCLICatalogRegexpRefusesForeignSyntax(t *testing.T) {
	for _, pattern := range []string{
		`a(?=b)`, `a(?!b)`, `(?<=a)b`, `(?<name>a)`, `(a)\1`, `(?P=name)`, `(?#note)`, `\x41`, `\u0041`,
		`a\Z`, `\Aa`, `\pL`, `[[:alpha:]]`, `(?U)a+`, `(?x)a b`, `[\S]`, `[\w-a]`, `[z-a]`, `(a`, `a)`, `[a`, `a\`,
	} {
		if re, err := cliCatalogRegexp(pattern); err == nil {
			t.Errorf("%q compiled to %s", pattern, re)
		}
	}
}

// The text primitives behave like Python's str methods.
func TestCLITextPrimitivesMatchPython(t *testing.T) {
	// Python's str.splitlines(): its line boundaries and edge cases.
	for text, want := range map[string][]string{
		"a\r\nb\rc\x0bd\x0ce\x1cf\x1dg\x1eh\u0085i\u2028j\u2029k": {"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k"},
		"\n":            {""},
		"a\n":           {"a"},
		"":              nil,
		"a\n\nb\r\n\r":  {"a", "", "b", ""},
		"a\x1fb\u00a0c": {"a\x1fb\u00a0c"},
	} {
		if got := pySplitLines(text); !slices.Equal(got, want) {
			t.Errorf("pySplitLines(%q) = %q, want %q", text, got, want)
		}
	}
	// [c for c in range(0x110000) if chr(c).isspace()]
	spaces := []rune{0x9, 0xa, 0xb, 0xc, 0xd, 0x1c, 0x1d, 0x1e, 0x1f, 0x20, 0x85, 0xa0, 0x1680,
		0x2000, 0x2001, 0x2002, 0x2003, 0x2004, 0x2005, 0x2006, 0x2007, 0x2008, 0x2009, 0x200a,
		0x2028, 0x2029, 0x202f, 0x205f, 0x3000}
	var got []rune
	for r := rune(0); r <= 0x10ffff; r++ {
		if pyIsSpace(r) {
			got = append(got, r)
		}
	}
	if !slices.Equal(got, spaces) {
		t.Errorf("pyIsSpace set = %U, want %U", got, spaces)
	}
	if got := truncateRunes("错错错x", 2); got != "错错" {
		t.Errorf("truncateRunes = %q", got)
	}
	if got := normalizeCLILine("\x1b[31m  You\u2019ve\u3000\x1f hit\u00a0your limit \x1b[0m"); got != "You've hit your limit" {
		t.Errorf("normalizeCLILine = %q", got)
	}
	if got := normalizeCLILine(strings.Repeat("错", 1990) + strings.Repeat(" ", 5) + "rate limit exceeded"); got != strings.Repeat("错", 1990)+" rate" {
		t.Errorf("normalizeCLILine keeps %d characters, want 1995", len([]rune(got)))
	}
}
