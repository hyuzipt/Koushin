package main

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type fillerInfo struct {
	Slug       string
	FillerEps  map[int]bool
	LastLookup time.Time
}

var (
	fillerCache = struct {
		mu      sync.Mutex
		byAniID map[int]*fillerInfo
	}{
		byAniID: make(map[int]*fillerInfo),
	}
	fillerWarned = struct {
		mu  sync.Mutex
		set map[string]bool
	}{
		set: make(map[string]bool),
	}
	fillerRowRe = regexp.MustCompile(`(?s)<tr class="filler[^"]*"[^>]*>.*?<td class="Number">(\d+)</td>`)
)

func levenshtein(a, b string) int {
	a = strings.ToLower(a)
	b = strings.ToLower(b)
	na, nb := len(a), len(b)
	if na == 0 {
		return nb
	}
	if nb == 0 {
		return na
	}
	dp := make([]int, (na+1)*(nb+1))
	idx := func(i, j int) int { return i*(nb+1) + j }

	for i := 0; i <= na; i++ {
		dp[idx(i, 0)] = i
	}
	for j := 0; j <= nb; j++ {
		dp[idx(0, j)] = j
	}

	for i := 1; i <= na; i++ {
		for j := 1; j <= nb; j++ {
			cost := 0
			if a[i-1] != b[j-1] {
				cost = 1
			}
			del := dp[idx(i-1, j)] + 1
			ins := dp[idx(i, j-1)] + 1
			sub := dp[idx(i-1, j-1)] + cost
			dp[idx(i, j)] = minInt(del, minInt(ins, sub))
		}
	}
	return dp[idx(na, nb)]
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

type fillerSearchResult struct {
	Slug  string
	Title string
}

func fetchFillerShows(ctx context.Context) ([]fillerSearchResult, error) {
	results := []fillerSearchResult{}

	req, _ := http.NewRequestWithContext(ctx, "GET", "https://www.animefillerlist.com/shows", nil)
	req.Header.Set("User-Agent", "koushin-filler-check/1.0")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("animefillerlist shows http %d: %s", resp.StatusCode, string(body))
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	htmlStr := string(b)

	needle := `<a href="/shows/`
	for {
		i := strings.Index(htmlStr, needle)
		if i == -1 {
			break
		}
		htmlStr = htmlStr[i+len(`<a href="`):]
		j := strings.Index(htmlStr, `"`)
		if j == -1 {
			break
		}
		href := htmlStr[:j]
		htmlStr = htmlStr[j+2:]

		k := strings.Index(htmlStr, "</a>")
		if k == -1 {
			break
		}
		title := htmlStr[:k]
		title = html.UnescapeString(strings.TrimSpace(title))

		if strings.HasPrefix(href, "/shows/") && title != "" {
			results = append(results, fillerSearchResult{
				Slug:  href,
				Title: title,
			})
		}

		htmlStr = htmlStr[k+4:]
	}

	if len(results) == 0 {
		return nil, errors.New("no shows parsed from animefillerlist")
	}
	return results, nil
}

func searchFillerSlug(ctx context.Context, titles []string) (string, error) {
	shows, err := fetchFillerShows(ctx)
	if err != nil {
		return "", err
	}

	norm := func(s string) string {
		return strings.ToLower(strings.TrimSpace(cleanTitleForSearch(s)))
	}
	tokenSet := func(s string) map[string]bool {
		s = norm(s)
		out := make(map[string]bool)
		for _, w := range strings.Fields(s) {
			w = strings.TrimSpace(w)
			if len(w) < 3 {
				continue
			}
			out[w] = true
		}
		return out
	}
	overlapCount := func(a, b map[string]bool) int {
		n := 0
		for k := range a {
			if b[k] {
				n++
			}
		}
		return n
	}

	type candidate struct {
		Slug     string
		Title    string
		Distance int
	}

	var cands []candidate
	for _, s := range shows {
		baseTitle := s.Title

		secondTitle := ""
		if start := strings.LastIndex(baseTitle, " ("); start != -1 && strings.HasSuffix(baseTitle, ")") {
			secondTitle = baseTitle[start+2 : len(baseTitle)-1]
			baseTitle = baseTitle[:start]
		}

		allVariants := []string{baseTitle}
		if secondTitle != "" {
			allVariants = append(allVariants, secondTitle)
		}
		vTok := make([]map[string]bool, 0, len(allVariants))
		for _, v := range allVariants {
			vTok = append(vTok, tokenSet(v))
		}

		for _, t := range titles {
			t = strings.TrimSpace(t)
			if t == "" {
				continue
			}
			tTok := tokenSet(t)
			qt := len(tTok)
			if qt == 0 {
				continue
			}
			for _, v := range allVariants {
				ov := 0
				for _, vv := range vTok {
					o := overlapCount(tTok, vv)
					if o > ov {
						ov = o
					}
				}
				if ov == 0 {
					continue
				}
				if qt >= 2 && ov*2 < qt {
					continue
				}

				dist := levenshtein(norm(t), norm(v))
				cands = append(cands, candidate{
					Slug:     s.Slug,
					Title:    v,
					Distance: dist,
				})
			}
		}
	}

	if len(cands) == 0 {
		return "", errors.New("no candidates for filler slug")
	}

	sort.Slice(cands, func(i, j int) bool {
		if cands[i].Distance != cands[j].Distance {
			return cands[i].Distance < cands[j].Distance
		}
		return len(cands[i].Title) < len(cands[j].Title)
	})

	best := cands[0]
	if best.Distance > 10 {
		return "", errors.New("no good filler match (distance too high)")
	}

	return best.Slug, nil
}

func fetchFillerEpisodes(ctx context.Context, slug string) (map[int]bool, error) {
	if !strings.HasPrefix(slug, "/") {
		slug = "/shows/" + slug
	}
	fullURL := "https://www.animefillerlist.com" + slug

	req, _ := http.NewRequestWithContext(ctx, "GET", fullURL, nil)
	req.Header.Set("User-Agent", "koushin-filler-check/1.0")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.Request != nil && resp.Request.URL != nil {
		got := resp.Request.URL.String()
		want := fullURL
		if strings.TrimSpace(got) != "" && strings.TrimSpace(want) != "" {
			if !strings.HasPrefix(got, want) {
				return nil, fmt.Errorf("animefillerlist show redirect/mismatch: got %s", got)
			}
		}
	}
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("animefillerlist show http %d: %s", resp.StatusCode, string(body))
	}

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	htmlStr := string(b)
	if !strings.Contains(strings.ToLower(htmlStr), "animefillerlist") {
		return nil, errors.New("animefillerlist mismatch (unexpected page)")
	}

	matches := fillerRowRe.FindAllStringSubmatch(htmlStr, -1)
	filler := make(map[int]bool)

	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		numStr := strings.TrimSpace(m[1])
		if n, err := strconv.Atoi(numStr); err == nil && n > 0 {
			filler[n] = true
		}
	}

	if len(filler) == 0 {
		return nil, errors.New("no filler episodes parsed from page")
	}
	return filler, nil
}

func getFillerInfo(ctx context.Context, aniID int, titles []string) (*fillerInfo, error) {
	fillerCache.mu.Lock()
	if fi, ok := fillerCache.byAniID[aniID]; ok {
		if fi.FillerEps == nil {
			if !fi.LastLookup.IsZero() && time.Since(fi.LastLookup) < 12*time.Hour {
				fillerCache.mu.Unlock()
				return nil, errors.New("no filler data for this show")
			}
		}
		if fi.FillerEps != nil {
			fillerCache.mu.Unlock()
			return fi, nil
		}
	}
	fillerCache.mu.Unlock()

	slug, err := searchFillerSlug(ctx, titles)
	if err != nil {
		fi := &fillerInfo{Slug: "", FillerEps: nil, LastLookup: time.Now()}
		fillerCache.mu.Lock()
		fillerCache.byAniID[aniID] = fi
		fillerCache.mu.Unlock()
		return nil, err
	}
	eps, err := fetchFillerEpisodes(ctx, slug)
	if err != nil {
		fi := &fillerInfo{Slug: slug, FillerEps: nil, LastLookup: time.Now()}
		fillerCache.mu.Lock()
		fillerCache.byAniID[aniID] = fi
		fillerCache.mu.Unlock()
		return nil, err
	}

	fi := &fillerInfo{
		Slug:       slug,
		FillerEps:  eps,
		LastLookup: time.Now(),
	}
	fillerCache.mu.Lock()
	fillerCache.byAniID[aniID] = fi
	fillerCache.mu.Unlock()
	return fi, nil
}

func markFillerWarned(aniID, ep int) bool {
	key := fmt.Sprintf("%d:%d", aniID, ep)
	fillerWarned.mu.Lock()
	defer fillerWarned.mu.Unlock()
	if fillerWarned.set[key] {
		return false
	}
	fillerWarned.set[key] = true
	return true
}
