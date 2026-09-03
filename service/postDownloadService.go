package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/akhilrex/podgrab/db"
	"github.com/bogem/id3v2/v2"
)

const navidromeClientName = "podgrab"

// PostDownloadConfig is stored in CONFIG/post-download.json. A podcast is
// matched by its Podgrab UUID, so filenames and folder names are irrelevant.
type PostDownloadConfig struct {
	Podcasts map[string]PostDownloadPodcastConfig `json:"podcasts"`
}

type PostDownloadPodcastConfig struct {
	PodgrabID        json.RawMessage `json:"podgrab_id"`
	AlbumID          string          `json:"album_id"`
	PlaylistID       string          `json:"playlist_id"`
	MetadataAttrs    []string        `json:"metadata_attributes"`
	FilteredPrefixes []string        `json:"filtered_prefixes"`
	EpisodePrefix    *EpisodePrefix  `json:"episode_prefix"`
	PlaylistSize     int             `json:"playlist_size"`
}

type EpisodePrefix struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

type episodeMetadata struct {
	Title       string
	Artist      string
	Album       string
	TrackNumber string
	Genre       string
	Date        string
}

type subsonicEnvelope struct {
	Response subsonicResponse `json:"subsonic-response"`
}

type subsonicResponse struct {
	Status   string            `json:"status"`
	Error    *subsonicAPIError `json:"error,omitempty"`
	Album    subsonicAlbum     `json:"album"`
	Playlist subsonicPlaylist  `json:"playlist"`
}

type subsonicAPIError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type subsonicAlbum struct {
	Songs []subsonicSong `json:"song"`
}

type subsonicPlaylist struct {
	Name    string         `json:"name"`
	Entries []subsonicSong `json:"entry"`
}

type subsonicSong struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// ApplyPostDownloadMetadata writes ID3 before Navidrome can index the MP3.
// A missing config or a podcast absent from the config is intentionally a no-op.
func ApplyPostDownloadMetadata(item *db.PodcastItem, mediaPath string) error {
	setting := db.GetOrCreateSetting()
	if !setting.EditID3Tags {
		return nil
	}
	showConfig, enabled, err := postDownloadPodcastConfig(item.PodcastID)
	if err != nil {
		return err
	}
	if !enabled {
		showConfig.MetadataAttrs = []string{"title", "artist", "album", "genre", "date"}
	}

	metadata, err := metadataForEpisode(item, showConfig)
	if err != nil {
		return err
	}
	return writeEpisodeID3(mediaPath, showConfig.MetadataAttrs, metadata)
}

// NotifyNavidromeAfterDownload asks Navidrome to scan and then rebuilds the
// configured playlist. It is designed to run in a goroutine after the episode
// has been marked as downloaded.
func NotifyNavidromeAfterDownload(item *db.PodcastItem) error {
	setting := db.GetOrCreateSetting()
	if !setting.UpdateNavidrome {
		return nil
	}
	showConfig, enabled, err := postDownloadPodcastConfig(item.PodcastID)
	if err != nil || !enabled || showConfig.AlbumID == "" || showConfig.PlaylistID == "" {
		return err
	}

	client, err := newNavidromeClient(setting)
	if err != nil {
		return err
	}
	// startScan is best-effort: older Subsonic-compatible servers may not expose it.
	if err := client.call("startScan", nil, &subsonicEnvelope{}); err != nil {
		Logger.Warnw("Navidrome scan could not be started", "episode", item.ID, "error", err)
	}

	wait := time.Duration(setting.NavidromeWaitSeconds) * time.Second
	if wait <= 0 {
		wait = envDuration("NAVIDROME_WAIT", 60*time.Second)
	}
	poll := time.Duration(setting.NavidromePollSeconds) * time.Second
	if poll <= 0 {
		poll = envDuration("NAVIDROME_POLL", 5*time.Second)
	}
	deadline := time.Now().Add(wait)
	for {
		album, albumErr := client.getAlbum(showConfig.AlbumID)
		if albumErr == nil && albumHasTitle(album, item.Title) {
			return client.rebuildPlaylist(showConfig, item.PodcastID, album)
		}
		if time.Now().Add(poll).After(deadline) {
			if albumErr != nil {
				return albumErr
			}
			return fmt.Errorf("episode %q did not appear in Navidrome after %s", item.Title, wait)
		}
		time.Sleep(poll)
	}
}

func postDownloadPodcastConfig(podcastID string) (PostDownloadPodcastConfig, bool, error) {
	var config PostDownloadConfig
	configPath := os.Getenv("POST_DOWNLOAD_CONFIG")
	if configPath == "" {
		configPath = path.Join(os.Getenv("CONFIG"), "post-download.json")
	}
	file, err := os.Open(configPath)
	if errors.Is(err, os.ErrNotExist) && os.Getenv("POST_DOWNLOAD_CONFIG") == "" {
		// Backwards-compatible path used by the original one-shot command.
		file, err = os.Open(path.Join(os.Getenv("CONFIG"), "cousin-iddd.config.json"))
	}
	if errors.Is(err, os.ErrNotExist) {
		return PostDownloadPodcastConfig{}, false, nil
	}
	if err != nil {
		return PostDownloadPodcastConfig{}, false, err
	}
	defer file.Close()
	if err := json.NewDecoder(file).Decode(&config); err != nil {
		return PostDownloadPodcastConfig{}, false, fmt.Errorf("invalid post-download config: %w", err)
	}
	for _, showConfig := range config.Podcasts {
		id, err := rawConfigID(showConfig.PodgrabID)
		if err != nil {
			return PostDownloadPodcastConfig{}, false, err
		}
		if id == podcastID {
			return showConfig, true, nil
		}
	}
	return PostDownloadPodcastConfig{}, false, nil
}

func metadataForEpisode(item *db.PodcastItem, config PostDownloadPodcastConfig) (episodeMetadata, error) {
	metadata := episodeMetadata{
		Title:  item.Title,
		Artist: item.Podcast.Author,
		Album:  item.Podcast.Title,
		Genre:  "Podcast",
		Date:   item.PubDate.Format("2006-01-02T15:04:05Z"),
	}
	if config.EpisodePrefix != nil {
		current, ok := extractConfiguredEpisodeNumber(item.Title, *config.EpisodePrefix)
		if ok {
			var episodes []db.PodcastItem
			if err := db.GetAllPodcastItemsByPodcastId(item.PodcastID, &episodes); err != nil {
				return metadata, err
			}
			highest := current
			for _, episode := range episodes {
				if number, valid := extractConfiguredEpisodeNumber(episode.Title, *config.EpisodePrefix); valid && number > highest {
					highest = number
				}
			}
			metadata.TrackNumber = fmt.Sprintf("%d/%d", current, highest)
		}
	}
	return metadata, nil
}

func writeEpisodeID3(mediaPath string, attrs []string, metadata episodeMetadata) error {
	tag, err := id3v2.Open(mediaPath, id3v2.Options{Parse: true})
	if err != nil {
		return err
	}
	defer tag.Close()
	enabled := stringSet(attrs)
	if enabled["title"] {
		tag.SetTitle(metadata.Title)
	}
	if enabled["artist"] {
		tag.SetArtist(metadata.Artist)
	}
	if enabled["album"] {
		tag.SetAlbum(metadata.Album)
	}
	if enabled["genre"] {
		tag.SetGenre(metadata.Genre)
	}
	if enabled["track_num"] && metadata.TrackNumber != "" {
		id := tag.CommonID("Track number/Position in set")
		tag.DeleteFrames(id)
		tag.AddTextFrame(id, id3v2.EncodingUTF8, metadata.TrackNumber)
	}
	if enabled["date"] {
		id := tag.CommonID("Recording time")
		tag.DeleteFrames(id)
		tag.AddTextFrame(id, id3v2.EncodingUTF8, metadata.Date)
	}
	return tag.Save()
}

type navidromeClient struct {
	httpClient *http.Client
	baseURL    string
	username   string
	password   string
}

func newNavidromeClient(setting *db.Setting) (*navidromeClient, error) {
	host := strings.TrimRight(setting.NavidromeHost, "/")
	if host == "" {
		host = strings.TrimRight(os.Getenv("NAVIDROME_HOST"), "/")
	}
	if host == "" {
		return nil, errors.New("NAVIDROME_HOST is required for Navidrome integration")
	}
	if !strings.Contains(host, "://") {
		host = "http://" + host
	}
	username := setting.NavidromeUsername
	if username == "" {
		username = os.Getenv("NAVIDROME_USERNAME")
	}
	password := setting.NavidromePassword
	if password == "" {
		password = os.Getenv("NAVIDROME_PASSWORD")
	}
	return &navidromeClient{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		baseURL:    host,
		username:   username,
		password:   password,
	}, nil
}

func (client *navidromeClient) getAlbum(id string) (subsonicAlbum, error) {
	values := url.Values{"id": {id}}
	var envelope subsonicEnvelope
	if err := client.call("getAlbum", values, &envelope); err != nil {
		return subsonicAlbum{}, err
	}
	return envelope.Response.Album, nil
}

func (client *navidromeClient) rebuildPlaylist(config PostDownloadPodcastConfig, podcastID string, album subsonicAlbum) error {
	songsByTitle := make(map[string]string, len(album.Songs))
	for _, song := range album.Songs {
		songsByTitle[song.Title] = song.ID
	}
	var episodes []db.PodcastItem
	if err := db.GetAllPodcastItemsByPodcastId(podcastID, &episodes); err != nil {
		return err
	}
	sort.SliceStable(episodes, func(i, j int) bool {
		return episodes[i].PubDate.After(episodes[j].PubDate)
	})
	limit := config.PlaylistSize
	if limit <= 0 {
		limit = 17
	}
	ids := make([]string, 0, limit)
	for _, episode := range episodes {
		if hasAnyPrefix(episode.Title, config.FilteredPrefixes) {
			continue
		}
		if id, exists := songsByTitle[episode.Title]; exists {
			ids = append(ids, id)
		}
		if len(ids) == limit {
			break
		}
	}
	values := url.Values{"playlistId": {config.PlaylistID}}
	for _, id := range ids {
		values.Add("songId", id)
	}
	var envelope subsonicEnvelope
	if err := client.call("createPlaylist", values, &envelope); err != nil {
		return err
	}
	if len(envelope.Response.Playlist.Entries) != len(ids) {
		return fmt.Errorf("Navidrome playlist contains %d songs; expected %d", len(envelope.Response.Playlist.Entries), len(ids))
	}
	return nil
}

func (client *navidromeClient) call(endpoint string, values url.Values, target *subsonicEnvelope) error {
	if values == nil {
		values = url.Values{}
	}
	values.Set("u", client.username)
	values.Set("p", client.password)
	values.Set("f", "json")
	values.Set("v", "1.16.1")
	values.Set("c", navidromeClientName)
	req, err := http.NewRequest(http.MethodGet, client.baseURL+"/rest/"+endpoint+"?"+values.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := client.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := ioutil.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("Navidrome %s: HTTP %s: %s", endpoint, resp.Status, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		return err
	}
	if target.Response.Status == "failed" {
		if target.Response.Error == nil {
			return errors.New("Navidrome returned a failed response")
		}
		return fmt.Errorf("Navidrome error %d: %s", target.Response.Error.Code, target.Response.Error.Message)
	}
	return nil
}

func extractConfiguredEpisodeNumber(title string, prefix EpisodePrefix) (int, bool) {
	if !strings.HasPrefix(title, prefix.Start) {
		return 0, false
	}
	rest := strings.TrimPrefix(title, prefix.Start)
	if prefix.End != "" {
		rest = strings.SplitN(rest, prefix.End, 2)[0]
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return 0, false
	}
	number, err := strconv.Atoi(fields[0])
	return number, err == nil
}

func rawConfigID(raw json.RawMessage) (string, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil
	}
	var number json.Number
	if json.Unmarshal(raw, &number) == nil {
		return number.String(), nil
	}
	return "", errors.New("post-download podgrab_id must be a string or number")
}

func albumHasTitle(album subsonicAlbum, title string) bool {
	for _, song := range album.Songs {
		if song.Title == title {
			return true
		}
	}
	return false
}
func hasAnyPrefix(value string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}
func stringSet(values []string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		result[value] = true
	}
	return result
}
func envDuration(key string, fallback time.Duration) time.Duration {
	if value := os.Getenv(key); value != "" {
		if parsed, err := time.ParseDuration(value); err == nil {
			return parsed
		}
	}
	return fallback
}
