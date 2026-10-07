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

// buildInfo is the build identity: what is known before anything is opened.
// whatsmeow's version comes from the module list embedded in the binary. FTS5
// is not part of the identity and stays false here; see withFTS.
func buildInfo() VersionInfo {
	info := VersionInfo{Version: version, Commit: commit, Go: runtime.Version(), Whatsmeow: "unknown"}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range bi.Deps {
			if dep.Path == "go.mau.fi/whatsmeow" {
				info.Whatsmeow = dep.Version
				if dep.Replace != nil {
					// A replace by a local directory has no version to show.
					info.Whatsmeow = dep.Replace.Version
					if info.Whatsmeow == "" {
						info.Whatsmeow = "replaced by " + dep.Replace.Path
					}
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

// withFTS adds the state of the open store for GET /api/version: whether the
// messages_fts index is in use. False covers both a SQLite build without FTS5
// and an index that could not be built (store.go logs which).
func (v VersionInfo) withFTS(active bool) VersionInfo {
	v.FTS5 = active
	return v
}

// String is the one-line form used in the startup log, and it carries the
// identity only. The banner is logged before the store is open, so it used to
// print a constant fts5=off next to a store line saying the opposite
// (issue #499).
func (v VersionInfo) String() string {
	return strings.Join([]string{"whatsapp-bridge " + v.Version, "commit " + v.Commit, v.Go, "whatsmeow " + v.Whatsmeow}, ", ")
}

func handleVersion(info VersionInfo) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, info)
	}
}
