package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/navidrome/navidrome/plugins/pdk/go/host"
	"github.com/navidrome/navidrome/plugins/pdk/go/metadata"
	"github.com/navidrome/navidrome/plugins/pdk/go/pdk"
)

const (
	userAgent               = "NavidromeAppleMusicPlugin/0.1"
	defaultCountry          = "us"
	defaultCacheTTL         = 7 // days
	defaultTopSongs         = 10
	httpTimeoutMs           = 10000
	negativeCacheTTLSeconds = 7200 // 2 hours
	iTunesSearchURL         = "https://itunes.apple.com/search"
	iTunesLookupURL         = "https://itunes.apple.com/lookup"
	appleMusicBaseURL       = "https://music.apple.com"

	// HTML parsing limits
	similarSectionMaxBytes = 60000 // generous chunk after section marker to cover all artist lockups
	sectionBoundaryOffset  = 100   // skip initial chars before searching for next section boundary

	// Config keys
	configCountries       = "countries"
	configCacheTTLDays    = "cache_ttl_days"
	configArtistURL       = "enable_artist_url"
	configArtistBiography = "enable_artist_biography"
	configArtistImages    = "enable_artist_images"
	configSimilarArtists  = "enable_similar_artists"
	configTopSongs        = "enable_top_songs"
	configAlbumImages     = "enable_album_images"
)

// Compile-time interface assertions
var (
	_ metadata.ArtistURLProvider       = (*appleMusicAgent)(nil)
	_ metadata.ArtistBiographyProvider = (*appleMusicAgent)(nil)
	_ metadata.ArtistImagesProvider    = (*appleMusicAgent)(nil)
	_ metadata.SimilarArtistsProvider  = (*appleMusicAgent)(nil)
	_ metadata.ArtistTopSongsProvider  = (*appleMusicAgent)(nil)
	_ metadata.AlbumImagesProvider     = (*appleMusicAgent)(nil)
)

func init() {
	metadata.Register(&appleMusicAgent{})
}

func main() {}

type appleMusicAgent struct{}

// --- iTunes API response types ---

type itunesSearchResponse struct {
	ResultCount int                  `json:"resultCount"`
	Results     []itunesArtistResult `json:"results"`
}

type itunesArtistResult struct {
	WrapperType string `json:"wrapperType"`
	ArtistName  string `json:"artistName"`
	ArtistID    int64  `json:"artistId"`
}

type itunesLookupResponse struct {
	ResultCount int                  `json:"resultCount"`
	Results     []itunesLookupResult `json:"results"`
}

type itunesLookupResult struct {
	WrapperType string `json:"wrapperType"`
	ArtistName  string `json:"artistName"`
	TrackName   string `json:"trackName"`
	ArtistID    int64  `json:"artistId"`
}

type itunesAlbumSearchResponse struct {
	ResultCount int                 `json:"resultCount"`
	Results     []itunesAlbumResult `json:"results"`
}

type itunesAlbumResult struct {
	WrapperType    string `json:"wrapperType"`
	CollectionName string `json:"collectionName"`
	ArtistName     string `json:"artistName"`
	ArtworkURL100  string `json:"artworkUrl100"`
}

// --- Scraped page data ---

type parsedPageData struct {
	Biography      string              `json:"biography,omitempty"`
	ImageURL       string              `json:"imageURL,omitempty"`
	SimilarArtists []similarArtistInfo `json:"similarArtists,omitempty"`
}

type similarArtistInfo struct {
	Name string `json:"name"`
}

// --- Cached artist ID ---

type cachedArtistID struct {
	ArtistID int64 `json:"artistId"`
}

type cachedAlbumArtwork struct {
	ArtworkURL string `json:"artworkUrl"`
}

// --- JSON-LD structure ---

type jsonLDData struct {
	Context     string `json:"@context"`
	Type        string `json:"@type"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Image       string `json:"image"`
}

// --- Config helpers ---

// getCountries returns the ordered list of country codes from config.
func getCountries() []string {
	val, exists := host.ConfigGet(configCountries)
	if !exists || strings.TrimSpace(val) == "" {
		return []string{defaultCountry}
	}
	parts := strings.Split(val, ",")
	countries := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(strings.ToLower(p))
		if p != "" {
			countries = append(countries, p)
		}
	}
	if len(countries) == 0 {
		return []string{defaultCountry}
	}
	return countries
}

// isEnabled returns whether a capability is enabled via config.
// Capabilities default to enabled; set the config value to "false" to disable.
func isEnabled(key string) bool {
	val, exists := host.ConfigGet(key)
	return !exists || val != "false"
}

// getCacheTTLSeconds returns the cache TTL in seconds from config.
func getCacheTTLSeconds() int64 {
	days, exists := host.ConfigGetInt(configCacheTTLDays)
	if !exists || days <= 0 {
		days = defaultCacheTTL
	}
	return days * 24 * 60 * 60
}

// --- KVStore helpers ---

// kvGet retrieves and unmarshals a JSON value from KVStore.
func kvGet(key string, target any) bool {
	data, exists, err := host.KVStoreGet(key)
	if err != nil {
		pdk.Log(pdk.LogWarn, "KVStore error for key "+key+": "+err.Error())
		return false
	}
	if !exists {
		return false
	}
	if err := json.Unmarshal(data, target); err != nil {
		return false
	}
	return true
}

// kvSet stores a JSON value in KVStore with no TTL (permanent).
func kvSet(key string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return host.KVStoreSet(key, data)
}

// kvSetWithTTL stores a JSON value in KVStore with a TTL in seconds.
func kvSetWithTTL(key string, value any, ttlSeconds int64) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return host.KVStoreSetWithTTL(key, data, ttlSeconds)
}

// clampLimit returns limit clamped to [1, total], or total when limit <= 0.
func clampLimit(limit, total int) int {
	if limit <= 0 || limit > total {
		return total
	}
	return limit
}

// --- HTTP helper ---

// httpGet performs a GET request and returns the response body.
func httpGet(rawURL string) ([]byte, int32, error) {
	resp, err := host.HTTPSend(host.HTTPRequest{
		Method:    "GET",
		URL:       rawURL,
		Headers:   map[string]string{"User-Agent": userAgent},
		TimeoutMs: httpTimeoutMs,
	})
	if err != nil {
		return nil, 0, err
	}
	return resp.Body, resp.StatusCode, nil
}

// httpGetJSON performs a GET request, checks for 200 status, and unmarshals the JSON response.
func httpGetJSON(rawURL string, target any) error {
	body, statusCode, err := httpGet(rawURL)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	if statusCode != 200 {
		return fmt.Errorf("returned status %d", statusCode)
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("failed to parse response: %w", err)
	}
	return nil
}

// --- Name normalization ---

// normalizeName normalizes an artist or album name for cache key use.
func normalizeName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// --- Artist resolution ---

// resolveArtistID looks up an Apple Music artist ID by name.
// Uses KVStore cache for permanent storage of name→ID mappings.
func resolveArtistID(artistName string) (int64, error) {
	normalized := normalizeName(artistName)
	if normalized == "" {
		return 0, errors.New("empty artist name")
	}

	// Check cache
	cacheKey := "artist:" + normalized
	var cached cachedArtistID
	if kvGet(cacheKey, &cached) {
		if cached.ArtistID == 0 {
			pdk.Log(pdk.LogDebug, "artist ID negative cache hit: "+normalized)
			return 0, nil
		}
		pdk.Log(pdk.LogDebug, "artist ID cache hit: "+normalized)
		return cached.ArtistID, nil
	}

	// Search iTunes API
	countries := getCountries()
	country := countries[0]

	searchURL := fmt.Sprintf("%s?term=%s&entity=musicArtist&limit=5&country=%s",
		iTunesSearchURL, url.QueryEscape(artistName), url.QueryEscape(country))

	pdk.Log(pdk.LogDebug, "searching iTunes API: "+searchURL)

	var searchResp itunesSearchResponse
	if err := httpGetJSON(searchURL, &searchResp); err != nil {
		return 0, fmt.Errorf("iTunes artist search: %w", err)
	}

	if searchResp.ResultCount == 0 {
		pdk.Log(pdk.LogDebug, "no artist found for: "+artistName)
		if err := kvSetWithTTL(cacheKey, cachedArtistID{ArtistID: 0}, negativeCacheTTLSeconds); err != nil {
			pdk.Log(pdk.LogWarn, "failed to cache negative artist result: "+err.Error())
		}
		return 0, nil
	}

	// Find best match by name similarity
	bestMatch := findBestArtistMatch(artistName, searchResp.Results)
	if bestMatch == nil {
		pdk.Log(pdk.LogDebug, "no matching artist found for: "+artistName)
		if err := kvSetWithTTL(cacheKey, cachedArtistID{ArtistID: 0}, negativeCacheTTLSeconds); err != nil {
			pdk.Log(pdk.LogWarn, "failed to cache negative artist result: "+err.Error())
		}
		return 0, nil
	}

	// Cache permanently
	if err := kvSet(cacheKey, cachedArtistID{ArtistID: bestMatch.ArtistID}); err != nil {
		pdk.Log(pdk.LogWarn, "failed to cache artist ID: "+err.Error())
	}

	pdk.Log(pdk.LogInfo, fmt.Sprintf("resolved artist '%s' → ID %d", artistName, bestMatch.ArtistID))
	return bestMatch.ArtistID, nil
}

// isNonLatin returns true if the string contains characters outside Latin + Latin Extended
// (U+0000–U+024F). Used to detect when string comparison against an iTunes result is
// meaningless — e.g. querying 新しい学校 will return "ATARASHII GAKKO!" which we can't
// verify ourselves, so we defer to iTunes' own confidence instead.
func isNonLatin(s string) bool {
	for _, r := range s {
		if r > 0x024F {
			return true
		}
	}
	return false
}

// simplifyName produces a comparable form for fuzzy name matching.
// Strips diacritics, collapses anything non-alphanumeric to spaces, and drops
// leading articles — so "Queensrÿche", "Queensryche", and "The Queensryche" all
// reduce to the same string.
func simplifyName(s string) string {
	replacer := strings.NewReplacer(
		"à", "a", "á", "a", "â", "a", "ä", "a", "å", "a", "ã", "a",
		"è", "e", "é", "e", "ê", "e", "ë", "e",
		"ì", "i", "í", "i", "î", "i", "ï", "i",
		"ò", "o", "ó", "o", "ô", "o", "ö", "o", "ø", "o", "õ", "o",
		"ù", "u", "ú", "u", "û", "u", "ü", "u",
		"ý", "y", "ÿ", "y", "ñ", "n", "ç", "c",
		"ß", "ss", "æ", "ae", "œ", "oe",
	)
	s = replacer.Replace(strings.ToLower(strings.TrimSpace(s)))
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}
	s = strings.Join(strings.Fields(b.String()), " ")
	for _, article := range []string{"the ", "a ", "an "} {
		s = strings.TrimPrefix(s, article)
	}
	return s
}

// editDistance returns the Levenshtein distance between two strings.
func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	la, lb := len(ra), len(rb)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev, curr := make([]int, lb+1), make([]int, lb+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			if ra[i-1] == rb[j-1] {
				curr[j] = prev[j-1]
			} else {
				d := prev[j]
				if prev[j-1] < d {
					d = prev[j-1]
				}
				if curr[j-1] < d {
					d = curr[j-1]
				}
				curr[j] = 1 + d
			}
		}
		prev, curr = curr, prev
	}
	return prev[lb]
}

// findBestArtistMatch selects the right artist from iTunes search results, or returns nil
// if nothing is close enough. A wrong match here poisons the provider waterfall by
// returning data that looks valid but isn't, so the bar for accepting a result is
// intentionally higher than the bar for rejecting one.
//
// For Latin queries, similarity is checked proportionally: the edit distance ceiling is
// len(simplifiedQuery)/4, minimum 1. This tolerates real-world noise (punctuation, minor
// spelling variants) while rejecting cases like "psynwav" matching "psywar" (distance 2,
// ceiling 1).
//
// For non-Latin queries, comparison against an iTunes result is not meaningful — we'd be
// comparing e.g. 新しい学校 against "ATARASHII GAKKO!". A single returned candidate means
// iTunes was confident enough to commit to one answer, so we trust it. Multiple candidates
// with no matching simplified name means we can't discriminate, so we return nil.
func findBestArtistMatch(query string, results []itunesArtistResult) *itunesArtistResult {
	candidates := make([]*itunesArtistResult, 0, len(results))
	for i := range results {
		if results[i].WrapperType == "artist" {
			candidates = append(candidates, &results[i])
		}
	}
	if len(candidates) == 0 {
		return nil
	}

	simplifiedQuery := simplifyName(query)

	for _, r := range candidates {
		if simplifyName(r.ArtistName) == simplifiedQuery {
			return r
		}
	}

	if isNonLatin(query) {
		if len(candidates) == 1 {
			pdk.Log(pdk.LogDebug, fmt.Sprintf("trusting single iTunes result %q for non-Latin query %q", candidates[0].ArtistName, query))
			return candidates[0]
		}
		return nil
	}

	ceiling := len([]rune(simplifiedQuery)) / 4
	if ceiling < 1 {
		ceiling = 1
	}
	for _, r := range candidates {
		if editDistance(simplifiedQuery, simplifyName(r.ArtistName)) <= ceiling {
			return r
		}
	}

	return nil
}

// baseNameDelimiters are characters that typically separate the core album title
// from metadata decorations (e.g., remaster info, edition, format).
var baseNameDelimiters = []string{" (", " [", " - ", ": "}

// extractBaseName extracts the core album title by truncating at each known
// delimiter type that separates it from metadata decorations.
// e.g., "The Dark Side of the Moon (50th Anniversary) [Remastered]" → "the dark side of the moon"
// e.g., "Versions - Single" → "versions"
func extractBaseName(normalized string) string {
	for _, delim := range baseNameDelimiters {
		if idx := strings.Index(normalized, delim); idx > 0 {
			normalized = normalized[:idx]
		}
	}
	return strings.TrimSpace(normalized)
}

// findBestAlbumMatch finds an album matching by name from lookup results.
// Results are assumed to be pre-filtered by artist (via Lookup API by artist ID),
// so no artist name check is performed.
// Uses a multi-pass strategy with decreasing strictness:
//   - Pass 1: exact match on full collection name
//   - Pass 2: exact match on base names (after stripping parenthetical/bracket/dash decorations)
//   - Pass 3: containment match on base names (one contains the other)
func findBestAlbumMatch(albumName string, results []itunesAlbumResult) *itunesAlbumResult {
	normalizedAlbum := normalizeName(albumName)
	baseAlbum := extractBaseName(normalizedAlbum)

	// Filter to collection entries
	type candidate struct {
		index          int
		normalizedName string
		baseName       string
	}
	var candidates []candidate
	for i := range results {
		if results[i].WrapperType != "collection" {
			continue
		}
		cn := normalizeName(results[i].CollectionName)
		candidates = append(candidates, candidate{
			index:          i,
			normalizedName: cn,
			baseName:       extractBaseName(cn),
		})
	}

	// Pass 1: exact match on full name
	for _, c := range candidates {
		if c.normalizedName == normalizedAlbum {
			return &results[c.index]
		}
	}

	// Pass 2: exact match on base names
	for _, c := range candidates {
		if c.baseName == baseAlbum {
			return &results[c.index]
		}
	}

	// Pass 3: containment — one base name contains the other.
	// Require the shorter name to be at least 4 characters to avoid false positives.
	if len(baseAlbum) >= 4 {
		for _, c := range candidates {
			if len(c.baseName) >= 4 &&
				(strings.Contains(c.baseName, baseAlbum) || strings.Contains(baseAlbum, c.baseName)) {
				return &results[c.index]
			}
		}
	}

	return nil
}

// resolveAlbumArtwork looks up album artwork URL via the iTunes Lookup API.
// Uses the artist ID to fetch all albums, then matches by album name.
// Uses KVStore cache with TTL. Caches "not found" with a shorter negative TTL.
func resolveAlbumArtwork(albumName, artistName string) (string, error) {
	normalizedAlbum := normalizeName(albumName)
	normalizedArtist := normalizeName(artistName)
	if normalizedAlbum == "" {
		return "", errors.New("empty album name")
	}
	if normalizedArtist == "" {
		return "", errors.New("empty artist name")
	}

	// Check cache
	cacheKey := fmt.Sprintf("album:%s:%s", normalizedArtist, normalizedAlbum)
	var cached cachedAlbumArtwork
	if kvGet(cacheKey, &cached) {
		if cached.ArtworkURL == "" {
			pdk.Log(pdk.LogDebug, "album artwork negative cache hit: "+cacheKey)
			return "", nil
		}
		pdk.Log(pdk.LogDebug, "album artwork cache hit: "+cacheKey)
		return cached.ArtworkURL, nil
	}

	// Resolve artist ID first
	artistID, err := resolveArtistID(artistName)
	if err != nil {
		return "", fmt.Errorf("failed to resolve artist for album lookup: %w", err)
	}
	if artistID == 0 {
		pdk.Log(pdk.LogDebug, "artist not found for album lookup: "+artistName)
		return "", nil
	}

	// Look up all albums by artist ID via the iTunes Lookup API
	lookupURL := fmt.Sprintf("%s?id=%d&entity=album&limit=200", iTunesLookupURL, artistID)

	pdk.Log(pdk.LogDebug, "looking up albums for artist: "+lookupURL)

	var lookupResp itunesAlbumSearchResponse
	if err := httpGetJSON(lookupURL, &lookupResp); err != nil {
		return "", fmt.Errorf("iTunes album lookup: %w", err)
	}

	// Find match by album name (artist already matched via artist ID)
	bestMatch := findBestAlbumMatch(albumName, lookupResp.Results)

	var artworkURL string
	if bestMatch != nil {
		artworkURL = bestMatch.ArtworkURL100
	}
	if artworkURL == "" {
		pdk.Log(pdk.LogDebug, fmt.Sprintf("no matching album found for '%s' by '%s'", albumName, artistName))
		if err := kvSetWithTTL(cacheKey, cachedAlbumArtwork{ArtworkURL: ""}, negativeCacheTTLSeconds); err != nil {
			pdk.Log(pdk.LogWarn, "failed to cache negative album result: "+err.Error())
		}
		return "", nil
	}

	// Cache with standard TTL
	ttl := getCacheTTLSeconds()
	if err := kvSetWithTTL(cacheKey, cachedAlbumArtwork{ArtworkURL: artworkURL}, ttl); err != nil {
		pdk.Log(pdk.LogWarn, "failed to cache album artwork: "+err.Error())
	}

	pdk.Log(pdk.LogInfo, fmt.Sprintf("resolved album '%s' by '%s' → artwork URL", albumName, artistName))
	return artworkURL, nil
}

// --- HTML parsing helpers ---

// jsonLDRegex matches a <script> tag containing type="application/ld+json", regardless of
// other attributes (e.g. id=...) that may appear before or after the type attribute.
var jsonLDRegex = regexp.MustCompile(`(?i)<script[^>]*type="application/ld\+json"[^>]*>`)

// parseJSONLD extracts and parses JSON-LD data from an HTML page.
func parseJSONLD(html string) (*jsonLDData, error) {
	const endTag = `</script>`

	loc := jsonLDRegex.FindStringIndex(html)
	if loc == nil {
		return nil, errors.New("no JSON-LD found")
	}
	startIdx := loc[1] // position right after the opening tag

	endIdx := strings.Index(html[startIdx:], endTag)
	if endIdx == -1 {
		return nil, errors.New("malformed JSON-LD")
	}

	jsonStr := strings.TrimSpace(html[startIdx : startIdx+endIdx])

	var ld jsonLDData
	if err := json.Unmarshal([]byte(jsonStr), &ld); err != nil {
		return nil, fmt.Errorf("failed to parse JSON-LD: %w", err)
	}

	return &ld, nil
}

// placeholderImageURL is the generic Apple Music image that should be treated as "no image".
const placeholderImageURL = "https://music.apple.com/assets/meta/apple-music.png"

// isPlaceholderImage returns true if the URL is Apple Music's generic placeholder.
func isPlaceholderImage(url string) bool {
	return url == placeholderImageURL
}

// parseOpenGraphImage extracts the og:image URL from an HTML page.
func parseOpenGraphImage(html string) string {
	// Look for <meta property="og:image" content="...">
	pattern := `<meta property="og:image" content="`
	idx := strings.Index(html, pattern)
	if idx == -1 {
		return ""
	}
	idx += len(pattern)
	endIdx := strings.Index(html[idx:], `"`)
	if endIdx == -1 {
		return ""
	}
	return html[idx : idx+endIdx]
}

// imageURLRegex matches Apple's mzstatic.com image dimension segments like "486x486bb".
var imageURLRegex = regexp.MustCompile(`/\d+x\d+[a-z]*\.`)

// rewriteImageSize rewrites an Apple mzstatic.com image URL to the given size.
func rewriteImageSize(imageURL string, size int) string {
	return imageURLRegex.ReplaceAllString(imageURL, fmt.Sprintf("/%dx%dbb.", size, size))
}

// buildImageList generates ImageInfo entries in multiple sizes from a base artwork URL.
func buildImageList(baseURL string) []metadata.ImageInfo {
	sizes := []int{1500, 600, 300}
	images := make([]metadata.ImageInfo, 0, len(sizes))
	for _, size := range sizes {
		images = append(images, metadata.ImageInfo{
			URL:  rewriteImageSize(baseURL, size),
			Size: int32(size),
		})
	}
	return images
}

// similarSectionMarkers contains localized aria-label values for the "Similar Artists" section.
var similarSectionMarkers = []string{
	`aria-label="Similar Artists"`,
	`aria-label="Artistas semelhantes"`,
	`aria-label="Ähnliche Künstler"`,
	`aria-label="Artistes similaires"`,
	`aria-label="Artistas similares"`,
}

// lockupTitleRegex matches artist names inside the ellipse-lockup title elements.
// Apple Music uses: <h3 data-testid="ellipse-lockup__title" ...>Artist Name</h3>
var lockupTitleRegex = regexp.MustCompile(`data-testid="ellipse-lockup__title"[^>]*>([^<]+)<`)

// parseSimilarArtists extracts similar artist names from the Apple Music HTML page.
func parseSimilarArtists(html string) []similarArtistInfo {
	// Find the similar artists section by looking for localized aria-label markers.
	sectionStart := -1
	for _, marker := range similarSectionMarkers {
		idx := strings.Index(html, marker)
		if idx != -1 {
			pdk.Log(pdk.LogDebug, fmt.Sprintf("similar artists: found marker %q at position %d", marker, idx))
			sectionStart = idx
			break
		}
	}

	if sectionStart == -1 {
		pdk.Log(pdk.LogDebug, "similar artists: no section markers found in HTML")
		return nil
	}

	// Extract a generous chunk after the section marker to cover all artist lockups.
	sectionEnd := min(sectionStart+similarSectionMaxBytes, len(html))
	section := html[sectionStart:sectionEnd]

	// Limit to the current section by finding the next section boundary.
	if nextSection := strings.Index(section[sectionBoundaryOffset:], `data-testid="section-container"`); nextSection != -1 {
		section = section[:sectionBoundaryOffset+nextSection]
	}
	pdk.Log(pdk.LogDebug, fmt.Sprintf("similar artists: extracting from section (%d chars)", len(section)))

	// Extract artist names from ellipse-lockup title elements.
	var artists []similarArtistInfo
	seen := make(map[string]bool)

	matches := lockupTitleRegex.FindAllStringSubmatch(section, -1)
	pdk.Log(pdk.LogDebug, fmt.Sprintf("similar artists: found %d lockup titles in section", len(matches)))
	for _, m := range matches {
		name := strings.TrimSpace(m[1])
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		artists = append(artists, similarArtistInfo{Name: name})
		pdk.Log(pdk.LogDebug, fmt.Sprintf("similar artists: found artist %q", name))
	}

	pdk.Log(pdk.LogDebug, fmt.Sprintf("similar artists: total found=%d", len(artists)))
	return artists
}

// --- Web page fetching ---

// pageField identifies which field of parsedPageData must be non-empty.
type pageField int

const (
	fieldAny       pageField = iota // any non-empty field
	fieldBiography                  // Biography
	fieldImage                      // ImageURL
	fieldSimilar                    // SimilarArtists
)

// fetchArtistPage fetches and parses the Apple Music artist page.
// Tries each country code in order until content is found.
func fetchArtistPage(artistID int64, wantField pageField) (*parsedPageData, error) {
	countries := getCountries()
	ttl := getCacheTTLSeconds()
	var firstResult *parsedPageData

	for _, country := range countries {
		cacheKey := fmt.Sprintf("page:%d:%s", artistID, country)

		// Check cache
		var cached parsedPageData
		if kvGet(cacheKey, &cached) {
			pdk.Log(pdk.LogDebug, fmt.Sprintf("page cache hit: %s", cacheKey))
			if firstResult == nil {
				firstResult = &cached
			}
			if hasField(&cached, wantField) {
				return &cached, nil
			}
			continue
		}

		// Fetch page
		pageURL := fmt.Sprintf("%s/%s/artist/-/%d", appleMusicBaseURL, country, artistID)
		pdk.Log(pdk.LogDebug, "fetching Apple Music page: "+pageURL)

		body, statusCode, err := httpGet(pageURL)
		if err != nil {
			pdk.Log(pdk.LogWarn, fmt.Sprintf("failed to fetch page for country %s: %s", country, err.Error()))
			continue
		}
		if statusCode != 200 {
			pdk.Log(pdk.LogWarn, fmt.Sprintf("Apple Music page returned %d for country %s", statusCode, country))
			continue
		}

		html := string(body)
		pdk.Log(pdk.LogDebug, fmt.Sprintf("received page for country %s: %d bytes, status %d", country, len(body), statusCode))
		page := parsePage(html)

		// Cache the result
		if err := kvSetWithTTL(cacheKey, page, ttl); err != nil {
			pdk.Log(pdk.LogWarn, "failed to cache page data: "+err.Error())
		}

		if firstResult == nil {
			firstResult = page
		}
		if hasField(page, wantField) {
			return page, nil
		}
	}

	if firstResult != nil {
		return firstResult, nil
	}
	pdk.Log(pdk.LogDebug, fmt.Sprintf("no page data found for artist %d in any country", artistID))
	return nil, nil
}

// parsePage extracts all metadata from an Apple Music artist HTML page.
// Always parses all fields so the cached result is complete for any future capability request.
func parsePage(html string) *parsedPageData {
	page := &parsedPageData{}

	pdk.Log(pdk.LogDebug, fmt.Sprintf("parsing page HTML (%d bytes)", len(html)))

	// Parse JSON-LD for biography and image
	ld, err := parseJSONLD(html)
	if err == nil {
		page.Biography = ld.Description
		page.ImageURL = ld.Image
		pdk.Log(pdk.LogDebug, fmt.Sprintf("JSON-LD parsed: type=%s, name=%s, bio=%d chars, image=%s",
			ld.Type, ld.Name, len(ld.Description), ld.Image))
	} else {
		pdk.Log(pdk.LogDebug, "JSON-LD parsing failed: "+err.Error())
	}

	// Discard generic Apple Music placeholder image
	if isPlaceholderImage(page.ImageURL) {
		pdk.Log(pdk.LogDebug, "discarding placeholder image: "+page.ImageURL)
		page.ImageURL = ""
	}

	// Fallback to OpenGraph for image
	if page.ImageURL == "" {
		page.ImageURL = parseOpenGraphImage(html)
		if isPlaceholderImage(page.ImageURL) {
			pdk.Log(pdk.LogDebug, "discarding placeholder OpenGraph image: "+page.ImageURL)
			page.ImageURL = ""
		} else if page.ImageURL != "" {
			pdk.Log(pdk.LogDebug, "OpenGraph image found: "+page.ImageURL)
		} else {
			pdk.Log(pdk.LogDebug, "no OpenGraph image found")
		}
	}

	// Parse similar artists
	page.SimilarArtists = parseSimilarArtists(html)

	pdk.Log(pdk.LogDebug, fmt.Sprintf("parsed page result: bio=%d chars, image=%v, similar=%d",
		len(page.Biography), page.ImageURL != "", len(page.SimilarArtists)))

	return page
}

// hasField checks if the parsed page has the requested field populated.
func hasField(page *parsedPageData, field pageField) bool {
	switch field {
	case fieldBiography:
		return page.Biography != ""
	case fieldImage:
		return page.ImageURL != ""
	case fieldSimilar:
		return len(page.SimilarArtists) > 0
	default:
		return page.Biography != "" || page.ImageURL != "" || len(page.SimilarArtists) > 0
	}
}

// --- Capability methods ---

// GetArtistURL returns the Apple Music URL for the artist.
func (a *appleMusicAgent) GetArtistURL(input metadata.ArtistRequest) (*metadata.ArtistURLResponse, error) {
	if !isEnabled(configArtistURL) {
		return nil, nil
	}
	artistID, err := resolveArtistID(input.Name)
	if err != nil {
		return nil, err
	}
	if artistID == 0 {
		return nil, nil
	}

	countries := getCountries()
	artistURL := fmt.Sprintf("%s/%s/artist/-/%d", appleMusicBaseURL, countries[0], artistID)
	return &metadata.ArtistURLResponse{URL: artistURL}, nil
}

// GetArtistBiography returns the artist biography from Apple Music.
func (a *appleMusicAgent) GetArtistBiography(input metadata.ArtistRequest) (*metadata.ArtistBiographyResponse, error) {
	if !isEnabled(configArtistBiography) {
		return nil, nil
	}
	artistID, err := resolveArtistID(input.Name)
	if err != nil {
		return nil, err
	}
	if artistID == 0 {
		return nil, nil
	}

	page, err := fetchArtistPage(artistID, fieldBiography)
	if err != nil {
		return nil, err
	}
	if page == nil || page.Biography == "" || strings.Contains(page.Biography, "Apple Music") {
		pdk.Log(pdk.LogDebug, "no biography found for: "+input.Name)
		return nil, nil
	}

	return &metadata.ArtistBiographyResponse{Biography: page.Biography}, nil
}

// GetArtistImages returns artist images from Apple Music in multiple sizes.
func (a *appleMusicAgent) GetArtistImages(input metadata.ArtistRequest) (*metadata.ArtistImagesResponse, error) {
	if !isEnabled(configArtistImages) {
		return nil, nil
	}
	artistID, err := resolveArtistID(input.Name)
	if err != nil {
		return nil, err
	}
	if artistID == 0 {
		return nil, nil
	}

	page, err := fetchArtistPage(artistID, fieldImage)
	if err != nil {
		return nil, err
	}
	if page == nil || page.ImageURL == "" || isPlaceholderImage(page.ImageURL) {
		pdk.Log(pdk.LogDebug, "no artist image found for: "+input.Name)
		return nil, nil
	}

	return &metadata.ArtistImagesResponse{Images: buildImageList(page.ImageURL)}, nil
}

// GetSimilarArtists returns similar artists scraped from the Apple Music page.
func (a *appleMusicAgent) GetSimilarArtists(input metadata.SimilarArtistsRequest) (*metadata.SimilarArtistsResponse, error) {
	if !isEnabled(configSimilarArtists) {
		return nil, nil
	}
	artistID, err := resolveArtistID(input.Name)
	if err != nil {
		return nil, err
	}
	if artistID == 0 {
		return nil, nil
	}

	page, err := fetchArtistPage(artistID, fieldSimilar)
	if err != nil {
		return nil, err
	}
	if page == nil || len(page.SimilarArtists) == 0 {
		pdk.Log(pdk.LogDebug, "no similar artists found for: "+input.Name)
		return nil, nil
	}

	limit := clampLimit(int(input.Limit), len(page.SimilarArtists))

	artists := make([]metadata.ArtistRef, 0, limit)
	for i := 0; i < limit; i++ {
		artists = append(artists, metadata.ArtistRef{
			Name: page.SimilarArtists[i].Name,
		})
	}

	return &metadata.SimilarArtistsResponse{Artists: artists}, nil
}

// GetArtistTopSongs returns the artist's top songs via the iTunes Lookup API.
func (a *appleMusicAgent) GetArtistTopSongs(input metadata.TopSongsRequest) (*metadata.TopSongsResponse, error) {
	if !isEnabled(configTopSongs) {
		return nil, nil
	}
	artistID, err := resolveArtistID(input.Name)
	if err != nil {
		return nil, err
	}
	if artistID == 0 {
		return nil, nil
	}

	count := int(input.Count)
	if count <= 0 {
		count = defaultTopSongs
	}

	// Check cache
	cacheKey := fmt.Sprintf("topsongs:%d:%d", artistID, count)
	var cached metadata.TopSongsResponse
	if kvGet(cacheKey, &cached) {
		pdk.Log(pdk.LogDebug, "top songs cache hit: "+cacheKey)
		return &cached, nil
	}

	// Fetch from iTunes Lookup API
	lookupURL := fmt.Sprintf("%s?id=%d&entity=song&sort=popular&limit=%d",
		iTunesLookupURL, artistID, count)

	pdk.Log(pdk.LogDebug, "fetching top songs: "+lookupURL)

	var lookupResp itunesLookupResponse
	if err := httpGetJSON(lookupURL, &lookupResp); err != nil {
		return nil, fmt.Errorf("iTunes top songs lookup: %w", err)
	}

	// First result is the artist itself, skip it
	songs := make([]metadata.SongRef, 0, len(lookupResp.Results))
	for _, r := range lookupResp.Results {
		if r.WrapperType == "track" && r.TrackName != "" {
			songs = append(songs, metadata.SongRef{
				Name:   r.TrackName,
				Artist: r.ArtistName,
			})
		}
	}

	if len(songs) == 0 {
		pdk.Log(pdk.LogDebug, "no top songs found for: "+input.Name)
		return nil, nil
	}

	result := &metadata.TopSongsResponse{Songs: songs}

	// Cache with TTL
	ttl := getCacheTTLSeconds()
	if err := kvSetWithTTL(cacheKey, result, ttl); err != nil {
		pdk.Log(pdk.LogWarn, "failed to cache top songs: "+err.Error())
	}

	return result, nil
}

// GetAlbumImages returns album artwork from Apple Music in multiple sizes.
func (a *appleMusicAgent) GetAlbumImages(input metadata.AlbumRequest) (*metadata.AlbumImagesResponse, error) {
	if !isEnabled(configAlbumImages) {
		return nil, nil
	}

	artworkURL, err := resolveAlbumArtwork(input.Name, input.Artist)
	if err != nil {
		return nil, err
	}
	if artworkURL == "" {
		pdk.Log(pdk.LogDebug, fmt.Sprintf("no album artwork found for '%s' by '%s'", input.Name, input.Artist))
		return nil, nil
	}

	return &metadata.AlbumImagesResponse{Images: buildImageList(artworkURL)}, nil
}
