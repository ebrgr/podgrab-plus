package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/akhilrex/podgrab/db"
	"github.com/bogem/id3v2/v2"
)

const navidromeClientName = "podgrab"

var navidromeSyncMu sync.Mutex

var defaultID3MetadataAttrs = []string{"title", "artist", "album", "genre", "date"}

// PostDownloadConfig is stored in CONFIG/post-download.json. A podcast is
// matched by its Podgrab UUID, so filenames and folder names are irrelevant.
type PostDownloadConfig struct {
	Podcasts map[string]PostDownloadPodcastConfig `json:"podcasts"`
}

type PostDownloadPodcastConfig struct {
	PodgrabID        json.RawMessage `json:"podgrab_id"`
	AlbumID          string          `json:"album_id,omitempty"`
	AlbumName        string          `json:"album_name,omitempty"`
	PlaylistID       string          `json:"playlist_id,omitempty"`
	PlaylistName     string          `json:"playlist_name,omitempty"`
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
	Status        string                 `json:"status"`
	Error         *subsonicAPIError      `json:"error,omitempty"`
	Album         subsonicAlbum          `json:"album"`
	Playlist      subsonicPlaylist       `json:"playlist"`
	SearchResult3 subsonicSearchResult3  `json:"searchResult3"`
	Playlists     subsonicPlaylistResult `json:"playlists"`
}

type subsonicAPIError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type subsonicAlbum struct {
	ID     string         `json:"id"`
	Name   string         `json:"name"`
	Artist string         `json:"artist"`
	Songs  []subsonicSong `json:"song"`
}

type subsonicSearchResult3 struct {
	Albums []subsonicAlbum `json:"album"`
}

type subsonicPlaylistResult struct {
	Playlists []subsonicPlaylist `json:"playlist"`
}

type subsonicPlaylist struct {
	ID      string         `json:"id"`
	Name    string         `json:"name"`
	Entries []subsonicSong `json:"entry"`
}

type subsonicSong struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// ApplyPostDownloadMetadata writes ID3 before Navidrome can index the MP3.
// A missing config or a podcast absent from the config is intentionally a no-op.
func ApplyPostDownloadMetadata(item *db.PodcastItem, mediaPath string, imagePath string) error {
	setting := db.GetOrCreateSetting()
	if !setting.EditID3Tags {
		return nil
	}
	showConfig, _, err := postDownloadPodcastConfig(item.PodcastID)
	if err != nil {
		return err
	}
	showConfig.MetadataAttrs = effectiveMetadataAttrs(showConfig.MetadataAttrs)

	metadata, err := metadataForEpisode(item, showConfig)
	if err != nil {
		return err
	}
	return writeEpisodeID3(mediaPath, showConfig.MetadataAttrs, metadata, imagePath)
}

// NotifyNavidromeAfterDownload asks Navidrome to scan and then rebuilds the
// configured playlist. It is designed to run in a goroutine after the episode
// has been marked as downloaded.
func NotifyNavidromeAfterDownload(item *db.PodcastItem) error {
	setting := db.GetOrCreateSetting()
	if !setting.UpdateNavidrome {
		return nil
	}

	// Downloads may finish concurrently. Serializing discovery prevents two
	// goroutines from creating the same playlist or overwriting the config file.
	navidromeSyncMu.Lock()
	defer navidromeSyncMu.Unlock()

	config, configPath, configKey, showConfig, enabled, err := loadPostDownloadPodcastConfig(item.PodcastID)
	if err != nil {
		return err
	}
	if !enabled {
		configKey = item.PodcastID
		showConfig = defaultPostDownloadPodcastConfig(item)
	}
	if showConfig.AlbumName == "" {
		showConfig.AlbumName = item.Podcast.Title
	}
	if showConfig.PlaylistName == "" {
		showConfig.PlaylistName = "Últimos episódios - " + item.Podcast.Title
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
	album, err := client.waitForAlbum(showConfig, item, wait, poll)
	if err != nil {
		return err
	}
	showConfig.AlbumID = album.ID

	playlist, err := client.findOrCreatePlaylist(showConfig)
	if err != nil {
		return err
	}
	showConfig.PlaylistID = playlist.ID
	config.Podcasts[configKey] = showConfig
	if err := savePostDownloadConfig(configPath, config); err != nil {
		return err
	}

	return client.rebuildPlaylist(showConfig, item.PodcastID, album)
}

func postDownloadPodcastConfig(podcastID string) (PostDownloadPodcastConfig, bool, error) {
	_, _, _, showConfig, enabled, err := loadPostDownloadPodcastConfig(podcastID)
	return showConfig, enabled, err
}

func loadPostDownloadPodcastConfig(podcastID string) (PostDownloadConfig, string, string, PostDownloadPodcastConfig, bool, error) {
	config := PostDownloadConfig{Podcasts: make(map[string]PostDownloadPodcastConfig)}
	configPath := os.Getenv("POST_DOWNLOAD_CONFIG")
	if configPath == "" {
		configPath = path.Join(os.Getenv("CONFIG"), "post-download.json")
	}
	file, err := os.Open(configPath)
	if errors.Is(err, os.ErrNotExist) && os.Getenv("POST_DOWNLOAD_CONFIG") == "" {
		// Backwards-compatible path used by the original one-shot command.
		legacyPath := path.Join(os.Getenv("CONFIG"), "cousin-iddd.config.json")
		file, err = os.Open(legacyPath)
		if err == nil {
			configPath = legacyPath
		}
	}
	if errors.Is(err, os.ErrNotExist) {
		return config, configPath, "", PostDownloadPodcastConfig{}, false, nil
	}
	if err != nil {
		return config, configPath, "", PostDownloadPodcastConfig{}, false, err
	}
	defer file.Close()
	if err := json.NewDecoder(file).Decode(&config); err != nil {
		return config, configPath, "", PostDownloadPodcastConfig{}, false, fmt.Errorf("invalid post-download config: %w", err)
	}
	if config.Podcasts == nil {
		config.Podcasts = make(map[string]PostDownloadPodcastConfig)
	}
	for key, showConfig := range config.Podcasts {
		id, err := rawConfigID(showConfig.PodgrabID)
		if err != nil {
			return config, configPath, "", PostDownloadPodcastConfig{}, false, err
		}
		if id == podcastID {
			return config, configPath, key, showConfig, true, nil
		}
	}
	return config, configPath, "", PostDownloadPodcastConfig{}, false, nil
}

func defaultPostDownloadPodcastConfig(item *db.PodcastItem) PostDownloadPodcastConfig {
	return PostDownloadPodcastConfig{
		PodgrabID:     json.RawMessage(strconv.Quote(item.PodcastID)),
		AlbumName:     item.Podcast.Title,
		PlaylistName:  "Últimos episódios - " + item.Podcast.Title,
		MetadataAttrs: append([]string(nil), defaultID3MetadataAttrs...),
		PlaylistSize:  17,
	}
}

func savePostDownloadConfig(configPath string, config PostDownloadConfig) error {
	if configPath == "" {
		return errors.New("post-download config path is empty")
	}
	if err := os.MkdirAll(path.Dir(configPath), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := ioutil.TempFile(path.Dir(configPath), ".post-download-*.json")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, configPath)
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

func writeEpisodeID3(mediaPath string, attrs []string, metadata episodeMetadata, imagePath string) error {
	tag, err := id3v2.Open(mediaPath, id3v2.Options{Parse: true})
	if err != nil {
		return err
	}
	defer tag.Close()
	encoding := id3v2.EncodingUTF8
	if tag.Version() == 3 {
		// ID3v2.3 does not support UTF-8. UTF-16 preserves characters such as
		// en dashes, stars and non-Latin alphabets without converting the tag.
		encoding = id3v2.EncodingUTF16
	}
	tag.SetDefaultEncoding(encoding)
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
		tag.AddTextFrame(id, encoding, metadata.TrackNumber)
	}
	if enabled["date"] {
		writeID3Date(tag, encoding, metadata.Date)
	}
	if imagePath != "" {
		if err := writeID3Cover(tag, encoding, imagePath); err != nil {
			return err
		}
	}
	return tag.Save()
}

func writeID3Cover(tag *id3v2.Tag, _ id3v2.Encoding, imagePath string) error {
	picture, err := ioutil.ReadFile(imagePath)
	if err != nil {
		return err
	}
	if len(picture) == 0 {
		return errors.New("episode image is empty")
	}
	mimeType := mime.TypeByExtension(strings.ToLower(filepath.Ext(imagePath)))
	if !strings.HasPrefix(mimeType, "image/") {
		mimeType = "image/jpeg"
	}
	tag.DeleteFrames(tag.CommonID("Attached picture"))
	tag.AddAttachedPicture(id3v2.PictureFrame{
		// Keep the APIC description empty and ISO-8859-1. Some players do not
		// correctly skip the two-byte UTF-16 terminator and then see a leading
		// NUL byte before the JPEG header, making the embedded cover unreadable.
		Encoding:    id3v2.EncodingISO,
		MimeType:    mimeType,
		PictureType: id3v2.PTFrontCover,
		Description: "",
		Picture:     picture,
	})
	return nil
}

func writeID3Date(tag *id3v2.Tag, encoding id3v2.Encoding, value string) {
	parsed, err := time.Parse("2006-01-02T15:04:05Z", value)
	if tag.Version() == 3 {
		// ID3v2.3 represents the recording date in separate TYER, TDAT and
		// TIME frames. TDRC only exists in ID3v2.4.
		frames := map[string]string{
			"Year": parsed.Format("2006"),
			"Date": parsed.Format("0201"),
			"Time": parsed.Format("1504"),
		}
		if err != nil {
			frames = map[string]string{"Year": value}
		}
		for description, frameValue := range frames {
			id := tag.CommonID(description)
			tag.DeleteFrames(id)
			tag.AddTextFrame(id, encoding, frameValue)
		}
		return
	}
	id := tag.CommonID("Recording time")
	tag.DeleteFrames(id)
	tag.AddTextFrame(id, encoding, value)
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

func (client *navidromeClient) searchAlbum(name, artist string) (subsonicAlbum, bool, error) {
	values := url.Values{
		"query":       {name},
		"albumCount":  {"100"},
		"artistCount": {"0"},
		"songCount":   {"0"},
	}
	var envelope subsonicEnvelope
	if err := client.call("search3", values, &envelope); err != nil {
		return subsonicAlbum{}, false, err
	}
	var nameMatch *subsonicAlbum
	for index := range envelope.Response.SearchResult3.Albums {
		album := &envelope.Response.SearchResult3.Albums[index]
		if !strings.EqualFold(strings.TrimSpace(album.Name), strings.TrimSpace(name)) {
			continue
		}
		if nameMatch == nil {
			nameMatch = album
		}
		if artist == "" || strings.EqualFold(strings.TrimSpace(album.Artist), strings.TrimSpace(artist)) {
			return *album, true, nil
		}
	}
	if nameMatch != nil {
		return *nameMatch, true, nil
	}
	return subsonicAlbum{}, false, nil
}

func (client *navidromeClient) waitForAlbum(config PostDownloadPodcastConfig, item *db.PodcastItem, wait, poll time.Duration) (subsonicAlbum, error) {
	deadline := time.Now().Add(wait)
	var lastErr error
	for {
		if config.AlbumID != "" {
			album, err := client.getAlbum(config.AlbumID)
			if err == nil && albumHasTitle(album, item.Title) {
				return album, nil
			}
			lastErr = err
		} else {
			match, found, err := client.searchAlbum(config.AlbumName, item.Podcast.Author)
			lastErr = err
			if err == nil && found {
				album, getErr := client.getAlbum(match.ID)
				lastErr = getErr
				if getErr == nil && albumHasTitle(album, item.Title) {
					return album, nil
				}
			}
		}
		if time.Now().Add(poll).After(deadline) {
			if lastErr != nil {
				return subsonicAlbum{}, lastErr
			}
			return subsonicAlbum{}, fmt.Errorf("episode %q did not appear in Navidrome album %q after %s", item.Title, config.AlbumName, wait)
		}
		time.Sleep(poll)
	}
}

func (client *navidromeClient) getPlaylists() ([]subsonicPlaylist, error) {
	var envelope subsonicEnvelope
	if err := client.call("getPlaylists", nil, &envelope); err != nil {
		return nil, err
	}
	return envelope.Response.Playlists.Playlists, nil
}

func (client *navidromeClient) findOrCreatePlaylist(config PostDownloadPodcastConfig) (subsonicPlaylist, error) {
	playlists, err := client.getPlaylists()
	if err != nil {
		return subsonicPlaylist{}, err
	}
	for _, playlist := range playlists {
		if config.PlaylistID != "" && playlist.ID == config.PlaylistID {
			return playlist, nil
		}
		if strings.EqualFold(strings.TrimSpace(playlist.Name), strings.TrimSpace(config.PlaylistName)) {
			return playlist, nil
		}
	}
	values := url.Values{"name": {config.PlaylistName}}
	var envelope subsonicEnvelope
	if err := client.call("createPlaylist", values, &envelope); err != nil {
		return subsonicPlaylist{}, err
	}
	if envelope.Response.Playlist.ID == "" {
		return subsonicPlaylist{}, errors.New("Navidrome created a playlist without returning its ID")
	}
	return envelope.Response.Playlist, nil
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
func effectiveMetadataAttrs(values []string) []string {
	if len(values) == 0 {
		return append([]string(nil), defaultID3MetadataAttrs...)
	}
	return values
}
func envDuration(key string, fallback time.Duration) time.Duration {
	if value := os.Getenv(key); value != "" {
		if parsed, err := time.ParseDuration(value); err == nil {
			return parsed
		}
	}
	return fallback
}
