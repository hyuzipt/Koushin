package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/5rahim/habari"
)

var (
	reParensYear = regexp.MustCompile(`\s*\((19|20)\d{2}\)`)
	reEpTail     = regexp.MustCompile(`\s*[-–—]\s*(?:ep|episode)?\s*\d{1,4}(?:v\d+)?\s*$`)
	reBrackets   = regexp.MustCompile(`\s*[\[\(][^\]\)]*[\]\)]`)
	reMultiSpace = regexp.MustCompile(`\s{2,}`)
	reAnyYear    = regexp.MustCompile(`\b(19|20)\d{2}\b`)

	reSxxEyy      = regexp.MustCompile(`(?i)\bS(\d{1,2})[ ._-]*E(\d{1,3})\b`)
	reSxxEyyLoose = regexp.MustCompile(`(?i)\bS\d{1,2}[ ._-]*E\d{1,3}\b`)

	reSeasonSNum = regexp.MustCompile(`(?i)\bS(\d{1,2})\b`)
	reSeasonWord = regexp.MustCompile(`(?i)\b(?:season|cour)\s*(\d{1,2})\b`)
	reSeasonOrd  = regexp.MustCompile(`(?i)\b(1st|2nd|3rd|[4-9]th)\s+season\b`)

	reTrailingCRC = regexp.MustCompile(`\s*\[[0-9a-fA-F]{6,}\]\s*$`)
)

var zeroBasedRelease = struct {
	mu    sync.Mutex
	bySig map[string]bool
}{bySig: make(map[string]bool)}

func zeroBasedStorePath() string { return filepath.Join(appDataDir(), "zero_based_releases.json") }

func loadZeroBasedSignatures() {
	b, err := os.ReadFile(zeroBasedStorePath())
	if err != nil {
		return
	}
	var tmp struct {
		Sigs []string `json:"sigs"`
	}
	if json.Unmarshal(b, &tmp) != nil {
		return
	}
	zeroBasedRelease.mu.Lock()
	for _, s := range tmp.Sigs {
		s = strings.TrimSpace(strings.ToLower(s))
		if s != "" {
			zeroBasedRelease.bySig[s] = true
		}
	}
	zeroBasedRelease.mu.Unlock()
}

func saveZeroBasedSignaturesLocked() {

	sigs := make([]string, 0, len(zeroBasedRelease.bySig))
	for s := range zeroBasedRelease.bySig {
		sigs = append(sigs, s)
	}
	sort.Strings(sigs)
	b, _ := json.MarshalIndent(struct {
		Sigs []string `json:"sigs"`
	}{Sigs: sigs}, "", "  ")
	_ = os.WriteFile(zeroBasedStorePath(), b, 0600)
}

func releaseSignatureForZeroBased(nameOrPath string) (sig string, hasEpisodeToken bool) {
	base := filepath.Base(strings.TrimSpace(nameOrPath))
	if i := strings.LastIndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	base = strings.TrimSpace(base)
	if base == "" {
		return "", false
	}

	base = reTrailingCRC.ReplaceAllString(base, "")
	base = strings.TrimSpace(base)
	if base == "" {
		return "", false
	}

	validDigitsAllowZero := func(digits string, allow4Digits bool) bool {
		digits = strings.TrimSpace(digits)
		if digits == "" {
			return false
		}

		if strings.TrimLeft(digits, "0") == "" {
			return true
		}
		return normalizeEpisodeCandidate(digits, allow4Digits) != ""
	}

	hasEpisodeToken = reSxxEyy.MatchString(base)
	if hasEpisodeToken {
		base = reSxxEyy.ReplaceAllStringFunc(base, func(m string) string {
			sm := reSxxEyy.FindStringSubmatch(m)
			if len(sm) == 3 {
				return "S" + sm[1] + "E__"
			}
			return "S__E__"
		})

		base = reMultiSpace.ReplaceAllString(base, " ")
		base = strings.TrimSpace(base)
		if base == "" {
			return "", false
		}
		return strings.ToLower(base), true
	}

	if idx := reEyyOnly.FindStringSubmatchIndex(base); len(idx) == 4 {
		digits := base[idx[2]:idx[3]]
		if validDigitsAllowZero(digits, false) {
			base = base[:idx[2]] + "__" + base[idx[3]:]
			base = reMultiSpace.ReplaceAllString(base, " ")
			base = strings.TrimSpace(base)
			if base == "" {
				return "", false
			}
			return strings.ToLower(base), true
		}
	}

	if ms := reEGeneric.FindAllStringSubmatchIndex(base, -1); len(ms) > 0 {
		for _, idx := range ms {
			if len(idx) != 4 {
				continue
			}
			digits := base[idx[2]:idx[3]]
			if !validDigitsAllowZero(digits, true) {
				continue
			}
			base2 := base[:idx[2]] + "__" + base[idx[3]:]
			base2 = reMultiSpace.ReplaceAllString(base2, " ")
			base2 = strings.TrimSpace(base2)
			if base2 == "" {
				continue
			}
			return strings.ToLower(base2), true
		}
	}

	if ms := reDashEp.FindAllStringSubmatchIndex(base, -1); len(ms) > 0 {
		for _, idx := range ms {
			if len(idx) != 4 {
				continue
			}
			digits := base[idx[2]:idx[3]]
			if !validDigitsAllowZero(digits, false) {
				continue
			}
			base2 := base[:idx[2]] + "__" + base[idx[3]:]
			base2 = reMultiSpace.ReplaceAllString(base2, " ")
			base2 = strings.TrimSpace(base2)
			if base2 == "" {
				continue
			}
			return strings.ToLower(base2), true
		}
	}

	if idx := reTrailingNum.FindStringSubmatchIndex(base); len(idx) == 4 {
		digits := base[idx[2]:idx[3]]
		if !validDigitsAllowZero(digits, false) {
			return "", false
		}

		base = base[:idx[2]] + "__" + base[idx[3]:]
		base = reMultiSpace.ReplaceAllString(base, " ")
		base = strings.TrimSpace(base)
		if base == "" {
			return "", false
		}
		return strings.ToLower(base), true
	}

	return "", false
}

func markZeroBasedSignature(sig string) {
	if strings.TrimSpace(sig) == "" {
		return
	}
	zeroBasedRelease.mu.Lock()
	if !zeroBasedRelease.bySig[sig] {
		zeroBasedRelease.bySig[sig] = true
		saveZeroBasedSignaturesLocked()
	}
	zeroBasedRelease.mu.Unlock()
}

func isZeroBasedSignature(sig string) bool {
	zeroBasedRelease.mu.Lock()
	on := zeroBasedRelease.bySig[sig]
	zeroBasedRelease.mu.Unlock()
	return on
}

func dirContainsEpisodeZeroForSignature(filePath string, sig string) bool {
	if strings.TrimSpace(filePath) == "" || strings.TrimSpace(sig) == "" {
		return false
	}
	dir := filepath.Dir(filePath)
	if dir == "" || dir == "." {
		return false
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	limit := 600
	if len(ents) < limit {
		limit = len(ents)
	}
	for i := 0; i < limit; i++ {
		e := ents[i]
		if e.IsDir() {
			continue
		}
		name := e.Name()
		lsig, ok := releaseSignatureForZeroBased(name)
		if !ok || lsig != sig {
			continue
		}

		base := filepath.Base(strings.TrimSpace(name))
		if j := strings.LastIndexByte(base, '.'); j >= 0 {
			base = base[:j]
		}
		base = strings.TrimSpace(reTrailingCRC.ReplaceAllString(base, ""))
		if base == "" {
			continue
		}
		if m := reSxxEyy.FindStringSubmatch(base); len(m) == 3 {
			d := strings.TrimSpace(m[2])
			if strings.TrimLeft(d, "0") == "" {
				return true
			}
			continue
		}
		if m := reEyyOnly.FindStringSubmatch(base); len(m) == 2 {
			d := strings.TrimSpace(m[1])
			if strings.TrimLeft(d, "0") == "" {
				return true
			}
		}
		if ms := reEGeneric.FindAllStringSubmatch(base, -1); len(ms) > 0 {
			for _, m := range ms {
				if len(m) == 2 {
					d := strings.TrimSpace(m[1])
					if strings.TrimLeft(d, "0") == "" {
						return true
					}
				}
			}
		}
		if ms := reDashEp.FindAllStringSubmatch(base, -1); len(ms) > 0 {
			for _, m := range ms {
				if len(m) == 2 {
					d := strings.TrimSpace(m[1])
					if strings.TrimLeft(d, "0") == "" {
						return true
					}
				}
			}
		}
		if m := reTrailingNum.FindStringSubmatch(base); len(m) == 2 {
			d := strings.TrimSpace(m[1])
			if strings.TrimLeft(d, "0") == "" {
				return true
			}
		}
	}
	return false
}

func applyZeroBasedEpisodeFix(filePath string, key string, ep string) string {
	ep = strings.TrimSpace(ep)
	if ep == "" {
		return ep
	}
	n, err := strconv.Atoi(ep)
	if err != nil {
		return ep
	}
	sig, ok := releaseSignatureForZeroBased(key)
	if !ok || sig == "" {
		sig, ok = releaseSignatureForZeroBased(filePath)
	}
	if n == 0 {
		if ok && sig != "" {
			markZeroBasedSignature(sig)
		} else {
			if fsig, fok := releaseSignatureForZeroBased(filePath); fok && fsig != "" {
				markZeroBasedSignature(fsig)
			}
		}
		return "1"
	}
	if !ok || sig == "" {
		return ep
	}
	if isZeroBasedSignature(sig) || dirContainsEpisodeZeroForSignature(filePath, sig) {
		markZeroBasedSignature(sig)
		return strconv.Itoa(n + 1)
	}
	return ep
}

func parseSeasonFromString(s string) int {
	if s == "" {
		return 0
	}
	if m := reSxxEyy.FindStringSubmatch(s); len(m) == 3 {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			return n
		}
	}

	if m := reSeasonSNum.FindStringSubmatch(s); len(m) == 2 {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			return n
		}
	}
	if m := reSeasonWord.FindStringSubmatch(s); len(m) == 2 {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			return n
		}
	}
	if m := reSeasonOrd.FindStringSubmatch(s); len(m) == 2 {
		s := strings.ToLower(m[1])
		switch s {
		case "1st":
			return 1
		case "2nd":
			return 2
		case "3rd":
			return 3
		default:
			digits := s[:len(s)-2]
			if n, err := strconv.Atoi(digits); err == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}

func wantSeasonFrom(sources ...string) int {
	for _, s := range sources {
		if n := parseSeasonFromString(s); n > 0 {
			return n
		}
	}
	return 0
}

func parseYearString(s string) int {
	if s == "" {
		return 0
	}
	if m := reAnyYear.FindString(s); m != "" {
		if y, err := strconv.Atoi(m); err == nil {
			return y
		}
	}
	return 0
}

func wantYearFrom(md *habari.Metadata, key, mediaTitle string) int {
	if y, err := strconv.Atoi(strings.TrimSpace(md.Year)); err == nil && y > 1900 {
		return y
	}
	if y := parseYearString(key); y > 0 {
		return y
	}
	if y := parseYearString(mediaTitle); y > 0 {
		return y
	}
	return 0
}

func cleanTitleForSearch(s string) string {
	s = strings.TrimSpace(s)

	if ext := strings.ToLower(filepath.Ext(s)); ext != "" {
		switch ext {
		case ".mkv", ".mp4", ".avi", ".mov", ".webm", ".m4v":
			s = strings.TrimSuffix(s, filepath.Ext(s))
		}
	}

	s = reBrackets.ReplaceAllString(s, "")
	s = reParensYear.ReplaceAllString(s, "")
	s = reAnyYear.ReplaceAllString(s, "")
	s = reSxxEyyLoose.ReplaceAllString(s, "")
	s = reEpTail.ReplaceAllString(s, "")

	s = strings.ReplaceAll(s, "_", " ")
	s = strings.ReplaceAll(s, ".", " ")

	s = reSeasonSNum.ReplaceAllString(s, "")
	s = reSeasonWord.ReplaceAllString(s, "")

	s = strings.Trim(s, " -–—")
	s = reMultiSpace.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

func seriesKeyForOverride(fileKey, parsedTitle, mediaTitle string, wantYear, wantSeason int) string {
	base := firstNonEmpty(parsedTitle, mediaTitle, fileKey)
	base = cleanTitleForSearch(base)
	base = strings.ToLower(strings.TrimSpace(base))
	if base == "" {
		return ""
	}
	key := base
	if wantYear > 0 {
		key += fmt.Sprintf("#y%d", wantYear)
	}
	if wantSeason > 0 {
		key += fmt.Sprintf("#s%d", wantSeason)
	}
	return key
}

func normalizeForTitleMatch(s string) string {
	s = strings.ToLower(strings.TrimSpace(cleanTitleForSearch(s)))
	if s == "" {
		return ""
	}
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= '0' && r <= '9':
			return r
		case r == ' ':
			return r
		default:
			return ' '
		}
	}, s)
	s = reMultiSpace.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

func tokenSetForTitleMatch(s string) map[string]bool {
	s = normalizeForTitleMatch(s)
	out := make(map[string]bool)
	for _, w := range strings.Fields(s) {
		w = strings.TrimSpace(w)
		if len(w) < 2 {
			continue
		}
		out[w] = true
	}
	return out
}

func overlapCount(a, b map[string]bool) int {
	n := 0
	for k := range a {
		if b[k] {
			n++
		}
	}
	return n
}

var (
	reEGeneric    = regexp.MustCompile(`(?i)\b(?:ep|eps|episode)\s*[-_. ]*\s*(\d{1,4})\b`)
	reDashEp      = regexp.MustCompile(`(?i)(?:^|\s)[-–—]\s*(\d{1,3})(?:v\d+)?\b`)
	reEyyOnly     = regexp.MustCompile(`(?i)\bE(\d{1,3})\b`)
	reTrailingNum = regexp.MustCompile(`(?:^|[\s._-])(\d{1,3})(?:v\d+)?\s*$`)
)

func pickEpisode(md *habari.Metadata, fallbackTitle string) (titleOut string, ep string) {
	titleOut = firstNonEmpty(md.FormattedTitle, md.Title, fallbackTitle)
	for _, arr := range [][]string{md.EpisodeNumber, md.EpisodeNumberAlt, md.OtherEpisodeNumber} {
		if len(arr) > 0 && strings.TrimSpace(arr[0]) != "" {
			raw := strings.TrimSpace(arr[0])
			if i := strings.IndexByte(raw, 'v'); i > 0 {
				raw = raw[:i]
			}
			ep = strings.TrimLeft(raw, "0")
			if ep == "" {
				ep = "0"
			}
			return
		}
	}
	if ep == "" {
		ep = guessEpisodeFromString(md.FileName)
	}
	if ep == "" {
		ep = guessEpisodeFromString(fallbackTitle)
	}
	return
}

func normalizeEpisodeCandidate(numStr string, allow4Digits bool) string {
	numStr = strings.TrimSpace(numStr)
	if numStr == "" {
		return ""
	}
	n, err := strconv.Atoi(numStr)
	if err != nil || n <= 0 {
		return ""
	}
	if n >= 1900 && n <= 2099 {
		return ""
	}
	switch n {
	case 2160, 1440, 1080, 720, 576, 480, 4320:
		return ""
	case 264, 265:
		return ""
	}
	if !allow4Digits && n >= 1000 {
		return ""
	}
	return strconv.Itoa(n)
}

func guessEpisodeFromString(s string) string {
	if s == "" {
		return ""
	}
	if ms := reEGeneric.FindAllStringSubmatch(s, -1); len(ms) > 0 {
		for _, m := range ms {
			if len(m) == 2 {
				if ep := normalizeEpisodeCandidate(m[1], true); ep != "" {
					return ep
				}
			}
		}
	}
	if m := reSxxEyy.FindStringSubmatch(s); len(m) == 3 {
		epDigits := strings.TrimSpace(m[2])
		if strings.TrimLeft(epDigits, "0") == "" {
			return "0"
		}
		if ep := normalizeEpisodeCandidate(epDigits, true); ep != "" {
			return ep
		}
	}
	if ms := reDashEp.FindAllStringSubmatch(s, -1); len(ms) > 0 {
		for _, m := range ms {
			if len(m) == 2 {
				if ep := normalizeEpisodeCandidate(m[1], false); ep != "" {
					return ep
				}
			}
		}
	}
	if m := reTrailingNum.FindStringSubmatch(s); len(m) == 2 {
		if ep := normalizeEpisodeCandidate(m[1], false); ep != "" {
			return ep
		}
	}
	return ""
}

func fallbackTitleFrom(st mpvState) string {
	if t := strings.TrimSpace(st.MediaTitle); t != "" {
		return t
	}
	base := filepath.Base(strings.TrimSpace(st.FileName))
	if i := strings.LastIndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	return base
}
