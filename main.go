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
	userAgent         = "NavidromeAppleMusicPlugin/0.1"
	defaultCountry    = "us"
	defaultCacheTTL   = 7 // days
	httpTimeoutMs     = 10000
	iTunesSearchURL   = "https://itunes.apple.com/search"
	iTunesLookupURL   = "https://itunes.apple.com/lookup"
	appleMusicBaseURL = "https://music.apple.com"
)

// Compile-time interface assertions (will compile after all methods are added in Tasks 11-15)
var (
	_ metadata.ArtistURLProvider       = (*appleMusicAgent)(nil)
	_ metadata.ArtistBiographyProvider = (*appleMusicAgent)(nil)
	_ metadata.ArtistImagesProvider    = (*appleMusicAgent)(nil)
	_ metadata.SimilarArtistsProvider  = (*appleMusicAgent)(nil)
	_ metadata.ArtistTopSongsProvider  = (*appleMusicAgent)(nil)
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
	WrapperType   string `json:"wrapperType"`
	ArtistType    string `json:"artistType"`
	ArtistName    string `json:"artistName"`
	ArtistLinkURL string `json:"artistLinkUrl"`
	ArtistID      int64  `json:"artistId"`
	PrimaryGenre  string `json:"primaryGenreName"`
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

// --- JSON-LD structure ---

type jsonLDData struct {
	Context     string `json:"@context"`
	Type        string `json:"@type"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Image       string `json:"image"`
}
