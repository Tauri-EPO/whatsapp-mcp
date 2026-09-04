package main

// Build identity, exposed by GET /api/version (unauthenticated: it reveals
// nothing an attacker could use and is the first thing to check when asking
// "is the server on the new image?").
//
// version and commit are injected at build time:
//   go build -ldflags "-X main.version=<tag> -X main.commit=<sha>"
// The Dockerfile passes GIT_SHA/VERSION build args; `go run .` reports "dev".

import (
	"net/http"
	"runtime"
	"runtime/debug"
	"strings"
)

var (
	version = "dev"
	commit  = "unknown"
)

type VersionInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Go        string `json:"go"`
	Whatsmeow string `json:"whatsmeow"`
	FTS5      bool   `json:"fts5"`
}

// buildInfo assembles the version response. whatsmeow's version comes from the
// module list embedded in the binary; fts5 reflects the actual SQLite build.
func buildInfo(fts5 bool) VersionInfo {
	info := VersionInfo{Version: version, Commit: commit, Go: runtime.Version(), Whatsmeow: "unknown", FTS5: fts5}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range bi.Deps {
			if dep.Path == "go.mau.fi/whatsmeow" {
				info.Whatsmeow = dep.Version
				if dep.Replace != nil {
					info.Whatsmeow = dep.Replace.Version
				}
			}
		}
		if info.Commit == "unknown" {
			for _, setting := range bi.Settings {
				if setting.Key == "vcs.revision" && setting.Value != "" {
					info.Commit = setting.Value
					if len(info.Commit) > 12 {
						info.Commit = info.Commit[:12]
					}
				}
			}
		}
	}
	return info
}

// versionString is the one-line form used in the startup log.
func (v VersionInfo) String() string {
	fts := "fts5=off"
	if v.FTS5 {
		fts = "fts5=on"
	}
	return strings.Join([]string{"whatsapp-bridge " + v.Version, "commit " + v.Commit, v.Go, "whatsmeow " + v.Whatsmeow, fts}, ", ")
}

func handleVersion(info VersionInfo) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, info)
	}
}
