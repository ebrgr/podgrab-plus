package service

import (
	"bytes"
	"io/ioutil"
	"path/filepath"
	"testing"

	"github.com/akhilrex/podgrab/db"
)

func TestWritePodcastM3U(t *testing.T) {
	folder := t.TempDir()
	playlistPath := filepath.Join(folder, "Podcast.m3u")
	episodes := []db.PodcastItem{
		{DownloadPath: filepath.Join(folder, "newest.mp3")},
		{DownloadPath: filepath.Join(folder, "older.mp3")},
	}
	if err := writePodcastM3U(playlistPath, "https://images.example/podcast.jpg", episodes); err != nil {
		t.Fatal(err)
	}
	contents, err := ioutil.ReadFile(playlistPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "#EXTALBUMARTURL: https://images.example/podcast.jpg\nnewest.mp3\nolder.mp3\n"
	if string(contents) != want {
		t.Fatalf("unexpected playlist contents: %q", contents)
	}
}

func TestCopyPodcastPlaylistArtwork(t *testing.T) {
	folder := t.TempDir()
	source := []byte("podcast artwork")
	if err := ioutil.WriteFile(filepath.Join(folder, "folder.jpg"), source, 0600); err != nil {
		t.Fatal(err)
	}
	if err := copyPodcastPlaylistArtwork(folder, "Podcast"); err != nil {
		t.Fatal(err)
	}
	copied, err := ioutil.ReadFile(filepath.Join(folder, "Podcast.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(copied, source) {
		t.Fatal("playlist artwork differs from folder.jpg")
	}
}
