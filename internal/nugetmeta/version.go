// Package nugetmeta implements NuGet package identity and validated nuspec metadata.
package nugetmeta

import (
	"fmt"
	"strconv"
	"strings"
)

// Version implements NuGet's 1-4 component version and SemVer precedence rules.
// Key excludes build metadata and folds prerelease case, as NuGet identity does.
type Version struct {
	Numbers [4]uint64
	Release string
	Build   string
}

func ParseVersion(s string) (Version, error) {
	var v Version
	bad := func() (Version, error) { return Version{}, fmt.Errorf("invalid NuGet version %q", s) }
	if s == "" || len(s) > 256 || strings.TrimSpace(s) != s {
		return bad()
	}
	core := s
	if i := strings.IndexByte(core, '+'); i >= 0 {
		v.Build = core[i+1:]
		core = core[:i]
		if !identifiers(v.Build, false) {
			return bad()
		}
	}
	if i := strings.IndexByte(core, '-'); i >= 0 {
		v.Release = core[i+1:]
		core = core[:i]
		if !identifiers(v.Release, true) {
			return bad()
		}
	}
	ns := strings.Split(core, ".")
	if len(ns) < 1 || len(ns) > 4 {
		return bad()
	}
	for i, n := range ns {
		if !digits(n) {
			return bad()
		}
		x, e := strconv.ParseUint(n, 10, 31)
		if e != nil {
			return bad()
		}
		v.Numbers[i] = x
	}
	return v, nil
}
func digits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
func identifiers(s string, prerelease bool) bool {
	if s == "" {
		return false
	}
	for _, p := range strings.Split(s, ".") {
		if p == "" {
			return false
		}
		for _, c := range p {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
		if prerelease && digits(p) && len(p) > 1 && p[0] == '0' {
			return false
		}
	}
	return true
}
func (v Version) Normalized() string {
	s := fmt.Sprintf("%d.%d.%d", v.Numbers[0], v.Numbers[1], v.Numbers[2])
	if v.Numbers[3] != 0 {
		s += fmt.Sprintf(".%d", v.Numbers[3])
	}
	if v.Release != "" {
		s += "-" + v.Release
	}
	if v.Build != "" {
		s += "+" + v.Build
	}
	return s
}
func (v Version) Key() string   { v.Build = ""; return strings.ToLower(v.Normalized()) }
func (v Version) SemVer2() bool { return v.Build != "" || strings.Contains(v.Release, ".") }
func (v Version) Compare(w Version) int {
	for i := range v.Numbers {
		if v.Numbers[i] < w.Numbers[i] {
			return -1
		}
		if v.Numbers[i] > w.Numbers[i] {
			return 1
		}
	}
	if v.Release == w.Release {
		return 0
	}
	if v.Release == "" {
		return 1
	}
	if w.Release == "" {
		return -1
	}
	a, b := strings.Split(strings.ToLower(v.Release), "."), strings.Split(strings.ToLower(w.Release), ".")
	for i := 0; i < len(a) && i < len(b); i++ {
		x, y := a[i], b[i]
		if x == y {
			continue
		}
		xn, yn := digits(x), digits(y)
		if xn && yn {
			if len(x) < len(y) {
				return -1
			}
			if len(x) > len(y) {
				return 1
			}
		} else if xn {
			return -1
		} else if yn {
			return 1
		}
		return strings.Compare(x, y)
	}
	if len(a) < len(b) {
		return -1
	}
	if len(a) > len(b) {
		return 1
	}
	return 0
}
func ValidID(s string) bool {
	if len(s) < 1 || len(s) > 100 || s == "." || s == ".." {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// SemVerLevel accepts a SemVer 1 capability string, not a NuGet four-part version.
func SemVerLevel(s string) (bool, error) {
	v, err := ParseVersion(s)
	if err != nil {
		return false, err
	}
	core := strings.SplitN(s, "-", 2)[0]
	if v.Build != "" || strings.Count(core, ".") != 2 || strings.Contains(v.Release, ".") {
		return false, fmt.Errorf("invalid semVerLevel")
	}
	for _, n := range strings.Split(core, ".") {
		if len(n) > 1 && n[0] == '0' {
			return false, fmt.Errorf("invalid semVerLevel")
		}
	}
	return v.Compare(Version{Numbers: [4]uint64{2, 0, 0, 0}}) >= 0, nil
}

// RangeSemVer2 validates a nuspec dependency range and classifies its endpoints.
func RangeSemVer2(s string) (bool, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return false, nil
	}
	parts := []string{s}
	if s[0] == '[' || s[0] == '(' {
		if len(s) < 2 || (s[len(s)-1] != ']' && s[len(s)-1] != ')') {
			return false, fmt.Errorf("invalid dependency range")
		}
		parts = strings.Split(s[1:len(s)-1], ",")
		if len(parts) > 2 {
			return false, fmt.Errorf("invalid dependency range")
		}
		if len(parts) == 1 && (s[0] != '[' || s[len(s)-1] != ']' || parts[0] == "") {
			return false, fmt.Errorf("invalid exact dependency range")
		}
	}
	sem2 := false
	var bounds []Version
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		var v Version
		var e error
		if strings.Contains(p, "*") {
			if i > 0 {
				return false, fmt.Errorf("floating upper bound")
			}
			v, e = floatingMinimum(p)
		} else {
			v, e = ParseVersion(p)
		}
		if e != nil {
			return false, e
		}
		sem2 = sem2 || v.SemVer2()
		bounds = append(bounds, v)
	}
	if len(bounds) == 0 {
		return false, fmt.Errorf("empty dependency range")
	}
	if len(bounds) == 2 {
		cmp := bounds[0].Compare(bounds[1])
		if cmp > 0 || cmp == 0 && (s[0] != '[' || s[len(s)-1] != ']') {
			return false, fmt.Errorf("empty or inverted dependency range")
		}
	}
	return sem2, nil
}

// floatingMinimum follows NuGet VersionRange's floating lower bound. Wildcards
// can end the numeric components or a prerelease prefix, never a build suffix.
func floatingMinimum(s string) (Version, error) {
	bad := func() (Version, error) { return Version{}, fmt.Errorf("invalid floating dependency version") }
	if strings.Contains(s, "+") {
		return bad()
	}
	core, release, hasRelease := strings.Cut(s, "-")
	if strings.Contains(core, "*") {
		ns := strings.Split(core, ".")
		if len(ns) > 4 || ns[len(ns)-1] != "*" {
			return bad()
		}
		for _, n := range ns[:len(ns)-1] {
			if !digits(n) {
				return bad()
			}
		}
		ns[len(ns)-1] = "0"
		core = strings.Join(ns, ".")
	}
	if hasRelease {
		if !strings.HasSuffix(release, "*") || strings.Count(release, "*") != 1 {
			return bad()
		}
		release = strings.TrimSuffix(release, "*")
		if release == "" || strings.HasSuffix(release, ".") {
			release += "0"
		}
		core += "-" + release
	}
	return ParseVersion(core)
}
