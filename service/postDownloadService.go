package service

import (
	"bytes"
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

	"github.com/bogem/id3v2/v2"
	"github.com/ebrgr/podgrab-plus/db"
)

const navidromeClientName = "podgrab"

var navidromeSyncMu sync.Mutex

var id3CheckMu sync.Mutex
var id3ChecksInProgress = make(map[string]bool)

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
		showConfig.PlaylistName = "Last episodes - " + item.Podcast.Title
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
	if errors.Is(err, id3v2.ErrUnsupportedVersion) {
		// Some podcast publishers still distribute ID3v2.2 tags. The ID3
		// library supports v2.3 and v2.4 only, so preserve the audio while
		// replacing the legacy tag with a new supported tag.
		if tag != nil {
			_ = tag.Close()
		}
		if err := removeUnsupportedID3Tag(mediaPath); err != nil {
			return fmt.Errorf("replace unsupported ID3 tag: %w", err)
		}
		tag, err = id3v2.Open(mediaPath, id3v2.Options{Parse: true})
	}
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

// removeUnsupportedID3Tag removes an ID3v2.0, v2.1, or v2.2 header and its
// payload. The original file is replaced only after the audio payload has been
// copied to a temporary file successfully.
func removeUnsupportedID3Tag(mediaPath string) error {
	source, err := os.Open(mediaPath)
	if err != nil {
		return err
	}

	info, err := source.Stat()
	if err != nil {
		source.Close()
		return err
	}
	header := make([]byte, 10)
	if _, err := io.ReadFull(source, header); err != nil {
		source.Close()
		return err
	}
	if string(header[:3]) != "ID3" || header[3] > 2 {
		source.Close()
		return errors.New("file does not contain an unsupported ID3v2 tag")
	}

	var tagSize int64
	for _, value := range header[6:10] {
		if value&0x80 != 0 {
			source.Close()
			return errors.New("unsupported ID3 tag has an invalid sync-safe size")
		}
		tagSize = (tagSize << 7) | int64(value)
	}
	audioOffset := int64(len(header)) + tagSize
	if audioOffset > info.Size() {
		source.Close()
		return errors.New("unsupported ID3 tag extends beyond the end of the file")
	}
	if _, err := source.Seek(audioOffset, io.SeekStart); err != nil {
		source.Close()
		return err
	}

	temporary, err := os.CreateTemp(filepath.Dir(mediaPath), ".podgrab-id3-*")
	if err != nil {
		source.Close()
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(info.Mode()); err != nil {
		temporary.Close()
		source.Close()
		return err
	}
	if _, err := io.Copy(temporary, source); err != nil {
		temporary.Close()
		source.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		source.Close()
		return err
	}
	if err := source.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, mediaPath)
}

// StartDownloadedEpisodeID3Check starts one background ID3 repair job for a
// podcast. It returns false when that podcast already has a job in progress.
func StartDownloadedEpisodeID3Check(podcastID string) (bool, error) {
	var podcast db.Podcast
	if err := db.GetPodcastById(podcastID, &podcast); err != nil {
		return false, err
	}

	id3CheckMu.Lock()
	if id3ChecksInProgress[podcastID] {
		id3CheckMu.Unlock()
		return false, nil
	}
	id3ChecksInProgress[podcastID] = true
	id3CheckMu.Unlock()

	go func() {
		defer func() {
			id3CheckMu.Lock()
			delete(id3ChecksInProgress, podcastID)
			id3CheckMu.Unlock()
		}()
		if err := CheckDownloadedEpisodeID3(podcastID); err != nil {
			Logger.Errorw("Downloaded episode ID3 check failed", "podcast", podcastID, "error", err)
		}
	}()
	return true, nil
}

// CheckDownloadedEpisodeID3 verifies every downloaded episode of one podcast.
// Missing or outdated metadata, artwork, and enabled image sidecars are
// repaired. A podgrab-id3-check.log file is appended beside the audio files.
// This explicit manual action works even if automatic ID3 editing is disabled.
func CheckDownloadedEpisodeID3(podcastID string) error {
	var episodes []db.PodcastItem
	if err := db.GetAllPodcastItemsByPodcastId(podcastID, &episodes); err != nil {
		return err
	}
	if len(episodes) == 0 {
		return nil
	}

	config, configured, err := postDownloadPodcastConfig(podcastID)
	if err != nil {
		return err
	}
	if !configured {
		config = defaultPostDownloadPodcastConfig(&episodes[0])
	}
	attrs := effectiveMetadataAttrs(config.MetadataAttrs)
	setting := db.GetOrCreateSetting()
	logs := make(map[string]*os.File)
	defer func() {
		for _, file := range logs {
			_ = file.Close()
		}
	}()

	var firstErr error
	for index := range episodes {
		item := &episodes[index]
		if item.DownloadStatus != db.Downloaded || item.DownloadPath == "" {
			continue
		}

		logFile, err := id3CheckLogForEpisode(logs, item.DownloadPath, item.Podcast.Title)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			Logger.Errorw("Could not write ID3 check log", "episode", item.ID, "error", err)
			continue
		}
		if _, err := os.Stat(item.DownloadPath); err != nil {
			if os.IsNotExist(err) {
				id3CheckLogLine(logFile, "MISSING", item, "file is marked as downloaded but is not present on disk")
			} else {
				id3CheckLogLine(logFile, "ERROR", item, "could not access file: "+err.Error())
				if firstErr == nil {
					firstErr = err
				}
			}
			continue
		}

		metadata, err := metadataForEpisode(item, config)
		if err != nil {
			id3CheckLogLine(logFile, "ERROR", item, "could not calculate expected metadata: "+err.Error())
			if firstErr == nil {
				firstErr = err
			}
			continue
		}

		imagePath := ""
		removeTemporaryImage := func() {}
		if item.Image != "" {
			imagePath, removeTemporaryImage, err = episodeImageForPostDownload(item, item.DownloadPath, false, true)
			if err != nil {
				id3CheckLogLine(logFile, "WARNING", item, "could not download episode image: "+err.Error())
				imagePath = ""
			}
		}

		matches, matchErr := id3MetadataMatches(item.DownloadPath, attrs, metadata, imagePath)
		sidecarPath := ""
		if imagePath != "" && setting.DownloadEpisodeImages {
			sidecarPath = id3CheckSidecarPath(item.DownloadPath, imagePath)
			if !filesHaveSameContents(sidecarPath, imagePath) {
				matches = false
			}
		}
		if matchErr != nil {
			matches = false
		}
		if matches {
			id3CheckLogLine(logFile, "UNCHANGED", item, "metadata and cover are current")
			removeTemporaryImage()
			continue
		}

		if err := writeEpisodeID3(item.DownloadPath, attrs, metadata, imagePath); err != nil {
			id3CheckLogLine(logFile, "ERROR", item, "could not update ID3 metadata: "+err.Error())
			if firstErr == nil {
				firstErr = err
			}
			removeTemporaryImage()
			continue
		}
		if sidecarPath != "" {
			if err := copyID3CheckSidecar(imagePath, sidecarPath); err != nil {
				id3CheckLogLine(logFile, "ERROR", item, "ID3 updated but could not save sidecar image: "+err.Error())
				if firstErr == nil {
					firstErr = err
				}
				removeTemporaryImage()
				continue
			}
			item.LocalImage = sidecarPath
			if err := db.UpdatePodcastItem(item); err != nil {
				id3CheckLogLine(logFile, "ERROR", item, "ID3 updated but could not save sidecar path: "+err.Error())
				if firstErr == nil {
					firstErr = err
				}
				removeTemporaryImage()
				continue
			}
		}
		id3CheckLogLine(logFile, "UPDATED", item, "metadata and cover were refreshed")
		removeTemporaryImage()
	}
	return firstErr
}

func id3MetadataMatches(mediaPath string, attrs []string, metadata episodeMetadata, imagePath string) (bool, error) {
	tag, err := id3v2.Open(mediaPath, id3v2.Options{Parse: true})
	if err != nil {
		return false, err
	}
	defer tag.Close()
	enabled := stringSet(attrs)
	if enabled["title"] && tag.Title() != metadata.Title {
		return false, nil
	}
	if enabled["artist"] && tag.Artist() != metadata.Artist {
		return false, nil
	}
	if enabled["album"] && tag.Album() != metadata.Album {
		return false, nil
	}
	if enabled["genre"] && tag.Genre() != metadata.Genre {
		return false, nil
	}
	if enabled["track_num"] && metadata.TrackNumber != "" && tag.GetTextFrame(tag.CommonID("Track number/Position in set")).Text != metadata.TrackNumber {
		return false, nil
	}
	if enabled["date"] && !id3DateMatches(tag, metadata.Date) {
		return false, nil
	}
	if imagePath != "" && !id3CoverMatches(tag, imagePath) {
		return false, nil
	}
	return true, nil
}

func id3DateMatches(tag *id3v2.Tag, value string) bool {
	if tag.Version() != 3 {
		return tag.GetTextFrame(tag.CommonID("Recording time")).Text == value
	}
	parsed, err := time.Parse("2006-01-02T15:04:05Z", value)
	if err != nil {
		return tag.GetTextFrame(tag.CommonID("Year")).Text == value
	}
	return tag.GetTextFrame(tag.CommonID("Year")).Text == parsed.Format("2006") &&
		tag.GetTextFrame(tag.CommonID("Date")).Text == parsed.Format("0201") &&
		tag.GetTextFrame(tag.CommonID("Time")).Text == parsed.Format("1504")
}

func id3CoverMatches(tag *id3v2.Tag, imagePath string) bool {
	expected, err := ioutil.ReadFile(imagePath)
	if err != nil || len(expected) == 0 {
		return false
	}
	for _, frame := range tag.GetFrames(tag.CommonID("Attached picture")) {
		picture, ok := frame.(id3v2.PictureFrame)
		if ok && picture.PictureType == id3v2.PTFrontCover && bytes.Equal(picture.Picture, expected) {
			return true
		}
	}
	return false
}

func id3CheckLogForEpisode(logs map[string]*os.File, audioPath string, podcastTitle string) (*os.File, error) {
	folder := filepath.Dir(audioPath)
	if file, exists := logs[folder]; exists {
		return file, nil
	}
	logPath := filepath.Join(folder, "podgrab-id3-check.log")
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintf(file, "\n%s\tCHECK START\t%s\n", time.Now().Format(time.RFC3339), podcastTitle); err != nil {
		file.Close()
		return nil, err
	}
	changeOwnership(logPath)
	logs[folder] = file
	return file, nil
}

func id3CheckLogLine(file *os.File, status string, item *db.PodcastItem, detail string) {
	if _, err := fmt.Fprintf(file, "%s\t%s\t%s\t%s\n", time.Now().Format(time.RFC3339), status, filepath.Base(item.DownloadPath), detail); err != nil {
		Logger.Errorw("Could not append ID3 check log", "episode", item.ID, "error", err)
	}
	fields := []interface{}{
		"status", status,
		"podcast", item.Podcast.Title,
		"episode", item.Title,
		"file", item.DownloadPath,
		"detail", detail,
	}
	switch status {
	case "ERROR":
		Logger.Errorw("Downloaded episode ID3 check", fields...)
	case "WARNING":
		Logger.Warnw("Downloaded episode ID3 check", fields...)
	default:
		Logger.Infow("Downloaded episode ID3 check", fields...)
	}
}

func id3CheckSidecarPath(audioPath string, imagePath string) string {
	return strings.TrimSuffix(audioPath, filepath.Ext(audioPath)) + filepath.Ext(imagePath)
}

func filesHaveSameContents(firstPath string, secondPath string) bool {
	first, err := ioutil.ReadFile(firstPath)
	if err != nil {
		return false
	}
	second, err := ioutil.ReadFile(secondPath)
	return err == nil && bytes.Equal(first, second)
}

func copyID3CheckSidecar(sourcePath string, destinationPath string) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()
	temporary, err := ioutil.TempFile(filepath.Dir(destinationPath), ".podgrab-sidecar-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := io.Copy(temporary, source); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, destinationPath); err != nil {
		return err
	}
	changeOwnership(destinationPath)
	return nil
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

// TestNavidromeConnection validates host and credentials through the Subsonic
// ping endpoint. Blank fields retain their saved or environment-provided value;
// supplied values are never persisted by this operation.
func TestNavidromeConnection(host string, username string, password string) error {
	configured := db.GetOrCreateSetting()
	candidate := *configured
	if strings.TrimSpace(host) != "" {
		candidate.NavidromeHost = strings.TrimRight(strings.TrimSpace(host), "/")
	}
	if strings.TrimSpace(username) != "" {
		candidate.NavidromeUsername = strings.TrimSpace(username)
	}
	if password != "" {
		candidate.NavidromePassword = password
	}

	client, err := newNavidromeClient(&candidate)
	if err != nil {
		return err
	}
	return client.call("ping", nil, &subsonicEnvelope{})
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
