package main

import (
	"net/http"
	"strings"
)

const (
	defaultPublicationLimit = 20
	maxPublicationLimit     = 100
)

// handlePublications serves the publication register, newest first.
//
//	GET /api/publications?limit=&before=&after=&include_superseded=1
//
// before pages toward older stories (use next_before from the last page);
// after returns stories newer than a sequence (use last_seq to poll).
func (c *controlServer) handlePublications(w http.ResponseWriter, r *http.Request) {
	before, err := parseCursor(r.URL.Query().Get("before"))
	if err != nil {
		http.Error(w, "before: "+err.Error(), http.StatusBadRequest)
		return
	}
	after, err := parseCursor(r.URL.Query().Get("after"))
	if err != nil {
		http.Error(w, "after: "+err.Error(), http.StatusBadRequest)
		return
	}
	items, err := readPublications(c.dataDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	include := r.URL.Query().Get("include_superseded")
	writeJSON(w, http.StatusOK, pagePublications(items, publicationQuery{
		Before:            before,
		After:             after,
		Limit:             queryLimit(r, "limit", defaultPublicationLimit, maxPublicationLimit),
		IncludeSuperseded: include == "1" || include == "true",
	}))
}

// handlePublication serves one publication by id, superseded or not.
func (c *controlServer) handlePublication(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/publications/")
	if !publicationIDPattern.MatchString(id) {
		http.Error(w, "invalid publication id", http.StatusBadRequest)
		return
	}
	items, err := readPublications(c.dataDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	replaced := supersededBy(items)
	for _, item := range items {
		if item.ID == id {
			writeJSON(w, http.StatusOK, publicationView{publication: item, SupersededBy: replaced[id]})
			return
		}
	}
	http.NotFound(w, r)
}
