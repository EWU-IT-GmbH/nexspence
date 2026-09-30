package nugetmeta

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

func TestNuGetVersion(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		want int
	}{{"1.9.0", "1.10.0", -1}, {"1.0.0.1", "1.0.0", 1}, {"1.0.0-alpha.2", "1.0.0-alpha.10", -1}, {"1.0.0-alpha", "1.0.0", -1}, {"1.0+foo", "1.0.0+bar", 0}, {"01.2.0.0", "1.2", 0}, {"1.0-BETA", "1.0-beta", 0}} {
		a, e := ParseVersion(tt.a)
		if e != nil {
			t.Fatal(e)
		}
		b, e := ParseVersion(tt.b)
		if e != nil {
			t.Fatal(e)
		}
		if got := a.Compare(b); got != tt.want {
			t.Errorf("%s vs %s = %d", tt.a, tt.b, got)
		}
	}
	for _, s := range []string{"", "-1", "1.2.3.4.5", "1.0.0+", "1.0.0-a..b", "1.0.0-a/evil", "2147483648.0.0", "1.0.0-a+one+two"} {
		if _, e := ParseVersion(s); e == nil {
			t.Errorf("accepted %q", s)
		}
	}
	v, _ := ParseVersion("01.02.003.0-ALPHA+meta")
	if v.Key() != "1.2.3-alpha" || v.Normalized() != "1.2.3-ALPHA+meta" {
		t.Fatal(v)
	}
}
func TestRangeAndLevel(t *testing.T) {
	for _, s := range []string{"1.0.0+a", "[1.0.0-alpha.1,2.0)", "(,2.0.0+meta]", "1.0.0-alpha.*"} {
		v, e := RangeSemVer2(s)
		if e != nil || !v {
			t.Fatalf("%s: %v %v", s, v, e)
		}
	}
	for _, s := range []string{"1.0", "[1,2)", "[2.0]", "1.2.*"} {
		v, e := RangeSemVer2(s)
		if e != nil || v {
			t.Fatalf("%s: %v %v", s, v, e)
		}
	}
	for _, s := range []string{"(,)", "[1,1)", "1.*.2", "[1,2,3]", "[3,1]", "(1)", "[1"} {
		if _, e := RangeSemVer2(s); e == nil {
			t.Errorf("accepted %s", s)
		}
	}
	for _, s := range []string{"2.0.0", "3.0.0"} {
		v, e := SemVerLevel(s)
		if e != nil || !v {
			t.Fatal(s)
		}
	}
	for _, s := range []string{"1.0.0", "2.0.0-rc"} {
		v, e := SemVerLevel(s)
		if e != nil || v {
			t.Fatal(s)
		}
	}
	for _, s := range []string{"2", "2.0.0.0", "2.0.0+a", "2.0.0-a.b", "02.0.0"} {
		if _, e := SemVerLevel(s); e == nil {
			t.Fatal(s)
		}
	}
}
func packageBytes(t *testing.T, manifest string) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	f, e := z.Create("sample.nuspec")
	if e != nil {
		t.Fatal(e)
	}
	_, e = f.Write([]byte(manifest))
	if e != nil {
		t.Fatal(e)
	}
	if e = z.Close(); e != nil {
		t.Fatal(e)
	}
	return b.Bytes()
}
func TestMetadata(t *testing.T) {
	b := packageBytes(t, `<package xmlns="http://schemas.microsoft.com/packaging/2013/05/nuspec.xsd"><metadata><id>Example.Lib</id><version>1.02.3</version><description>Example</description><dependencies><group targetFramework="net8.0"><dependency id="Other" version="[1.0.0-alpha.1,2.0)"/></group></dependencies></metadata></package>`)
	m, e := Read(bytes.NewReader(b), int64(len(b)))
	if e != nil {
		t.Fatal(e)
	}
	if m.ID != "example.lib" || m.Version != "1.2.3" || !m.SemVer2 || !m.Listed || m.Prerelease {
		t.Fatalf("%+v", m)
	}
	if len(m.DependencyGroups) != 1 || len(m.DependencyGroups[0].Dependencies) != 1 {
		t.Fatal(m)
	}
	for _, manifest := range []string{`<package><metadata><id>../bad</id><version>1</version></metadata></package>`, `<package><metadata><id>ok</id><version>bad</version></metadata></package>`, strings.Repeat("a", MaxManifestBytes+1)} {
		b = packageBytes(t, manifest)
		if _, e = Read(bytes.NewReader(b), int64(len(b))); e == nil {
			t.Fatal("invalid manifest accepted")
		}
	}
	if _, e = Read(bytes.NewReader([]byte("fake")), 4); e == nil {
		t.Fatal("not a ZIP")
	}
}

func TestPackageIdentityCannotChangeURLPath(t *testing.T) {
	for _, s := range []string{".", "..", "../private", "a/b", "a%2fb", "a\\b", "a?query"} {
		if ValidID(s) {
			t.Errorf("unsafe id accepted: %s", s)
		}
	}
	for _, s := range []string{"example.library", "a_b", "a-b", "a..b"} {
		if !ValidID(s) {
			t.Errorf("valid id refused: %s", s)
		}
	}
}
