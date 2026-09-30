package nugetmeta

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

const SchemaVersion = 1
const MaxManifestBytes = 1 << 20

type Dependency struct {
	ID    string `xml:"id,attr" json:"id"`
	Range string `xml:"version,attr" json:"range,omitempty"`
}
type DependencyGroup struct {
	TargetFramework string       `xml:"targetFramework,attr" json:"targetFramework,omitempty"`
	Dependencies    []Dependency `xml:"dependency" json:"dependencies,omitempty"`
}
type Metadata struct {
	Schema            int               `json:"schema"`
	ID                string            `json:"id"`
	Version           string            `json:"version"`
	OriginalVersion   string            `json:"originalVersion"`
	Key               string            `json:"key"`
	Prerelease        bool              `json:"prerelease"`
	SemVer2           bool              `json:"semVer2"`
	Listed            bool              `json:"listed"`
	Description       string            `json:"description,omitempty"`
	Authors           string            `json:"authors,omitempty"`
	Title             string            `json:"title,omitempty"`
	Summary           string            `json:"summary,omitempty"`
	Tags              string            `json:"tags,omitempty"`
	LicenseURL        string            `json:"licenseUrl,omitempty"`
	LicenseExpression string            `json:"licenseExpression,omitempty"`
	ProjectURL        string            `json:"projectUrl,omitempty"`
	IconURL           string            `json:"iconUrl,omitempty"`
	DependencyGroups  []DependencyGroup `json:"dependencyGroups,omitempty"`
}

type manifest struct {
	XMLName  xml.Name `xml:"package"`
	Metadata struct {
		ID          string `xml:"id"`
		Version     string `xml:"version"`
		Description string `xml:"description"`
		Authors     string `xml:"authors"`
		Title       string `xml:"title"`
		Summary     string `xml:"summary"`
		Tags        string `xml:"tags"`
		LicenseURL  string `xml:"licenseUrl"`
		ProjectURL  string `xml:"projectUrl"`
		IconURL     string `xml:"iconUrl"`
		License     struct {
			Type  string `xml:"type,attr"`
			Value string `xml:",chardata"`
		} `xml:"license"`
		Dependencies struct {
			Direct []Dependency      `xml:"dependency"`
			Groups []DependencyGroup `xml:"group"`
		} `xml:"dependencies"`
	} `xml:"metadata"`
}

// Read extracts the one root manifest with a strict decompression bound. No filename fallback:
// a malformed archive must never become searchable under invented coordinates.
func Read(r io.ReaderAt, size int64) (Metadata, error) {
	var out Metadata
	zr, e := zip.NewReader(r, size)
	if e != nil {
		return out, fmt.Errorf("invalid nupkg: %w", e)
	}
	var mf *zip.File
	for _, f := range zr.File {
		if !strings.Contains(f.Name, "/") && strings.HasSuffix(strings.ToLower(f.Name), ".nuspec") {
			if mf != nil {
				return out, fmt.Errorf("multiple root nuspec files")
			}
			mf = f
		}
	}
	if mf == nil {
		return out, fmt.Errorf("missing root nuspec")
	}
	if mf.UncompressedSize64 > MaxManifestBytes {
		return out, fmt.Errorf("nuspec too large")
	}
	rc, e := mf.Open()
	if e != nil {
		return out, e
	}
	defer rc.Close()
	b, e := io.ReadAll(io.LimitReader(rc, MaxManifestBytes+1))
	if e != nil {
		return out, e
	}
	if len(b) > MaxManifestBytes {
		return out, fmt.Errorf("nuspec too large")
	}
	var m manifest
	if e = xml.Unmarshal(b, &m); e != nil {
		return out, e
	}
	x := m.Metadata
	x.ID = strings.TrimSpace(x.ID)
	x.Version = strings.TrimSpace(x.Version)
	if !ValidID(x.ID) {
		return out, fmt.Errorf("invalid package id")
	}
	v, e := ParseVersion(x.Version)
	if e != nil {
		return out, e
	}
	out = Metadata{Schema: SchemaVersion, ID: strings.ToLower(x.ID), Version: v.Normalized(), OriginalVersion: x.Version, Key: v.Key(), Prerelease: v.Release != "", SemVer2: v.SemVer2(), Listed: true,
		Description: x.Description, Authors: x.Authors, Title: x.Title, Summary: x.Summary, Tags: x.Tags, LicenseURL: x.LicenseURL, ProjectURL: x.ProjectURL, IconURL: x.IconURL, DependencyGroups: x.Dependencies.Groups}
	if x.License.Type == "expression" {
		out.LicenseExpression = strings.TrimSpace(x.License.Value)
	}
	if len(x.Dependencies.Direct) > 0 {
		out.DependencyGroups = append([]DependencyGroup{{Dependencies: x.Dependencies.Direct}}, out.DependencyGroups...)
	}
	for _, g := range out.DependencyGroups {
		for _, d := range g.Dependencies {
			if !ValidID(d.ID) {
				return Metadata{}, fmt.Errorf("invalid dependency id")
			}
			s, e := RangeSemVer2(d.Range)
			if e != nil {
				return Metadata{}, e
			}
			out.SemVer2 = out.SemVer2 || s
		}
	}
	return out, nil
}
