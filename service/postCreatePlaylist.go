package service

import (
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/akhilrex/podgrab/db"
)

var podcastPlaylistMu sync.Mutex

// CreateOrUpdatePodcastPlaylist writes a M3U playlist in the podcast folder.
// Its entries are relative audio filenames ordered from newest to oldest.
func CreateOrUpdatePodcastPlaylist(podcastID string) error {
	setting := db.GetOrCreateSetting()
	if !setting.CreateM3UPlaylists {
		return nil
	}

	podcastPlaylistMu.Lock()
	defer podcastPlaylistMu.Unlock()

	var podcast db.Podcast
	if err := db.GetPodcastById(podcastID, &podcast); err != nil {
		return err
	}
	var episodes []db.PodcastItem
	if err := db.GetAllPodcastItemsByPodcastId(podcastID, &episodes); err != nil {
		return err
	}

	downloaded := make([]db.PodcastItem, 0, len(episodes))
	for _, episode := range episodes {
		if episode.DownloadStatus != db.Downloaded || episode.DownloadPath == "" {
			continue
		}
		if _, err := os.Stat(episode.DownloadPath); err == nil {
			downloaded = append(downloaded, episode)
		}
	}
	if len(downloaded) == 0 {
		return nil
	}

	sort.SliceStable(downloaded, func(i, j int) bool {
		if downloaded[i].PubDate.Equal(downloaded[j].PubDate) {
			return downloaded[i].DownloadDate.After(downloaded[j].DownloadDate)
		}
		return downloaded[i].PubDate.After(downloaded[j].PubDate)
	})

	folder := filepath.Dir(downloaded[0].DownloadPath)
	playlistBaseName := cleanFileName(podcast.Title)
	playlistPath := filepath.Join(folder, playlistBaseName+".m3u")
	if err := writePodcastM3U(playlistPath, podcast.Image, downloaded); err != nil {
		return err
	}

	return copyPodcastPlaylistArtwork(folder, playlistBaseName)
}

func writePodcastM3U(playlistPath string, artworkURL string, episodes []db.PodcastItem) error {
	var contents strings.Builder
	contents.WriteString("#EXTALBUMARTURL: ")
	contents.WriteString(strings.TrimSpace(artworkURL))
	contents.WriteByte('\n')
	for _, episode := range episodes {
		contents.WriteString(filepath.Base(episode.DownloadPath))
		contents.WriteByte('\n')
	}

	temporary, err := ioutil.TempFile(filepath.Dir(playlistPath), ".podgrab-playlist-*.m3u")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.WriteString(contents.String()); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, playlistPath); err != nil {
		return err
	}
	changeOwnership(playlistPath)
	return nil
}

// copyPodcastPlaylistArtwork copies folder.jpg to <playlist-name>.jpg so
// players that associate artwork by basename can find the playlist cover.
func copyPodcastPlaylistArtwork(folder string, playlistBaseName string) error {
	sourcePath := filepath.Join(folder, "folder.jpg")
	if _, err := os.Stat(sourcePath); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}

	destinationPath := filepath.Join(folder, playlistBaseName+".jpg")
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()

	temporary, err := ioutil.TempFile(folder, ".podgrab-playlist-art-*.jpg")
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
