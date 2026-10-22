package engine

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// PEP 440 versions and specifiers, PEP 508 marker evaluation and wheel tag
// compatibility, as far as makewand needs them to classify a failed pip fetch
// (pipReleaseVerdict) and to decide whether a declared URL or VCS dependency
// applies to the interpreter (pipUnfetchedRemoteTarget). They follow
// packaging, the library pip vendors.

// pep440VersionRe is packaging's VERSION_PATTERN.
var pep440VersionRe = regexp.MustCompile(`(?i)^\s*v?(?:(?:([0-9]+)!)?([0-9]+(?:\.[0-9]+)*)` +
	`(?:[-_.]?(alpha|a|beta|b|preview|pre|c|rc)[-_.]?([0-9]+)?)?` +
	`(?:-([0-9]+)|[-_.]?(post|rev|r)[-_.]?([0-9]+)?)?` +
	`(?:[-_.]?(dev)[-_.]?([0-9]+)?)?)` +
	`(?:\+([a-z0-9]+(?:[-_.][a-z0-9]+)*))?\s*$`)

// pep440Version is a parsed version. pre, post and dev are -1 when absent.
type pep440Version struct {
	epoch    int
	release  []int
	preKind  int // 0 a, 1 b, 2 rc; meaningful when pre >= 0
	pre      int
	post     int
	dev      int
	local    []string
	hasLocal bool
}

// parsePEP440 parses a version the way packaging.version.Version does.
func parsePEP440(s string) (pep440Version, bool) {
	m := pep440VersionRe.FindStringSubmatch(s)
	if m == nil {
		return pep440Version{}, false
	}
	v := pep440Version{pre: -1, post: -1, dev: -1}
	if m[1] != "" {
		v.epoch, _ = strconv.Atoi(m[1])
	}
	for _, part := range strings.Split(m[2], ".") {
		n, err := strconv.Atoi(part)
		if err != nil {
			return pep440Version{}, false
		}
		v.release = append(v.release, n)
	}
	if m[3] != "" {
		switch strings.ToLower(m[3]) {
		case "alpha", "a":
			v.preKind = 0
		case "beta", "b":
			v.preKind = 1
		default:
			v.preKind = 2
		}
		v.pre, _ = strconv.Atoi(m[4])
	}
	switch {
	case m[5] != "":
		v.post, _ = strconv.Atoi(m[5])
	case m[6] != "":
		v.post, _ = strconv.Atoi(m[7])
	}
	if m[8] != "" {
		v.dev, _ = strconv.Atoi(m[9])
	}
	if m[10] != "" {
		v.hasLocal = true
		v.local = strings.FieldsFunc(strings.ToLower(m[10]), func(r rune) bool { return r == '-' || r == '_' || r == '.' })
	}
	return v, true
}

func (v pep440Version) isPrerelease() bool { return v.pre >= 0 || v.dev >= 0 }

// publicVersion is v without its local label.
func (v pep440Version) public() pep440Version {
	v.local, v.hasLocal = nil, false
	return v
}

// cmpPEP440 orders two versions as packaging's _cmpkey does.
func cmpPEP440(a, b pep440Version) int {
	if c := cmpInt(a.epoch, b.epoch); c != 0 {
		return c
	}
	n := max(len(a.release), len(b.release))
	for i := 0; i < n; i++ {
		x, y := 0, 0
		if i < len(a.release) {
			x = a.release[i]
		}
		if i < len(b.release) {
			y = b.release[i]
		}
		if c := cmpInt(x, y); c != 0 {
			return c
		}
	}
	if c := cmpInt(preKey(a), preKey(b)); c != 0 {
		return c
	}
	if c := cmpInt(a.post, b.post); c != 0 { // -1 (none) sorts first
		return c
	}
	if c := cmpInt(devKey(a), devKey(b)); c != 0 {
		return c
	}
	return cmpLocal(a, b)
}

// preKey orders the pre-release segment: a dev release of a final version
// sorts before its pre-releases, a final version after them.
func preKey(v pep440Version) int {
	switch {
	case v.pre < 0 && v.post < 0 && v.dev >= 0:
		return -1 << 30
	case v.pre < 0:
		return 1 << 30
	}
	return v.preKind<<20 | v.pre
}

func devKey(v pep440Version) int {
	if v.dev < 0 {
		return 1 << 30
	}
	return v.dev
}

func cmpLocal(a, b pep440Version) int {
	switch {
	case !a.hasLocal && !b.hasLocal:
		return 0
	case !a.hasLocal:
		return -1
	case !b.hasLocal:
		return 1
	}
	for i := 0; i < len(a.local) && i < len(b.local); i++ {
		x, xNum := strconv.Atoi(a.local[i])
		y, yNum := strconv.Atoi(b.local[i])
		switch {
		case xNum == nil && yNum == nil:
			if c := cmpInt(x, y); c != 0 {
				return c
			}
		case xNum == nil:
			return 1 // a number sorts after a string
		case yNum == nil:
			return -1
		default:
			if c := strings.Compare(a.local[i], b.local[i]); c != 0 {
				return c
			}
		}
	}
	return cmpInt(len(a.local), len(b.local))
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// pep440Specifier is one clause of a specifier set, such as ">=1.2" or
// "==1.4.*".
type pep440Specifier struct {
	op       string
	version  string
	wildcard bool
}

var pep440SpecifierRe = regexp.MustCompile(`^\s*(~=|===|==|!=|<=|>=|<|>)\s*([^\s,;]+)\s*$`)

// parsePEP440Specifiers parses a comma-separated specifier set ("" is the
// empty set, which every version satisfies).
func parsePEP440Specifiers(s string) ([]pep440Specifier, bool) {
	if strings.TrimSpace(s) == "" {
		return nil, true
	}
	var specs []pep440Specifier
	for _, part := range strings.Split(s, ",") {
		m := pep440SpecifierRe.FindStringSubmatch(part)
		if m == nil {
			return nil, false
		}
		spec := pep440Specifier{op: m[1], version: m[2]}
		if spec.op == "==" || spec.op == "!=" {
			if base, ok := strings.CutSuffix(spec.version, ".*"); ok {
				spec.version, spec.wildcard = base, true
			}
		}
		if spec.op != "===" {
			if _, ok := parsePEP440(spec.version); !ok {
				return nil, false
			}
			if spec.op == "~=" && len(strings.Split(strings.SplitN(spec.version, "+", 2)[0], ".")) < 2 {
				return nil, false
			}
		}
		specs = append(specs, spec)
	}
	return specs, true
}

// specifiersAllowPrereleases reports whether a specifier set names a
// pre-release itself, which lets it match pre-releases (PEP 440).
func specifiersAllowPrereleases(specs []pep440Specifier) bool {
	for _, spec := range specs {
		if spec.op == "!=" {
			continue
		}
		if v, ok := parsePEP440(spec.version); ok && v.isPrerelease() {
			return true
		}
	}
	return false
}

// pep440Contains reports whether version satisfies every specifier, with
// pre-releases admitted as PEP 440 and pip admit them: when prereleases is
// set, or when the set names a pre-release.
func pep440Contains(specs []pep440Specifier, version string, prereleases bool) bool {
	v, ok := parsePEP440(version)
	if !ok {
		// Only arbitrary equality (===) can match a string that is no version.
		for _, spec := range specs {
			if spec.op != "===" || !spec.matches(version, v) {
				return false
			}
		}
		return len(specs) > 0
	}
	if v.isPrerelease() && !prereleases && !specifiersAllowPrereleases(specs) {
		return false
	}
	for _, spec := range specs {
		if !spec.matches(version, v) {
			return false
		}
	}
	return true
}

// matches decides one specifier clause for a version.
func (s pep440Specifier) matches(raw string, v pep440Version) bool {
	if s.op == "===" {
		return strings.EqualFold(strings.TrimSpace(raw), strings.TrimSpace(s.version))
	}
	spec, _ := parsePEP440(s.version)
	switch s.op {
	case "==":
		return s.equal(v, spec)
	case "!=":
		return !s.equal(v, spec)
	case "<=":
		return cmpPEP440(v.public(), spec) <= 0
	case ">=":
		return cmpPEP440(v.public(), spec) >= 0
	case "<":
		if cmpPEP440(v, spec) >= 0 {
			return false
		}
		// <V excludes the pre-releases of V unless V is one itself.
		return spec.isPrerelease() || !v.isPrerelease() || cmpPEP440(baseRelease(v), baseRelease(spec)) != 0
	case ">":
		if cmpPEP440(v, spec) <= 0 {
			return false
		}
		// >V excludes the post-releases of V unless V is one itself, and the
		// local versions of V.
		if spec.post < 0 && v.post >= 0 && cmpPEP440(baseRelease(v), baseRelease(spec)) == 0 {
			return false
		}
		return !v.hasLocal || cmpPEP440(baseRelease(v), baseRelease(spec)) != 0
	case "~=":
		prefix := spec.release[:len(spec.release)-1]
		if cmpPEP440(v.public(), spec) < 0 {
			return false
		}
		return pep440HasPrefix(v.release, prefix) && v.epoch == spec.epoch
	}
	return false
}

// equal decides == (and the negation of !=): a prefix match for a wildcard,
// otherwise an exact match that ignores the candidate's local label unless
// the specifier has one.
func (s pep440Specifier) equal(v, spec pep440Version) bool {
	if s.wildcard {
		return v.epoch == spec.epoch && pep440HasPrefix(v.release, spec.release)
	}
	if !spec.hasLocal {
		v = v.public()
	}
	return cmpPEP440(v, spec) == 0
}

// baseRelease is the epoch and release of v.
func baseRelease(v pep440Version) pep440Version {
	return pep440Version{epoch: v.epoch, release: v.release, pre: -1, post: -1, dev: -1}
}

// pep440HasPrefix reports whether release starts with prefix, padding the
// release with zeros.
func pep440HasPrefix(release, prefix []int) bool {
	for i, p := range prefix {
		x := 0
		if i < len(release) {
			x = release[i]
		}
		if x != p {
			return false
		}
	}
	return true
}

// pipMarkerEnv is the marker environment of an interpreter (packaging's
// default_environment), keyed by marker variable.
type pipMarkerEnv map[string]string

// value returns the environment's value of a marker variable, legacy dotted
// spellings included.
func (env pipMarkerEnv) value(variable string) (string, bool) {
	switch variable {
	case "os.name":
		variable = "os_name"
	case "sys.platform":
		variable = "sys_platform"
	case "platform.version":
		variable = "platform_version"
	case "platform.machine":
		variable = "platform_machine"
	case "platform.python_implementation", "python_implementation":
		variable = "platform_python_implementation"
	}
	value, ok := env[variable]
	return value, ok
}

// evaluate decides the marker for an environment with extra selected ("" for
// none), as packaging does. ok is false when a comparison cannot be decided
// (an unknown variable, an operand packaging would refuse).
func (m *pipMarker) evaluate(env pipMarkerEnv, extra string) (holds, ok bool) {
	switch m.op {
	case "and", "or":
		decisive := m.op == "or"
		for _, term := range m.terms {
			holds, ok := term.evaluate(env, extra)
			if !ok {
				return false, false
			}
			if holds == decisive {
				return decisive, true
			}
		}
		return !decisive, true
	}
	operand := func(o pipMarkerOperand) (string, bool) {
		if !o.variable {
			return o.value, true
		}
		if o.text == "extra" {
			return extra, true
		}
		return env.value(o.text)
	}
	lhs, okL := operand(m.lhs)
	rhs, okR := operand(m.rhs)
	if !okL || !okR {
		return false, false
	}
	if m.lhs.isExtra() || m.rhs.isExtra() {
		lhs, rhs = normalizePipName(lhs), normalizePipName(rhs)
	}
	switch m.op {
	case "in":
		return strings.Contains(rhs, lhs), true
	case "not in":
		return !strings.Contains(rhs, lhs), true
	}
	if specs, valid := parsePEP440Specifiers(m.op + rhs); valid && len(specs) == 1 {
		if _, isVersion := parsePEP440(lhs); !isVersion && specs[0].op != "===" {
			return false, false // packaging raises InvalidVersion here
		}
		return pep440Contains(specs, lhs, true), true
	}
	switch m.op {
	case "==":
		return lhs == rhs, true
	case "!=":
		return lhs != rhs, true
	case "<":
		return lhs < rhs, true
	case "<=":
		return lhs <= rhs, true
	case ">":
		return lhs > rhs, true
	case ">=":
		return lhs >= rhs, true
	}
	return false, false // ~= and === need a version
}

// pipRequirementApplies reports whether a requirement's environment marker
// holds for env (with no extra selected, as for a top-level requirement).
// known is false when it cannot be decided: no marker environment, or a
// marker makewand cannot read or evaluate.
func pipRequirementApplies(text string, env pipMarkerEnv) (applies, known bool) {
	_, marker, hasMarker := strings.Cut(text, ";")
	marker = strings.TrimSpace(marker)
	if !hasMarker || marker == "" {
		return true, true
	}
	if env == nil {
		return false, false
	}
	parsed, ok := parsePipMarker(marker)
	if !ok {
		return false, false
	}
	return parsed.evaluate(env, "")
}

// pipWheelFileRe splits a wheel file name into its name, version and tags.
var pipWheelFileRe = regexp.MustCompile(`^([^-]+)-([^-]+)(?:-[0-9][^-]*)?-([^-]+)-([^-]+)-([^-]+)\.whl$`)

// pipWheelVersion returns a wheel's project version from its file name.
func pipWheelVersion(filename string) (string, bool) {
	m := pipWheelFileRe.FindStringSubmatch(filename)
	if m == nil {
		return "", false
	}
	return m[2], true
}

// pipWheelCompatible reports whether a wheel file's tags fit the interpreter
// env describes: a pure wheel (py3/none/any), one for its Python version and
// implementation, or an abi3 wheel for an earlier CPython 3; on Linux, a
// manylinux, musllinux or linux wheel for its machine (the libc version a
// manylinux tag requires is not checked). Without an environment every wheel
// counts.
func pipWheelCompatible(filename string, env pipMarkerEnv) bool {
	m := pipWheelFileRe.FindStringSubmatch(filename)
	if m == nil {
		return false
	}
	if env == nil {
		return true
	}
	major, minor, ok := pythonMajorMinor(env["python_version"])
	if !ok {
		return true
	}
	impl := map[string]string{"cpython": "cp", "pypy": "pp"}[env["implementation_name"]]
	machine := strings.ToLower(env["platform_machine"])
	pyOK := func(tag string) bool {
		switch {
		case tag == "py"+strconv.Itoa(major), tag == "py"+strconv.Itoa(major)+strconv.Itoa(minor):
			return true
		case impl != "" && tag == impl+strconv.Itoa(major)+strconv.Itoa(minor):
			return true
		}
		return false
	}
	abiOK := func(py, abi string) bool {
		switch {
		case abi == "none":
			return pyOK(py) || isAbi3Python(py, impl, major, minor)
		case abi == "abi3":
			return isAbi3Python(py, impl, major, minor)
		}
		return impl != "" && pyOK(py) && strings.HasPrefix(abi, impl+strconv.Itoa(major)+strconv.Itoa(minor))
	}
	platOK := func(plat string) bool {
		if plat == "any" {
			return true
		}
		if env["sys_platform"] != "linux" || machine == "" {
			return false
		}
		if !strings.HasSuffix(plat, "_"+machine) {
			return false
		}
		return strings.HasPrefix(plat, "manylinux") || strings.HasPrefix(plat, "musllinux") || strings.HasPrefix(plat, "linux_")
	}
	for _, py := range strings.Split(m[3], ".") {
		for _, abi := range strings.Split(m[4], ".") {
			if !abiOK(py, abi) {
				continue
			}
			if slices.ContainsFunc(strings.Split(m[5], "."), platOK) {
				return true
			}
		}
	}
	return false
}

// isAbi3Python reports whether a python tag is a stable-ABI CPython tag the
// interpreter can load: cp3X with X at most its minor version.
func isAbi3Python(py, impl string, major, minor int) bool {
	if impl != "cp" || major != 3 {
		return false
	}
	rest, ok := strings.CutPrefix(py, "cp3")
	if !ok {
		return false
	}
	x, err := strconv.Atoi(rest)
	return err == nil && x <= minor
}

// pythonMajorMinor parses a python_version marker value ("3.12").
func pythonMajorMinor(s string) (int, int, bool) {
	a, b, ok := strings.Cut(s, ".")
	if !ok {
		return 0, 0, false
	}
	major, err1 := strconv.Atoi(a)
	minor, err2 := strconv.Atoi(b)
	return major, minor, err1 == nil && err2 == nil
}
