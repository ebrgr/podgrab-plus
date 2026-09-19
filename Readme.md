
[![Contributors][contributors-shield]][contributors-url]
[![Forks][forks-shield]][forks-url]
[![Stargazers][stars-shield]][stars-url]
[![Issues][issues-shield]][issues-url]
[![MIT License][license-shield]][license-url]
[![LinkedIn][linkedin-shield]][linkedin-url]

<!-- PROJECT LOGO -->
<br />
<p align="center">
  <!-- <a href="https://github.com/ebrgr/podgrab-plus">
    <img src="images/logo.png" alt="Logo" width="80" height="80">
  </a> -->

  <h1 align="center" style="margin-bottom:0px">Podgrab-Plus [NAViDROME Ed.]</h1>
  <p align="center">Current Version - q3/2026</p>

  <p align="center">
    A self-hosted podcast manager to download episodes as soon as they become live
    <br />
    <a href="https://github.com/ebrgr/podgrab-plus"><strong>Explore the docs »</strong></a>
    <br />
    <br />
    <!-- <a href="https://github.com/ebrgr/podgrab-plus">View Demo</a>
    · -->
    <a href="https://github.com/ebrgr/podgrab-plus/issues">Report Bug</a>
    ·
    <a href="https://github.com/ebrgr/podgrab-plus/issues">Request Feature</a>
        ·
    <a href="Screenshots.md">Screenshots</a>
  </p>
</p>

<!-- TABLE OF CONTENTS -->

## Table of Contents

- [Table of Contents](#table-of-contents)
- [About The Project](#about-the-project)
  - [Motivation](#motivation)
  - [Project origin and Cousin-IDDD](#project-origin-and-cousin-iddd)
  - [Built With](#built-with)
  - [Features](#features)
- [Installation](#installation)
  - [Using Docker](#using-docker)
  - [Using Docker-Compose](#using-docker-compose)
  - [Build from Source / Ubuntu Installation](#build-from-source--ubuntu-installation)
  - [Environment Variables](#environment-variables)
  - [ID3 and Navidrome post-download integration](#id3-and-navidrome-post-download-integration)
  - [Setup](#setup)
- [License](#license)
- [Roadmap](#roadmap)
- [Contact](#contact)

<!-- ABOUT THE PROJECT -->

## About The Project

Podgrab is a is a self-hosted podcast manager which automatically downloads latest podcast episodes. It is a light-weight application built using GO.

It works best if you already know which podcasts you want to monitor. However there is a podcast search system powered by iTunes built into Podgrab


### Motivation

Podgrab started as a tool that I initially built to solve a specific problem I had. During the COVID pandemic times I started going for a run. I do not prefer taking my phone along so I would add podcast episodes to my smart watch which could be connected with my bluetooth earphones. Most podcasting apps do not expose the mp3 files directly which is why I decided to build this quick tool for myself. Once it reached a stage where my requirements were fulfilled I decided to make it a little pretty and share it with everyone else.

### Project origin and Cousin-IDDD

Podgrab-Plus is a branch of Podgrab focused on preserving downloaded podcast
files as a well-organized local audio library and integrating that library with
Navidrome. Its post-download workflow was initially inspired by
[Cousin-IDDD](https://github.com/allanjamesvestal/Cousin-IDDD), a separate
project designed to process downloaded podcast audio: enrich MP3 files with
ID3 metadata and artwork, then make the episodes available to a Subsonic-
compatible music server.

That idea was brought into the Podgrab interface and service flow so it runs
after an episode download. This branch can write missing ID3 metadata, embed
cover art, optionally save a sidecar image, create an `.m3u` sidecar playlist,
and update Navidrome through its Subsonic API. The integration work in this
branch was vibe-coded with ChatGPT and then reviewed and adapted for Podgrab.


### Built With

- [Go](https://golang.org/)
- [Go-Gin](https://github.com/gin-gonic/gin)
- [GORM](https://github.com/go-gorm/gorm)
- [SQLite](https://www.sqlite.org/index.html)
- [Chat-GPT]

### Features
- Download/Archive complete podcast
- Auto-download new episodes
- Tag/Label podcasts into groups
- Download on demand
- Podcast Discovery - Search and Add podcasts using iTunes API
- Full-fledged podcast player - Play downloaded files or stream from original source. Play single episodes, full podcasts and podcast groups(tags)
- Add using direct RSS feed URL / OMPL import / Search
- Basic Authentication
- Existing episode file detection - Prevent re-downloading files if already present
- Easy OPML import/export
- Customizable episode names
- Dark Mode
- Self Hosted / Open Source
- Docker support

## Installation

The easiest way to run Podgrab is to run it as a docker container.

### Using Docker

Simple setup without mounted volumes (for testing and evaluation)

```sh
  docker run -d -p 8080:8080 --name=podgrab ghcr.io/ebrgr/podgrab-plus:latest
```

Binding local volumes to the container

```sh
   docker run -d -p 8080:8080 --name=podgrab -v "/host/path/to/assets:/assets" -v "/host/path/to/config:/config" ghcr.io/ebrgr/podgrab-plus:latest
```

### Using Docker-Compose

Modify the docker compose file provided [here](https://github.com/ebrgr/podgrab-plus/blob/master/docker-compose.yml) to update the volume and port binding and run the following command

```yaml
version: "2.1"
services:
  podgrab:
    image: ghcr.io/ebrgr/podgrab-plus:latest
    container_name: podgrab
    environment:
      - CHECK_FREQUENCY=240
     # - PASSWORD=password     ## Uncomment to enable basic authentication, username = podgrab
    volumes:
      - /path/to/config:/config
      - /path/to/data:/assets
    ports:
      - 8080:8080
    restart: unless-stopped
```

```sh
   docker-compose up -d
```
### Build from Source / Ubuntu Installation

Although personally I feel that using the docker container is the best way of using and enjoying something like Podgrab, a lot of people in the community are still not comfortable with using Docker and wanted to host it natively on their Linux servers. Follow the link below to get a guide on how to build Podgrab from source.

[Build from source / Ubuntu Guide](docs/ubuntu-install.md)
### Environment Variables

| Name            | Description                                                             | Default |
| --------------- | ----------------------------------------------------------------------- | ------- |
| CHECK_FREQUENCY | How frequently to check for new episodes and missing files (in minutes) | 30      |
| PASSWORD        | Set to some non empty value to enable Basic Authentication, username `podgrab`|(empty)|
| PORT            | Change the internal port of the application. If you change this you might have to change your docker configuration as well | (empty) |  

### ID3 and Navidrome post-download integration

The integration can create `CONFIG/post-download.json` automatically. After an
MP3 is downloaded, Podgrab writes its ID3 fields, requests a Navidrome scan,
finds the album by podcast name and author, and finds or creates a playlist
named `Últimos episódios - <podcast>`. The discovered album and playlist IDs
are persisted in the JSON file. Copy `post-download.example.json` only when you
need custom names, filters, track-number parsing or playlist size. The older
`CONFIG/cousin-iddd.config.json` filename is also accepted.

| Name | Description | Default |
| --- | --- | --- |
| POST_DOWNLOAD_CONFIG | Optional custom path for the integration JSON file | `CONFIG/post-download.json` |
| NAVIDROME_HOST | Navidrome base URL | (empty) |
| NAVIDROME_USERNAME | Subsonic API username | (empty) |
| NAVIDROME_PASSWORD | Subsonic API password | (empty) |
| NAVIDROME_WAIT | Maximum time to wait for the episode to be indexed | `60s` |
| NAVIDROME_POLL | Interval between album checks | `5s` |

ID3 editing and Navidrome updates are disabled by default and can be enabled
from the Settings page. Navidrome connection values saved in the UI take
precedence over environment variables; environment variables remain available
as a fallback.

#### Navidrome library layout

If your Navidrome `/music` directory contains the Podgrab downloads, add an
`.ndignore` file to `/music` with the following content:

```text
/podcast/**
```

This prevents the main Navidrome library from indexing the `/music/podcast/`
folder. Then create a second Navidrome library whose path points to that
podcast folder. Keeping podcasts in their own library separates them from the
main music collection while allowing the post-download integration to scan and
manage their albums and playlists.

### Setup

- Enable *websocket support* if running behind a reverse proxy. This is needed for the "Add to playlist" functionality.
- Go through the settings page once and change relevant settings before adding podcasts.

## License

Distributed under the GPL-3.0 License. See `LICENSE` for more information.

## Roadmap

- [x] Basic Authentication
- [x] Append Date to filename
- [x] iTunes Search
- [x] Existing episodes detection (Will not redownload if files exist even with a fresh install)
- [x] Downloading/downloaded indicator
- [x] Played/Unplayed Flag
- [x] OPML import
- [x] OPML export
- [x] In built podcast player
- [x] Set ID3 tags if not set
- [x] save .m3u sidecar playlist
- [x] Initial [Cousin-IDDD](https://github.com/allanjamesvestal/Cousin-IDDD) integration in the project interface through the Subsonic API
- [ ] initial multi-language app
- [ ] Filtering and Sorting options




<!-- CONTACT -->

## Contact

Project Link: [https://github.com/ebrgr/podgrab-plus](https://github.com/ebrgr/podgrab-plus)

<!-- MARKDOWN LINKS & IMAGES -->
<!-- https://www.markdownguide.org/basic-syntax/#reference-style-links -->

[contributors-shield]: https://img.shields.io/github/contributors/ebrgr/podgrab-plus.svg?style=flat-square
[contributors-url]: https://github.com/ebrgr/podgrab-plus/graphs/contributors
[forks-shield]: https://img.shields.io/github/forks/ebrgr/podgrab-plus.svg?style=flat-square
[forks-url]: https://github.com/ebrgr/podgrab-plus/network/members
[stars-shield]: https://img.shields.io/github/stars/ebrgr/podgrab-plus.svg?style=flat-square
[stars-url]: https://github.com/ebrgr/podgrab-plus/stargazers
[issues-shield]: https://img.shields.io/github/issues/ebrgr/podgrab-plus.svg?style=flat-square
[issues-url]: https://github.com/ebrgr/podgrab-plus/issues
[license-shield]: https://img.shields.io/github/license/ebrgr/podgrab-plus.svg?style=flat-square
[license-url]: https://github.com/ebrgr/podgrab-plus/blob/master/LICENSE.txt
[linkedin-shield]: https://img.shields.io/badge/-LinkedIn-black.svg?style=flat-square&logo=linkedin&colorB=555
