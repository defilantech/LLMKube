package grounding

import (
	"regexp"
	"strings"
)

// Claim is one empirical assertion detected in an added docs line: a
// number carrying a measurement unit, plus the subject tokens it is
// attributed to. Detection is lexical; it identifies claims that require
// a source, it does not judge truth (proposal 1075, section 4.1).
type Claim struct {
	File     string
	Line     int
	Text     string
	Number   string   // thousands separators stripped, decimal point preserved ("1200", "2.5")
	Unit     string   // normalized unit ("tok/s")
	Subjects []string // capitalized identifier-ish tokens from the line
}

// claimNumberRE matches a number immediately followed by a measurement
// unit. Bare numbers, versions, and counts do not match: the unit is what
// makes a number an empirical claim.
var claimNumberRE = regexp.MustCompile(
	`[~≈]?(\d+(?:[.,]\d+)*)\s*(tok/s|t/s|tokens/s|ms\b|GiB|GB\b|MiB|MB\b|W\b|%)`)

// subjectRE captures model-name-ish tokens: a capitalized run of letters,
// digits, dots, and hyphens at least three characters long.
var subjectRE = regexp.MustCompile(`\b[A-Z][A-Za-z0-9.-]{2,}\b`)

func isDocsFile(path string) bool {
	return strings.HasSuffix(path, ".md") ||
		strings.HasPrefix(path, "docs/") ||
		strings.HasPrefix(path, "examples/")
}

// DetectEmpiricalClaims scans added docs lines for number+unit claims. One
// Claim is emitted per number+unit match, not per line, so every number on
// a benchmark row is independently validated; a coder citing a source for
// the first column can no longer fabricate the remaining columns unchecked.
func DetectEmpiricalClaims(added []AddedLine) []Claim {
	var out []Claim
	for _, al := range added {
		if !isDocsFile(al.File) {
			continue
		}
		subjects := subjectRE.FindAllString(al.Text, -1)
		for _, m := range claimNumberRE.FindAllStringSubmatch(al.Text, -1) {
			out = append(out, Claim{
				File: al.File,
				Line: al.Line,
				Text: al.Text,
				// Strip thousands separators only, never the decimal
				// point: stripping the decimal point makes 9.9 and 99
				// collide, which would let a fabricated decimal validate
				// against an unrelated whole number.
				Number:   strings.ReplaceAll(m[1], ",", ""),
				Unit:     normalizeUnit(m[2]),
				Subjects: subjects,
			})
		}
	}
	return out
}

func normalizeUnit(u string) string {
	switch u {
	case "t/s", "tokens/s":
		return "tok/s"
	default:
		return u
	}
}
