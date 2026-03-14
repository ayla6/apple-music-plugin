package main

import (
	"encoding/json"
	"strings"

	"github.com/navidrome/navidrome/plugins/pdk/go/host"
	"github.com/navidrome/navidrome/plugins/pdk/go/pdk"
	"github.com/stretchr/testify/mock"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("appleMusicAgent", func() {
	BeforeEach(func() {
		pdk.ResetMock()
		host.ConfigMock.ExpectedCalls = nil
		host.ConfigMock.Calls = nil
		host.KVStoreMock.ExpectedCalls = nil
		host.KVStoreMock.Calls = nil
		host.HTTPMock.ExpectedCalls = nil
		host.HTTPMock.Calls = nil
	})

	Describe("getCountries", func() {
		It("returns default country when config not set", func() {
			host.ConfigMock.On("Get", "countries").Return("", false)
			Expect(getCountries()).To(Equal([]string{"us"}))
		})

		It("returns default country when config is empty", func() {
			host.ConfigMock.On("Get", "countries").Return("  ", true)
			Expect(getCountries()).To(Equal([]string{"us"}))
		})

		It("parses single country", func() {
			host.ConfigMock.On("Get", "countries").Return("br", true)
			Expect(getCountries()).To(Equal([]string{"br"}))
		})

		It("parses multiple countries with spaces", func() {
			host.ConfigMock.On("Get", "countries").Return(" br , us , de ", true)
			Expect(getCountries()).To(Equal([]string{"br", "us", "de"}))
		})

		It("normalizes to lowercase", func() {
			host.ConfigMock.On("Get", "countries").Return("BR,US", true)
			Expect(getCountries()).To(Equal([]string{"br", "us"}))
		})

		It("skips empty entries", func() {
			host.ConfigMock.On("Get", "countries").Return("br,,us,", true)
			Expect(getCountries()).To(Equal([]string{"br", "us"}))
		})
	})

	Describe("getCacheTTLSeconds", func() {
		It("returns default TTL when config not set", func() {
			host.ConfigMock.On("GetInt", "cache_ttl_days").Return(int64(0), false)
			Expect(getCacheTTLSeconds()).To(Equal(int64(7 * 24 * 60 * 60)))
		})

		It("returns default TTL when config is zero", func() {
			host.ConfigMock.On("GetInt", "cache_ttl_days").Return(int64(0), true)
			Expect(getCacheTTLSeconds()).To(Equal(int64(7 * 24 * 60 * 60)))
		})

		It("returns configured TTL in seconds", func() {
			host.ConfigMock.On("GetInt", "cache_ttl_days").Return(int64(14), true)
			Expect(getCacheTTLSeconds()).To(Equal(int64(14 * 24 * 60 * 60)))
		})
	})

	Describe("normalizeArtistName", func() {
		It("lowercases and trims", func() {
			Expect(normalizeArtistName("  Taylor Swift  ")).To(Equal("taylor swift"))
		})

		It("handles empty string", func() {
			Expect(normalizeArtistName("")).To(Equal(""))
		})
	})

	Describe("kvGetArtistID", func() {
		It("returns cached artist ID", func() {
			data, _ := json.Marshal(cachedArtistID{ArtistID: 12345})
			host.KVStoreMock.On("Get", "artist:test").Return(data, true, nil)
			result, ok := kvGetArtistID("artist:test")
			Expect(ok).To(BeTrue())
			Expect(result.ArtistID).To(Equal(int64(12345)))
		})

		It("returns false when key not found", func() {
			host.KVStoreMock.On("Get", "artist:missing").Return([]byte(nil), false, nil)
			_, ok := kvGetArtistID("artist:missing")
			Expect(ok).To(BeFalse())
		})

		It("returns false on invalid JSON", func() {
			host.KVStoreMock.On("Get", "artist:bad").Return([]byte("invalid"), true, nil)
			_, ok := kvGetArtistID("artist:bad")
			Expect(ok).To(BeFalse())
		})
	})

	Describe("kvSet", func() {
		It("marshals and stores value", func() {
			expected, _ := json.Marshal(cachedArtistID{ArtistID: 999})
			host.KVStoreMock.On("Set", "key", expected).Return(nil)
			err := kvSet("key", cachedArtistID{ArtistID: 999})
			Expect(err).ToNot(HaveOccurred())
			host.KVStoreMock.AssertCalled(GinkgoT(), "Set", "key", expected)
		})
	})

	Describe("kvSetWithTTL", func() {
		It("marshals and stores value with TTL", func() {
			expected, _ := json.Marshal(cachedArtistID{ArtistID: 999})
			host.KVStoreMock.On("SetWithTTL", "key", expected, int64(3600)).Return(nil)
			err := kvSetWithTTL("key", cachedArtistID{ArtistID: 999}, 3600)
			Expect(err).ToNot(HaveOccurred())
			host.KVStoreMock.AssertCalled(GinkgoT(), "SetWithTTL", "key", expected, int64(3600))
		})
	})

	Describe("httpGet", func() {
		It("sends GET request with user agent", func() {
			host.HTTPMock.On("Send", mock.MatchedBy(func(req host.HTTPRequest) bool {
				return req.Method == "GET" &&
					req.URL == "https://example.com/test" &&
					req.Headers["User-Agent"] == userAgent &&
					req.TimeoutMs == httpTimeoutMs
			})).Return(&host.HTTPResponse{
				StatusCode: 200,
				Body:       []byte("response"),
			}, nil)

			body, status, err := httpGet("https://example.com/test")
			Expect(err).ToNot(HaveOccurred())
			Expect(status).To(Equal(int32(200)))
			Expect(body).To(Equal([]byte("response")))
		})
	})

	Describe("findBestArtistMatch", func() {
		It("returns exact case-insensitive match", func() {
			results := []itunesArtistResult{
				{WrapperType: "artist", ArtistName: "Taylor", ArtistID: 1},
				{WrapperType: "artist", ArtistName: "Taylor Swift", ArtistID: 2},
			}
			match := findBestArtistMatch("taylor swift", results)
			Expect(match).ToNot(BeNil())
			Expect(match.ArtistID).To(Equal(int64(2)))
		})

		It("falls back to first artist when no exact match", func() {
			results := []itunesArtistResult{
				{WrapperType: "collection", ArtistName: "Album", ArtistID: 1},
				{WrapperType: "artist", ArtistName: "Some Artist", ArtistID: 2},
			}
			match := findBestArtistMatch("query", results)
			Expect(match).ToNot(BeNil())
			Expect(match.ArtistID).To(Equal(int64(2)))
		})

		It("skips non-artist results", func() {
			results := []itunesArtistResult{
				{WrapperType: "collection", ArtistName: "Taylor Swift", ArtistID: 1},
			}
			match := findBestArtistMatch("Taylor Swift", results)
			Expect(match).To(BeNil())
		})

		It("returns nil for empty results", func() {
			match := findBestArtistMatch("anything", nil)
			Expect(match).To(BeNil())
		})
	})

	Describe("resolveArtistID", func() {
		BeforeEach(func() {
			pdk.PDKMock.On("Log", mock.Anything, mock.Anything).Maybe()
		})

		It("returns cached artist ID", func() {
			data, _ := json.Marshal(cachedArtistID{ArtistID: 159260351})
			host.KVStoreMock.On("Get", "artist:taylor swift").Return(data, true, nil)
			host.ConfigMock.On("Get", "countries").Return("us", true)

			id, err := resolveArtistID("Taylor Swift")
			Expect(err).ToNot(HaveOccurred())
			Expect(id).To(Equal(int64(159260351)))
		})

		It("searches iTunes API on cache miss", func() {
			host.KVStoreMock.On("Get", "artist:taylor swift").Return([]byte(nil), false, nil)
			host.ConfigMock.On("Get", "countries").Return("us", true)

			searchResp := itunesSearchResponse{
				ResultCount: 1,
				Results: []itunesArtistResult{
					{WrapperType: "artist", ArtistName: "Taylor Swift", ArtistID: 159260351},
				},
			}
			respBody, _ := json.Marshal(searchResp)
			host.HTTPMock.On("Send", mock.MatchedBy(func(req host.HTTPRequest) bool {
				return req.Method == "GET" && strings.Contains(req.URL, "itunes.apple.com/search")
			})).Return(&host.HTTPResponse{StatusCode: 200, Body: respBody}, nil)

			cachedData, _ := json.Marshal(cachedArtistID{ArtistID: 159260351})
			host.KVStoreMock.On("Set", "artist:taylor swift", cachedData).Return(nil)

			id, err := resolveArtistID("Taylor Swift")
			Expect(err).ToNot(HaveOccurred())
			Expect(id).To(Equal(int64(159260351)))
		})

		It("returns error for empty artist name", func() {
			_, err := resolveArtistID("")
			Expect(err).To(MatchError("empty artist name"))
		})

		It("returns error when no results found", func() {
			host.KVStoreMock.On("Get", "artist:unknown").Return([]byte(nil), false, nil)
			host.ConfigMock.On("Get", "countries").Return("us", true)

			searchResp := itunesSearchResponse{ResultCount: 0, Results: nil}
			respBody, _ := json.Marshal(searchResp)
			host.HTTPMock.On("Send", mock.Anything).Return(&host.HTTPResponse{StatusCode: 200, Body: respBody}, nil)

			_, err := resolveArtistID("Unknown")
			Expect(err).To(MatchError("no artist found"))
		})
	})

	Describe("parseJSONLD", func() {
		It("extracts JSON-LD data from HTML", func() {
			html := `<html><head><script type="application/ld+json">{"@type":"MusicGroup","name":"Taylor Swift","description":"A bio","image":"https://example.com/img.jpg"}</script></head></html>`
			ld, err := parseJSONLD(html)
			Expect(err).ToNot(HaveOccurred())
			Expect(ld.Name).To(Equal("Taylor Swift"))
			Expect(ld.Description).To(Equal("A bio"))
			Expect(ld.Image).To(Equal("https://example.com/img.jpg"))
		})

		It("returns error when no JSON-LD found", func() {
			_, err := parseJSONLD("<html><head></head></html>")
			Expect(err).To(MatchError("no JSON-LD found"))
		})

		It("returns error for malformed JSON-LD", func() {
			html := `<html><script type="application/ld+json">{invalid`
			_, err := parseJSONLD(html)
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("parseOpenGraphImage", func() {
		It("extracts og:image URL", func() {
			html := `<html><meta property="og:image" content="https://example.com/og.jpg"></html>`
			Expect(parseOpenGraphImage(html)).To(Equal("https://example.com/og.jpg"))
		})

		It("returns empty when no og:image found", func() {
			Expect(parseOpenGraphImage("<html></html>")).To(BeEmpty())
		})
	})

	Describe("rewriteImageSize", func() {
		It("rewrites dimension segment", func() {
			url := "https://is1-ssl.mzstatic.com/image/thumb/Music116/v4/ab/cd/ef/abcdef-12345/486x486bb.jpg"
			result := rewriteImageSize(url, 1000)
			Expect(result).To(ContainSubstring("/1000x1000bb."))
			Expect(result).ToNot(ContainSubstring("486x486"))
		})

		It("handles URLs without dimension segment", func() {
			url := "https://example.com/image.jpg"
			Expect(rewriteImageSize(url, 300)).To(Equal(url))
		})
	})

	Describe("parseSimilarArtists", func() {
		It("extracts artists from section with aria-labels", func() {
			html := `<html><section class="svelte-abc">` +
				`<div aria-label="Similar Artists">` +
				`<div aria-label="Ed Sheeran, "><a href="/us/artist/ed-sheeran/183313439"></a></div>` +
				`<div aria-label="Adele, "><a href="/us/artist/adele/262836961"></a></div>` +
				`</div></section></html>`
			artists := parseSimilarArtists(html)
			Expect(artists).To(HaveLen(2))
			Expect(artists[0].Name).To(Equal("Ed Sheeran"))
			Expect(artists[1].Name).To(Equal("Adele"))
		})

		It("returns nil when no section found", func() {
			Expect(parseSimilarArtists("<html></html>")).To(BeNil())
		})

		It("deduplicates artist names", func() {
			html := `<html><section class="svelte-abc">` +
				`<div aria-label="Similar Artists">` +
				`<div aria-label="Ed Sheeran, "></div>` +
				`<div aria-label="Ed Sheeran, "></div>` +
				`</div></section></html>`
			artists := parseSimilarArtists(html)
			Expect(artists).To(HaveLen(1))
		})
	})
})
