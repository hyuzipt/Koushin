package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var errAniListRateLimited = errors.New("anilist rate limited")

// AniList allows roughly 90 requests per minute (sometimes less). When it
// answers 429, or reports that no requests are left, every AniList call is
// paused until the window resets instead of hammering the API.
var anilistLimit struct {
	mu    sync.Mutex
	until time.Time
}

// anilistCooldown reports how long AniList calls are paused for (0 if not).
func anilistCooldown() time.Duration {
	anilistLimit.mu.Lock()
	defer anilistLimit.mu.Unlock()
	return time.Until(anilistLimit.until)
}

func anilistPauseFor(d time.Duration) {
	anilistLimit.mu.Lock()
	if until := time.Now().Add(d); until.After(anilistLimit.until) {
		anilistLimit.until = until
	}
	anilistLimit.mu.Unlock()
}

// anilistDo sends a request to AniList, honouring its rate limit. A 429
// response is turned into errAniListRateLimited.
func anilistDo(req *http.Request) (*http.Response, error) {
	if anilistCooldown() > 0 {
		return nil, errAniListRateLimited
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		wait := time.Minute
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
			wait = time.Duration(s) * time.Second
		}
		anilistPauseFor(wait)
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return nil, errAniListRateLimited
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			anilistPauseFor(time.Until(time.Unix(reset, 0)))
		}
	}
	return resp, nil
}

// anilistGraphQL runs a GraphQL query and decodes the JSON response into
// out (which may be nil). token may be empty for public queries.
func anilistGraphQL(ctx context.Context, token, ua, query string, vars map[string]any, out any) error {
	body := map[string]any{"query": query}
	if vars != nil {
		body["variables"] = vars
	}
	bs, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", anilistGQLURL, bytes.NewReader(bs))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := anilistDo(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("anilist http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// mediaFields is the set of fields fetched for every anime lookup.
const mediaFields = `id
      title { romaji english native }
      coverImage { large medium }
      startDate { year }
      episodes
      format`

type mediaLite struct {
	ID    int `json:"id"`
	Title struct {
		Romaji  string `json:"romaji"`
		English string `json:"english"`
		Native  string `json:"native"`
	} `json:"title"`
	CoverImage struct {
		Large  string `json:"large"`
		Medium string `json:"medium"`
	} `json:"coverImage"`
	StartDate struct {
		Year int `json:"year"`
	} `json:"startDate"`
	Episodes int    `json:"episodes"`
	Format   string `json:"format"`
}

type anilistResp struct {
	Data struct {
		Page struct {
			Media []mediaLite `json:"media"`
		} `json:"Page"`
	} `json:"data"`
}

const (
	aniStatusCurrent   = "CURRENT"
	aniStatusPlanning  = "PLANNING"
	aniStatusCompleted = "COMPLETED"
	aniStatusDropped   = "DROPPED"
	aniStatusPaused    = "PAUSED"
	aniStatusRepeating = "REPEATING"
)

func pickBest(ms []mediaLite, query string, wantYear, wantSeason int) (n string, c string, i int, totalEps int) {
	if len(ms) == 0 {
		return "", "", 0, 0
	}

	qNorm := normalizeForTitleMatch(query)
	qTokens := tokenSetForTitleMatch(query)
	qTokN := len(qTokens)

	type scored struct {
		M         mediaLite
		Name      string
		Score     float64
		Exact     bool
		Overlap   int
		TokCount  int
		StartYear int
	}

	scoreMedia := func(m mediaLite) scored {
		variants := []string{m.Title.English, m.Title.Romaji, m.Title.Native}
		best := scored{M: m, Name: mediaLiteTitle(m), Score: 0, Exact: false, Overlap: 0, TokCount: 0, StartYear: m.StartDate.Year}
		for _, v := range variants {
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			vn := normalizeForTitleMatch(v)
			vt := tokenSetForTitleMatch(v)
			o := overlapCount(qTokens, vt)
			cov := 0.0
			if qTokN > 0 {
				cov = float64(o) / float64(qTokN)
			}
			jacc := 0.0
			if u := qTokN + len(vt) - o; u > 0 {
				jacc = float64(o) / float64(u)
			}
			exact := qNorm != "" && vn == qNorm
			prefix := qNorm != "" && (strings.HasPrefix(vn, qNorm) || strings.HasPrefix(qNorm, vn))
			s := cov + 0.25*jacc
			if prefix {
				s += 0.15
			}
			if exact {
				s += 1.0
			}
			if s > best.Score {
				best.Score = s
				best.Exact = exact
				best.Overlap = o
				best.TokCount = len(vt)
				best.Name = strings.TrimSpace(v)
			}
		}
		return best
	}

	var cands []scored
	for _, m := range ms {
		s := scoreMedia(m)
		if qTokN > 0 {
			if qTokN >= 2 {
				if s.Overlap < 2 && !s.Exact {
					continue
				}
			} else {
				if s.Overlap < 1 && !s.Exact {
					continue
				}
			}
		}
		cands = append(cands, s)
	}
	if len(cands) == 0 {
		return "", "", 0, 0
	}

	base := cands

	if wantSeason > 0 {
		tv := make([]scored, 0, len(base))
		for _, s := range base {
			if strings.EqualFold(s.M.Format, "TV") {
				tv = append(tv, s)
			}
		}
		if len(tv) > 0 {
			base = tv
		}
	}

	sort.Slice(base, func(i, j int) bool {
		if base[i].Exact != base[j].Exact {
			return base[i].Exact
		}
		if base[i].Score != base[j].Score {
			return base[i].Score > base[j].Score
		}
		if base[i].TokCount != base[j].TokCount {
			return base[i].TokCount < base[j].TokCount
		}

		yi, yj := base[i].StartYear, base[j].StartYear
		if yi == 0 && yj != 0 {
			return false
		}
		if yi != 0 && yj == 0 {
			return true
		}
		if yi != yj {
			return yi < yj
		}
		return base[i].M.ID < base[j].M.ID
	})

	if wantSeason > 0 {
		if wantSeason <= len(base) {
			m := base[wantSeason-1].M
			n = mediaLiteTitle(m)
			return n, m.CoverImage.Large, m.ID, m.Episodes
		}
	}

	if wantYear > 0 {
		for _, s := range base {
			m := s.M
			if m.StartDate.Year == wantYear {
				n = mediaLiteTitle(m)
				return n, m.CoverImage.Large, m.ID, m.Episodes
			}
		}
	}

	m := base[0].M
	n = mediaLiteTitle(m)
	return n, m.CoverImage.Large, m.ID, m.Episodes
}

func findAniList(ctx context.Context, rawTitle string, wantYear, wantSeason int, ua string) (name, coverURL string, id int, totalEps int, err error) {
	const q = `
query($search: String) {
  Page(perPage: 10) {
    media(search: $search, type: ANIME, sort: [SEARCH_MATCH, START_DATE_DESC]) {
      ` + mediaFields + `
    }
  }
}`
	for _, title := range []string{cleanTitleForSearch(rawTitle), rawTitle} {
		var ar anilistResp
		if err = anilistGraphQL(ctx, "", ua, q, map[string]any{"search": title}, &ar); err != nil {
			if errors.Is(err, errAniListRateLimited) {
				return
			}
			continue
		}
		ms := ar.Data.Page.Media
		if len(ms) == 0 {
			err = errors.New("no match on AniList")
			continue
		}
		name, coverURL, id, totalEps = pickBest(ms, title, wantYear, wantSeason)
		if id != 0 {
			return name, coverURL, id, totalEps, nil
		}
		err = errors.New("no acceptable match on AniList")
	}
	if err == nil {
		err = errors.New("no match on AniList")
	}
	return
}

func searchAniList(ctx context.Context, query string, ua string) ([]mediaLite, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("empty query")
	}
	const q = `
query($search: String) {
  Page(perPage: 25) {
    media(search: $search, type: ANIME, sort: [SEARCH_MATCH, START_DATE_DESC]) {
      ` + mediaFields + `
    }
  }
}`
	var ar anilistResp
	if err := anilistGraphQL(ctx, "", ua, q, map[string]any{"search": query}, &ar); err != nil {
		return nil, err
	}
	return ar.Data.Page.Media, nil
}

func getAniListMediaByID(ctx context.Context, id int, ua string) (mediaLite, error) {
	if id <= 0 {
		return mediaLite{}, errors.New("invalid id")
	}
	const q = `
query($id: Int) {
  Media(id: $id, type: ANIME) {
    ` + mediaFields + `
  }
}`
	var resp struct {
		Data struct {
			Media *mediaLite `json:"Media"`
		} `json:"data"`
	}
	if err := anilistGraphQL(ctx, "", ua, q, map[string]any{"id": id}, &resp); err != nil {
		return mediaLite{}, err
	}
	if resp.Data.Media == nil {
		return mediaLite{}, errors.New("media not found")
	}
	return *resp.Data.Media, nil
}

func anilistURL(id int) string {
	if id <= 0 {
		return ""
	}
	return fmt.Sprintf("https://anilist.co/anime/%d", id)
}

type anilistRewatchCacheEntry struct {
	Rewatching bool
	CheckedAt  time.Time
}

var anilistRewatchCache = struct {
	mu    sync.Mutex
	byAni map[int]anilistRewatchCacheEntry
}{byAni: make(map[int]anilistRewatchCacheEntry)}

func anilistIsRewatching(ctx context.Context, aniID int) bool {
	if aniID <= 0 {
		return false
	}
	store.mu.RLock()
	tok := strings.TrimSpace(store.AccessToken)
	uid := store.UserID
	store.mu.RUnlock()
	if tok == "" || uid <= 0 {
		return false
	}

	anilistRewatchCache.mu.Lock()
	if e, ok := anilistRewatchCache.byAni[aniID]; ok {
		if !e.CheckedAt.IsZero() && time.Since(e.CheckedAt) < 15*time.Minute {
			r := e.Rewatching
			anilistRewatchCache.mu.Unlock()
			return r
		}
	}
	anilistRewatchCache.mu.Unlock()

	c, cancel := context.WithTimeout(ctx, 6*time.Second)
	entry, err := getMediaListEntry(c, tok, uid, aniID)
	cancel()
	if err != nil || entry == nil {
		anilistRewatchCache.mu.Lock()
		anilistRewatchCache.byAni[aniID] = anilistRewatchCacheEntry{Rewatching: false, CheckedAt: time.Now()}
		anilistRewatchCache.mu.Unlock()
		return false
	}

	st := strings.ToUpper(strings.TrimSpace(entry.Status))
	rewatch := st == aniStatusCompleted || st == aniStatusRepeating

	anilistRewatchCache.mu.Lock()
	anilistRewatchCache.byAni[aniID] = anilistRewatchCacheEntry{Rewatching: rewatch, CheckedAt: time.Now()}
	anilistRewatchCache.mu.Unlock()
	return rewatch
}

func mediaLiteTitle(m mediaLite) string {
	name := strings.TrimSpace(m.Title.English)
	if name == "" {
		name = firstNonEmpty(m.Title.Romaji, m.Title.Native)
	}
	return strings.TrimSpace(name)
}

func oauthLogin(ctx context.Context) error {
	clientID := anilistClientID

	tokenCh := make(chan string, 1)
	errCh := make(chan error, 1)
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second}
	mux := http.NewServeMux()

	receivedResponse := false
	var responseMu sync.Mutex

	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		responseMu.Lock()
		receivedResponse = true
		responseMu.Unlock()

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, `<!doctype html>
<html><head><meta charset="utf-8"><title>Koushin · AniList Login</title></head>
<body style="font-family:system-ui;max-width:680px;margin:40px auto;line-height:1.5;text-align:center">
<h2>Completing login...</h2>
<p id="status">Processing authentication...</p>
<script>
(function() {
	// The access token is in the URL fragment (after #)
	const hash = window.location.hash.substring(1); // Remove the leading #
	
	if (!hash) {
		document.getElementById('status').textContent = 'No authentication data found. Please try again.';
		return;
	}
	
	const params = new URLSearchParams(hash);
	const token = params.get('access_token');
	const error = params.get('error');
	
	if (error) {
		document.getElementById('status').textContent = 'Authentication failed: ' + error;
		fetch('/submit', {
			method: 'POST',
			headers: {'Content-Type': 'application/json'},
			body: JSON.stringify({error: error})
		});
		return;
	}
	
	if (token) {
		document.getElementById('status').textContent = 'Login successful! Closing...';
		fetch('/submit', {
			method: 'POST',
			headers: {'Content-Type': 'application/json'},
			body: JSON.stringify({token: token})
		}).then(response => {
			if (response.ok) {
				document.getElementById('status').textContent = 'Login successful! You can close this window.';
				setTimeout(() => window.close(), 1500);
			} else {
				document.getElementById('status').textContent = 'Login failed. Please try again.';
			}
		}).catch(err => {
			document.getElementById('status').textContent = 'Error: ' + err.message;
		});
	} else {
		document.getElementById('status').textContent = 'No access token found. Please try again.';
	}
})();
</script>
</body></html>`)
	})

	// The page may post more than once; never block the handler.
	sendErr := func(err error) {
		select {
		case errCh <- err:
		default:
		}
	}

	mux.HandleFunc("/submit", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Token string `json:"token"`
			Error string `json:"error"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			sendErr(fmt.Errorf("failed to decode response: %w", err))
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if body.Error != "" {
			sendErr(fmt.Errorf("anilist oauth error: %s", body.Error))
			http.Error(w, "authentication error", http.StatusBadRequest)
			return
		}
		tok := strings.TrimSpace(body.Token)
		if tok == "" {
			sendErr(errors.New("empty access token"))
			http.Error(w, "empty token", http.StatusBadRequest)
			return
		}
		select {
		case tokenCh <- tok:
		default:
		}
		_, _ = w.Write([]byte("OK"))
	})

	ln, err := net.Listen("tcp", localLoginAddr)
	if err != nil {
		return fmt.Errorf("could not start the login callback on %s: %w", localLoginAddr, err)
	}
	srv.Handler = mux
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		c2, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = srv.Shutdown(c2)
		cancel()
	}()

	authURL := fmt.Sprintf("https://anilist.co/api/v2/oauth/authorize?client_id=%s&response_type=token", clientID)
	logAppend("oauth(implicit): opening ", authURL)
	openBrowser(authURL)

	timeout := time.NewTimer(2 * time.Minute)
	defer timeout.Stop()

	var accessToken string
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		return err
	case accessToken = <-tokenCh:
		logAppend("oauth(implicit): got token len=", fmt.Sprint(len(accessToken)))
	case <-timeout.C:
		responseMu.Lock()
		gotResponse := receivedResponse
		responseMu.Unlock()
		if !gotResponse {
			logAppend("oauth(implicit): timeout - user likely closed the browser")
			return errors.New("login cancelled or timed out")
		}
		return errors.New("authentication timeout")
	}

	name, uid, whoErr := whoAmI(ctx, accessToken)
	if whoErr != nil {
		logAppend("oauth: whoAmI error: ", whoErr.Error())
		return fmt.Errorf("failed to verify token: %w", whoErr)
	}

	store.mu.Lock()
	store.AccessToken = accessToken
	store.Username = name
	store.UserID = uid
	store.ShowAniProfile = true
	store.mu.Unlock()
	store.Save()

	logAppend("oauth: success; logged in as ", name, " (id ", fmt.Sprint(uid), ")")
	return nil
}

func whoAmI(ctx context.Context, token string) (username string, userID int, err error) {
	var out struct {
		Data struct {
			Viewer struct {
				ID   int    `json:"id"`
				Name string `json:"name"`
			} `json:"Viewer"`
		} `json:"data"`
	}
	if err := anilistGraphQL(ctx, token, "", `query{ Viewer { id name } }`, nil, &out); err != nil {
		return "", 0, fmt.Errorf("whoAmI: %w", err)
	}
	return out.Data.Viewer.Name, out.Data.Viewer.ID, nil
}

func randBase64URL(n int) string {
	if n <= 0 {
		n = 32
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

type mediaListEntryLite struct {
	ID       int    `json:"id"`
	Status   string `json:"status"`
	Progress int    `json:"progress"`
}

func getMediaListEntry(ctx context.Context, token string, userID int, mediaID int) (*mediaListEntryLite, error) {
	const q = `
query($userId:Int, $mediaId:Int) {
  MediaList(userId:$userId, mediaId:$mediaId, type: ANIME) {
    id
    status
    progress
  }
}`
	var out struct {
		Data struct {
			MediaList *mediaListEntryLite `json:"MediaList"`
		} `json:"data"`
	}
	vars := map[string]any{"userId": userID, "mediaId": mediaID}
	if err := anilistGraphQL(ctx, token, "", q, vars, &out); err != nil {
		return nil, err
	}
	return out.Data.MediaList, nil
}

func saveMediaListEntry(ctx context.Context, token string, mediaID int, progress int, status string) error {
	const m = `
mutation($mediaId:Int, $progress:Int, $status:MediaListStatus) {
  SaveMediaListEntry(mediaId:$mediaId, progress:$progress, status:$status) { id status progress }
}`
	vars := map[string]any{"mediaId": mediaID, "progress": progress, "status": status}
	return anilistGraphQL(ctx, token, "", m, vars, nil)
}

func syncAniListProgress(ctx context.Context, token string, userID int, mediaID int, progress int, totalEps int) error {
	if strings.TrimSpace(token) == "" || userID <= 0 || mediaID <= 0 || progress <= 0 {
		return errors.New("invalid sync parameters")
	}
	if totalEps == 1 && progress > 1 {
		progress = 1
	}
	if totalEps > 0 && progress > totalEps {
		progress = totalEps
	}

	var existingStatus string
	if e, err := getMediaListEntry(ctx, token, userID, mediaID); err == nil && e != nil {
		existingStatus = strings.TrimSpace(e.Status)
	}

	targetStatus := aniStatusCurrent
	switch strings.ToUpper(existingStatus) {
	case aniStatusCompleted:
		targetStatus = aniStatusRepeating
	case aniStatusRepeating:
		targetStatus = aniStatusRepeating
	case aniStatusCurrent:
		targetStatus = aniStatusCurrent
	case "":
		targetStatus = aniStatusCurrent
	default:
		targetStatus = aniStatusCurrent
	}

	if totalEps > 0 && progress >= totalEps {
		targetStatus = aniStatusCompleted
	}

	return saveMediaListEntry(ctx, token, mediaID, progress, targetStatus)
}

func refreshViewerFromToken(ctx context.Context) {
	store.mu.RLock()
	tok := strings.TrimSpace(store.AccessToken)
	store.mu.RUnlock()
	if tok == "" {
		return
	}
	c, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	name, uid, err := whoAmI(c, tok)
	if err != nil {
		logAppend("viewer refresh failed: ", err.Error())
		return
	}

	store.mu.Lock()
	changed := store.Username != name || store.UserID != uid
	store.Username = name
	store.UserID = uid
	store.mu.Unlock()
	if changed {
		store.Save()
		refreshAuthMenu()
	}
}

func anilistTrackingEnabled() bool {
	store.mu.RLock()
	on := store.AniListTracking
	hasToken := strings.TrimSpace(store.AccessToken) != ""
	uid := store.UserID
	store.mu.RUnlock()
	return on && hasToken && uid > 0
}
