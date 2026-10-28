// Package rail holds the rail sources that are not OASA: the STASY (metro, tram) static GTFS
// and the Proastiakos boards of railway.gov.gr. Both live outside the OASA feed, so they keep
// their own store and caches here.
package rail

import (
	"fmt"
	"strings"
)

// UserAgent is sent upstream. railway.gov.gr and the CKAN catalogue are public services; a
// named client is how they can identity (and throttle) us.
const UserAgent = "athens-transit-rt/1.0 (+https://github.com/angelospk/athens-transit-rt)"

// foldRunes maps the accented Greek letters and the final sigma to their plain forms. Names
// come with and without accents depending on the source, and the app sends what the user typed.
var foldRunes = map[rune]rune{
	'ά': 'α', 'έ': 'ε', 'ή': 'η', 'ί': 'ι', 'ϊ': 'ι', 'ΐ': 'ι',
	'ό': 'ο', 'ύ': 'υ', 'ϋ': 'υ', 'ΰ': 'υ', 'ώ': 'ω', 'ς': 'σ',
}

// Fold normalises a Greek name for matching: lower case, no accents or diaeresis, final sigma
// to sigma, single spaces. "ΣΥΝΤΑΓΜΑ" and "Σύνταγμα" fold to the same string.
func Fold(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if u, ok := foldRunes[r]; ok {
			r = u
		}
		b.WriteRune(r)
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// HHMM formats seconds after midnight as GTFS shows them: "05:30", and "25:10" past midnight.
func HHMM(secs int32) string {
	return fmt.Sprintf("%02d:%02d", secs/3600, secs/60%60)
}
