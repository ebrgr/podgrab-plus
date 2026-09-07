package service

import (
	"bytes"
	"encoding/json"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bogem/id3v2/v2"
)

func TestWriteEpisodeID3(t *testing.T) {
	mediaPath := filepath.Join(t.TempDir(), "episode.mp3")
	file, err := os.Create(mediaPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	original, err := id3v2.Open(mediaPath, id3v2.Options{Parse: true})
	if err != nil {
		t.Fatal(err)
	}
	original.SetVersion(3)
	original.SetTitle("placeholder")
	if err := original.Save(); err != nil {
		t.Fatal(err)
	}
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}

	metadata := episodeMetadata{
		Title:       "Episódio 735 – Pokémon versus Digimon ⭐",
		Artist:      "Podcast author",
		Album:       "Podcast name",
		TrackNumber: "2/10",
		Genre:       "Podcast",
		Date:        "2026-09-03T12:00:00Z",
	}
	attrs := []string{"title", "artist", "album", "track_num", "genre", "date"}
	coverPath := filepath.Join(filepath.Dir(mediaPath), "episode.jpg")
	cover := []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 0x4a, 0x46, 0x49, 0x46}
	if err := ioutil.WriteFile(coverPath, cover, 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeEpisodeID3(mediaPath, attrs, metadata, coverPath); err != nil {
		t.Fatal(err)
	}

	tag, err := id3v2.Open(mediaPath, id3v2.Options{Parse: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tag.Close()
	if tag.Title() != metadata.Title || tag.Artist() != metadata.Artist || tag.Album() != metadata.Album || tag.Genre() != metadata.Genre {
		t.Fatalf("unexpected ID3 values: title=%q artist=%q album=%q genre=%q", tag.Title(), tag.Artist(), tag.Album(), tag.Genre())
	}
	if tag.Version() != 3 || tag.GetTextFrame(tag.CommonID("Year")).Text != "2026" {
		t.Fatalf("expected ID3v2.3 date frames, got version=%d year=%q", tag.Version(), tag.GetTextFrame(tag.CommonID("Year")).Text)
	}
	pictures := tag.GetFrames(tag.CommonID("Attached picture"))
	if len(pictures) != 1 {
		t.Fatalf("expected one embedded cover, got %d", len(pictures))
	}
	picture, ok := pictures[0].(id3v2.PictureFrame)
	if !ok || picture.MimeType != "image/jpeg" || string(picture.Picture) != string(cover) {
		t.Fatalf("unexpected embedded cover: %#v", pictures[0])
	}
}

func TestWriteEpisodeID3ReplacesExistingCover(t *testing.T) {
	mediaPath := filepath.Join(t.TempDir(), "episode.mp3")
	if err := ioutil.WriteFile(mediaPath, []byte("audio payload"), 0600); err != nil {
		t.Fatal(err)
	}

	oldCoverPath := filepath.Join(filepath.Dir(mediaPath), "old.jpg")
	newCoverPath := filepath.Join(filepath.Dir(mediaPath), "new.jpg")
	oldCover := []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 0x4a, 0x46, 0x49, 0x46, 0x01}
	newCover := []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 0x4a, 0x46, 0x49, 0x46, 0x02}
	if err := ioutil.WriteFile(oldCoverPath, oldCover, 0600); err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(newCoverPath, newCover, 0600); err != nil {
		t.Fatal(err)
	}

	metadata := episodeMetadata{Title: "Episode", Artist: "Author", Album: "Podcast", Genre: "Podcast"}
	if err := writeEpisodeID3(mediaPath, defaultID3MetadataAttrs, metadata, oldCoverPath); err != nil {
		t.Fatal(err)
	}
	if err := writeEpisodeID3(mediaPath, defaultID3MetadataAttrs, metadata, newCoverPath); err != nil {
		t.Fatal(err)
	}

	tag, err := id3v2.Open(mediaPath, id3v2.Options{Parse: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tag.Close()
	pictures := tag.GetFrames(tag.CommonID("Attached picture"))
	if len(pictures) != 1 {
		t.Fatalf("expected one embedded cover, got %d", len(pictures))
	}
	picture, ok := pictures[0].(id3v2.PictureFrame)
	if !ok {
		t.Fatalf("expected PictureFrame, got %T", pictures[0])
	}
	if !picture.Encoding.Equals(id3v2.EncodingISO) || picture.Description != "" {
		t.Fatalf("expected an empty ISO-8859-1 APIC description, got encoding=%v description=%q", picture.Encoding, picture.Description)
	}
	if !bytes.Equal(picture.Picture, newCover) {
		t.Fatal("expected the new cover to replace the existing one")
	}
}

func TestEmptyMetadataAttributesUseDefaults(t *testing.T) {
	attrs := effectiveMetadataAttrs(nil)
	if len(attrs) != len(defaultID3MetadataAttrs) {
		t.Fatalf("expected %d default attributes, got %v", len(defaultID3MetadataAttrs), attrs)
	}
}

func TestEpisodeImagePathUsesAudioBaseName(t *testing.T) {
	path := episodeImagePath("/media/My Podcast/episode-42.mp3", "https://images.example/cover.webp?size=large", "image/webp")
	if path != "/media/My Podcast/episode-42.webp" {
		t.Fatalf("unexpected image sidecar path: %s", path)
	}
}

func TestSearchAlbumMatchesNameAndArtist(t *testing.T) {
	client := testNavidromeClient(func(request *http.Request) string {
		if request.URL.Path != "/rest/search3" {
			t.Fatalf("unexpected endpoint: %s", request.URL.Path)
		}
		return `{"subsonic-response":{"status":"ok","searchResult3":{"album":[{"id":"wrong","name":"My Podcast","artist":"Other"},{"id":"wanted","name":"My Podcast","artist":"Host"}]}}}`
	})

	album, found, err := client.searchAlbum("My Podcast", "Host")
	if err != nil {
		t.Fatal(err)
	}
	if !found || album.ID != "wanted" {
		t.Fatalf("expected album wanted, got found=%v album=%+v", found, album)
	}
}

func TestFindOrCreatePlaylistCreatesMissingPlaylist(t *testing.T) {
	created := false
	client := testNavidromeClient(func(request *http.Request) string {
		switch request.URL.Path {
		case "/rest/getPlaylists":
			return `{"subsonic-response":{"status":"ok","playlists":{"playlist":[]}}}`
		case "/rest/createPlaylist":
			created = request.URL.Query().Get("name") == "Latest - My Podcast"
			return `{"subsonic-response":{"status":"ok","playlist":{"id":"new-playlist","name":"Latest - My Podcast"}}}`
		default:
			t.Fatalf("unexpected endpoint: %s", request.URL.Path)
		}
		return ""
	})

	playlist, err := client.findOrCreatePlaylist(PostDownloadPodcastConfig{PlaylistName: "Latest - My Podcast"})
	if err != nil {
		t.Fatal(err)
	}
	if !created || playlist.ID != "new-playlist" {
		t.Fatalf("playlist was not created correctly: created=%v playlist=%+v", created, playlist)
	}
}

func TestSaveAndLoadDiscoveredIDs(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "post-download.json")
	previous, hadPrevious := os.LookupEnv("POST_DOWNLOAD_CONFIG")
	os.Setenv("POST_DOWNLOAD_CONFIG", configPath)
	defer func() {
		if hadPrevious {
			os.Setenv("POST_DOWNLOAD_CONFIG", previous)
		} else {
			os.Unsetenv("POST_DOWNLOAD_CONFIG")
		}
	}()

	podcastID := "podcast-uuid"
	config := PostDownloadConfig{Podcasts: map[string]PostDownloadPodcastConfig{
		podcastID: {
			PodgrabID:    json.RawMessage(`"podcast-uuid"`),
			AlbumID:      "album-id",
			PlaylistID:   "playlist-id",
			AlbumName:    "My Podcast",
			PlaylistName: "Latest - My Podcast",
		},
	}}
	if err := savePostDownloadConfig(configPath, config); err != nil {
		t.Fatal(err)
	}
	loaded, enabled, err := postDownloadPodcastConfig(podcastID)
	if err != nil {
		t.Fatal(err)
	}
	if !enabled || loaded.AlbumID != "album-id" || loaded.PlaylistID != "playlist-id" {
		t.Fatalf("discovered IDs were not persisted: enabled=%v config=%+v", enabled, loaded)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func testNavidromeClient(response func(*http.Request) string) *navidromeClient {
	httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       ioutil.NopCloser(strings.NewReader(response(request))),
			Request:    request,
		}, nil
	})}
	return &navidromeClient{
		httpClient: httpClient,
		baseURL:    "http://navidrome.test",
		username:   "user",
		password:   "password",
	}
}
