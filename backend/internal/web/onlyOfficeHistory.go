package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	jwt "github.com/golang-jwt/jwt/v4"
	"github.com/gtsteffaniak/filebrowser/backend/internal/adapters/fs/files"
	"github.com/gtsteffaniak/filebrowser/backend/internal/database/users"
	"github.com/gtsteffaniak/filebrowser/backend/internal/utils"
	"github.com/gtsteffaniak/filebrowser/backend/pkg/indexing/iteminfo"
	"github.com/gtsteffaniak/filebrowser/backend/pkg/settings"
	"github.com/gtsteffaniak/go-logger/logger"
)

// OnlyOffice document version history.
//
// The layout follows the ONLYOFFICE reference integration
// (document-server-integration): a document's history directory holds one
// numbered directory per previous version, and the live file is always the
// newest version.
//
//	<history root>/<sha256(real path)>/
//	  document.json        real path of the document (for orphan cleanup)
//	  pending/<key>/       snapshot taken at the first save of an editing session
//	    file               the document as it was when the session started
//	    meta.json
//	  <n>/                 version n, n = 1, 2, ...
//	    file               the document content of version n
//	    changes.zip        the edits that turn version n into version n+1
//	    meta.json          key, created, and the callback "history" object of
//	                       the edit from version n to version n+1
//
// The document server only sends "changesurl" and "history" with the final
// save of a session (status 2), and those cover the whole session. FileBrowser
// writes force saves (status 6) straight to the live file, so the content the
// changes apply to is captured before the first write of the session and kept
// under pending/ until the session ends.

const (
	officeHistoryFileName    = "file"
	officeHistoryChangesName = "changes.zip"
	officeHistoryMetaName    = "meta.json"
	officeHistoryPendingDir  = "pending"
	officeHistoryDocMetaName = "document.json"
	officeHistoryTimeFormat  = "2006-01-02 15:04:05"

	// officeHistoryChangesMaxBytes bounds the changes.zip download.
	officeHistoryChangesMaxBytes = 256 << 20
)

var officeHistoryKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// officeVersionMeta is stored as meta.json in a version (or pending) directory.
type officeVersionMeta struct {
	Version  int    `json:"version,omitempty"`
	Key      string `json:"key"`      // document key the content of this version was edited under
	Created  string `json:"created"`  // when this version's content was last written (UTC)
	FileType string `json:"fileType"` // extension without the dot
	// History is the callback "history" object (changes + serverVersion) of
	// the edit that turned this version into the next one.
	History json.RawMessage `json:"history,omitempty"`
	// Author of the next version, used when History carries no user.
	SavedBy    *officeHistoryUser `json:"savedBy,omitempty"`
	SavedAt    string             `json:"savedAt,omitempty"`
	HasChanges bool               `json:"hasChanges"`
}

type officeHistoryUser struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// officeHistoryChanges mirrors the callback "history" object.
type officeHistoryChanges struct {
	Changes []struct {
		Created string             `json:"created"`
		User    *officeHistoryUser `json:"user"`
	} `json:"changes"`
	ServerVersion string `json:"serverVersion"`
}

// officeHistoryEntry is one element of the history list passed to the
// editor's refreshHistory method.
type officeHistoryEntry struct {
	Version       int                `json:"version"`
	Key           string             `json:"key"`
	Created       string             `json:"created"`
	User          *officeHistoryUser `json:"user,omitempty"`
	Changes       json.RawMessage    `json:"changes,omitempty"`
	ServerVersion string             `json:"serverVersion,omitempty"`
}

func officeHistoryEnabled() bool {
	return settings.Config.Integrations.OnlyOffice.History.Enabled
}

// officeHistoryRoot returns the directory that stores document versions.
func officeHistoryRoot() string {
	if p := settings.Config.Integrations.OnlyOffice.History.Path; p != "" {
		return p
	}
	return filepath.Join(filepath.Dir(settings.Config.Server.DatabaseV2.Path), "office-history")
}

// officeHistoryDocDir returns the history directory of the document stored at realPath.
func officeHistoryDocDir(realPath string) string {
	return filepath.Join(officeHistoryRoot(), utils.HashSHA256(realPath))
}

// officeHistoryKeyDir maps a document key to a safe directory name.
func officeHistoryKeyDir(key string) string {
	if officeHistoryKeyPattern.MatchString(key) {
		return key
	}
	return utils.HashSHA256(key)
}

func officeHistoryFileType(realPath string) string {
	return strings.ToLower(strings.TrimPrefix(filepath.Ext(realPath), "."))
}

func readOfficeVersionMeta(dir string) (*officeVersionMeta, error) {
	data, err := os.ReadFile(filepath.Join(dir, officeHistoryMetaName))
	if err != nil {
		return nil, err
	}
	var meta officeVersionMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

// writeFileAtomic writes data to path through a temporary file in the same directory.
func writeFileAtomic(path string, data []byte) error {
	return writeReaderAtomic(path, bytes.NewReader(data), -1)
}

// writeReaderAtomic copies r to path through a temporary file. A non-negative
// limit fails the write when r holds more than limit bytes.
func writeReaderAtomic(path string, r io.Reader, limit int64) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	src := r
	if limit >= 0 {
		src = io.LimitReader(r, limit+1)
	}
	n, err := io.Copy(tmp, src)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if limit >= 0 && n > limit {
		return fmt.Errorf("content exceeds %d bytes", limit)
	}
	return os.Rename(tmpName, path)
}

func writeOfficeVersionMeta(dir string, meta *officeVersionMeta) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, officeHistoryMetaName), data)
}

// listOfficeVersions returns the stored versions of a document, oldest first.
func listOfficeVersions(docDir string) ([]*officeVersionMeta, error) {
	entries, err := os.ReadDir(docDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var versions []*officeVersionMeta
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		n, err := strconv.Atoi(e.Name())
		if err != nil || n < 1 {
			continue
		}
		meta, err := readOfficeVersionMeta(filepath.Join(docDir, e.Name()))
		if err != nil {
			logger.Warningf("OnlyOffice history: skipping unreadable version %s: %v", filepath.Join(docDir, e.Name()), err)
			continue
		}
		meta.Version = n
		versions = append(versions, meta)
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i].Version < versions[j].Version })
	return versions, nil
}

// captureOfficeSessionStart snapshots the document before the first write of
// the editing session identified by key. Later saves of the same session keep
// the first snapshot.
func captureOfficeSessionStart(realPath, key string) error {
	docDir := officeHistoryDocDir(realPath)
	pendingDir := filepath.Join(docDir, officeHistoryPendingDir, officeHistoryKeyDir(key))
	if _, err := os.Stat(filepath.Join(pendingDir, officeHistoryMetaName)); err == nil {
		return nil
	}
	if err := os.MkdirAll(pendingDir, 0o700); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(docDir, officeHistoryDocMetaName), mustMarshalJSON(map[string]string{"path": realPath})); err != nil {
		return err
	}

	src, err := os.Open(realPath)
	if err != nil {
		return err
	}
	defer src.Close()
	stat, err := src.Stat()
	if err != nil {
		return err
	}
	if err := writeReaderAtomic(filepath.Join(pendingDir, officeHistoryFileName), src, -1); err != nil {
		return err
	}
	return writeOfficeVersionMeta(pendingDir, &officeVersionMeta{
		Key:      key,
		Created:  stat.ModTime().UTC().Format(officeHistoryTimeFormat),
		FileType: officeHistoryFileType(realPath),
	})
}

// commitOfficeVersion turns the pending snapshot of the session identified by
// key into the next stored version. changes may be nil when the document
// server sent no change set (e.g. a session that ended after a force save).
func commitOfficeVersion(realPath, key string, changes io.Reader, history json.RawMessage, savedBy *officeHistoryUser) error {
	docDir := officeHistoryDocDir(realPath)
	pendingDir := filepath.Join(docDir, officeHistoryPendingDir, officeHistoryKeyDir(key))
	meta, err := readOfficeVersionMeta(pendingDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}

	versions, err := listOfficeVersions(docDir)
	if err != nil {
		return err
	}
	next := 1
	if len(versions) > 0 {
		next = versions[len(versions)-1].Version + 1
	}

	if changes != nil {
		if err := writeReaderAtomic(filepath.Join(pendingDir, officeHistoryChangesName), changes, officeHistoryChangesMaxBytes); err != nil {
			logger.Warningf("OnlyOffice history: could not store changes for %s: %v", realPath, err)
		} else {
			meta.HasChanges = true
		}
	}
	if len(history) > 0 && string(history) != "null" {
		meta.History = history
	}
	meta.SavedBy = savedBy
	meta.SavedAt = time.Now().UTC().Format(officeHistoryTimeFormat)
	meta.Version = next
	if err := writeOfficeVersionMeta(pendingDir, meta); err != nil {
		return err
	}
	if err := os.Rename(pendingDir, filepath.Join(docDir, strconv.Itoa(next))); err != nil {
		return err
	}
	pruneOfficeVersions(docDir)
	removeStaleOfficeSessions(docDir)
	return nil
}

// officeHistoryPendingMaxAge bounds how long the snapshot of a session that
// never reported its end (e.g. the document server lost it) is kept.
const officeHistoryPendingMaxAge = 7 * 24 * time.Hour

// removeStaleOfficeSessions drops pending snapshots of long-gone sessions.
func removeStaleOfficeSessions(docDir string) {
	pendingRoot := filepath.Join(docDir, officeHistoryPendingDir)
	entries, err := os.ReadDir(pendingRoot)
	if err != nil {
		return
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < officeHistoryPendingMaxAge {
			continue
		}
		if err := os.RemoveAll(filepath.Join(pendingRoot, e.Name())); err != nil {
			logger.Warningf("OnlyOffice history: could not remove stale session %s: %v", e.Name(), err)
		}
	}
}

// pruneOfficeVersions removes the oldest versions beyond history.maxVersions.
func pruneOfficeVersions(docDir string) {
	limit := settings.Config.Integrations.OnlyOffice.History.MaxVersions
	if limit <= 0 {
		return
	}
	versions, err := listOfficeVersions(docDir)
	if err != nil {
		return
	}
	for len(versions) > limit {
		dir := filepath.Join(docDir, strconv.Itoa(versions[0].Version))
		if err := os.RemoveAll(dir); err != nil {
			logger.Warningf("OnlyOffice history: could not prune %s: %v", dir, err)
			return
		}
		versions = versions[1:]
	}
}

func mustMarshalJSON(v interface{}) []byte {
	data, _ := json.Marshal(v)
	return data
}

// recordOfficeSessionStart is called by the callback handler under the
// document's save lock, right before the downloaded document overwrites the
// live file.
func recordOfficeSessionStart(realPath string, data *OnlyOfficeCallback) {
	if !officeHistoryEnabled() || realPath == "" {
		return
	}
	if err := captureOfficeSessionStart(realPath, data.Key); err != nil {
		logger.Errorf("OnlyOffice history: could not snapshot %s before saving: %v", realPath, err)
	}
}

// recordOfficeSessionEnd stores the session's starting content as a version
// once the document server reports the session as closed.
func recordOfficeSessionEnd(realPath string, data *OnlyOfficeCallback, user *users.User) {
	if !officeHistoryEnabled() || realPath == "" {
		return
	}

	var changes io.Reader
	if data.ChangesURL != "" {
		if downloadURL := resolveOnlyOfficeDownloadURL(data.ChangesURL); downloadURL != "" {
			resp, err := onlyOfficeDownloadClient.Get(downloadURL)
			if err != nil {
				logger.Warningf("OnlyOffice history: could not download changes: %v", err)
			} else {
				defer resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					changes = resp.Body
				} else {
					logger.Warningf("OnlyOffice history: changes download returned status %d", resp.StatusCode)
				}
			}
		}
	}

	var history json.RawMessage
	if data.History != nil {
		history = mustMarshalJSON(data.History)
	}
	var savedBy *officeHistoryUser
	if user != nil {
		savedBy = &officeHistoryUser{ID: strconv.FormatUint(uint64(user.ID), 10), Name: user.Username}
	}
	if err := commitOfficeVersion(realPath, data.Key, changes, history, savedBy); err != nil {
		logger.Errorf("OnlyOffice history: could not store version of %s: %v", realPath, err)
	}
}

// lastOfficeChange returns the time and author of the last change recorded in
// a callback history object.
func lastOfficeChange(meta *officeVersionMeta) (created string, user *officeHistoryUser, serverVersion string, changes json.RawMessage) {
	created = meta.SavedAt
	user = meta.SavedBy
	if len(meta.History) == 0 {
		return created, user, "", nil
	}
	var h officeHistoryChanges
	if err := json.Unmarshal(meta.History, &h); err != nil {
		return created, user, "", nil
	}
	var raw struct {
		Changes json.RawMessage `json:"changes"`
	}
	_ = json.Unmarshal(meta.History, &raw)
	if n := len(h.Changes); n > 0 {
		if h.Changes[n-1].Created != "" {
			created = h.Changes[n-1].Created
		}
		if h.Changes[n-1].User != nil {
			user = h.Changes[n-1].User
		}
	}
	return created, user, h.ServerVersion, raw.Changes
}

// buildOfficeHistory assembles the refreshHistory payload: the stored versions
// followed by the live document, which uses currentKey (the key of the open
// editor session).
func buildOfficeHistory(versions []*officeVersionMeta, currentKey string, currentModTime time.Time) (int, []officeHistoryEntry) {
	entries := make([]officeHistoryEntry, 0, len(versions)+1)
	var prev *officeVersionMeta
	for _, v := range versions {
		entries = append(entries, officeHistoryEntryFor(v.Version, v.Key, v.Created, prev))
		prev = v
	}
	current := 1
	if prev != nil {
		current = prev.Version + 1
	}
	entries = append(entries, officeHistoryEntryFor(current, currentKey, currentModTime.UTC().Format(officeHistoryTimeFormat), prev))
	return current, entries
}

// officeHistoryEntryFor describes a version; prev is the stored version it was
// edited from, if it is still kept.
func officeHistoryEntryFor(version int, key, created string, prev *officeVersionMeta) officeHistoryEntry {
	entry := officeHistoryEntry{Version: version, Key: key, Created: created}
	// Only describe the edit when prev is the version directly before this one.
	if prev == nil || prev.Version != version-1 {
		return entry
	}
	changedAt, user, serverVersion, changes := lastOfficeChange(prev)
	if changedAt != "" {
		entry.Created = changedAt
	}
	entry.User = user
	entry.ServerVersion = serverVersion
	entry.Changes = changes
	return entry
}

// officeHistoryContext resolves the document for a history request and checks
// that history is enabled and the user may view the document.
func officeHistoryContext(r *http.Request, d *Context) (source, path string, fi *iteminfo.ExtendedFileInfo, status int, err error) {
	if !officeHistoryEnabled() || settings.Config.Integrations.OnlyOffice.Url == "" {
		return "", "", nil, http.StatusNotFound, errors.New("onlyoffice history is not enabled")
	}
	if d.Share.Hash != "" {
		return "", "", nil, http.StatusForbidden, errors.New("onlyoffice history is not available on shares")
	}
	source = r.URL.Query().Get("source")
	path, err = utils.SanitizePath(r.URL.Query().Get("path"))
	if err != nil {
		return "", "", nil, http.StatusBadRequest, err
	}
	if source == "" || path == "" {
		return "", "", nil, http.StatusBadRequest, errors.New("missing required parameters: source and path")
	}
	sourceInfo, ok := settings.Config.Server.NameToSource[source]
	if !ok {
		return "", "", nil, http.StatusNotFound, errors.New("source not found")
	}
	source = sourceInfo.Name
	perms, err := effectiveFilePerms(d, source)
	if err != nil || !perms.View {
		return "", "", nil, http.StatusForbidden, errors.New("user is not allowed to view files in this source")
	}
	fi, err = files.FileInfoFaster(utils.FileOptions{
		Path:           path,
		Source:         source,
		FollowSymlinks: true,
	}, d.User)
	if err != nil {
		return "", "", nil, ErrToStatus(err), err
	}
	if fi.RealPath == "" {
		return "", "", nil, http.StatusNotFound, errors.New("could not resolve document")
	}
	return source, path, fi, 0, nil
}

// onlyofficeHistoryGetHandler returns the version list for the editor's version history.
//
// @Summary Get OnlyOffice document history
// @Description Returns the versions of a document for the OnlyOffice editor's refreshHistory method. Requires integrations.office.history.enabled.
// @Tags Office
// @Produce json
// @Param source query string true "Source name"
// @Param path query string true "File path"
// @Success 200 {object} map[string]interface{} "currentVersion and history list"
// @Failure 400 {object} map[string]string "Missing or invalid parameters"
// @Failure 404 {object} map[string]string "History disabled or document not found"
// @Failure 409 {object} map[string]string "No open editor session for the document"
// @Router /api/office/history [get]
// @Security ApiKeyAuth
func onlyofficeHistoryGetHandler(w http.ResponseWriter, r *http.Request, d *Context) (int, error) {
	_, _, fi, status, err := officeHistoryContext(r, d)
	if err != nil {
		return status, err
	}
	currentKey, err := GetOnlyOfficeId(fi.RealPath)
	if err != nil {
		return http.StatusConflict, errors.New("no open editor session for this document")
	}
	versions, err := listOfficeVersions(officeHistoryDocDir(fi.RealPath))
	if err != nil {
		return http.StatusInternalServerError, err
	}
	current, history := buildOfficeHistory(versions, currentKey, fi.ModTime)
	return RenderJSON(w, r, map[string]interface{}{
		"currentVersion": current,
		"history":        history,
	})
}

// onlyofficeHistoryDataGetHandler returns the data the editor needs to display one version.
//
// @Summary Get OnlyOffice document version data
// @Description Returns the setHistoryData payload for one version: its file URL and, when stored, the previous version and the change set between them. Signed with the OnlyOffice secret.
// @Tags Office
// @Produce json
// @Param source query string true "Source name"
// @Param path query string true "File path"
// @Param version query int true "Version number"
// @Success 200 {object} map[string]interface{} "setHistoryData payload"
// @Failure 400 {object} map[string]string "Missing or invalid parameters"
// @Failure 404 {object} map[string]string "History disabled, document or version not found"
// @Router /api/office/history/data [get]
// @Security ApiKeyAuth
func onlyofficeHistoryDataGetHandler(w http.ResponseWriter, r *http.Request, d *Context) (int, error) {
	source, path, fi, status, err := officeHistoryContext(r, d)
	if err != nil {
		return status, err
	}
	version, err := strconv.Atoi(r.URL.Query().Get("version"))
	if err != nil || version < 1 {
		return http.StatusBadRequest, errors.New("invalid version")
	}
	versions, err := listOfficeVersions(officeHistoryDocDir(fi.RealPath))
	if err != nil {
		return http.StatusInternalServerError, err
	}
	byVersion := map[int]*officeVersionMeta{}
	current := 1
	for _, v := range versions {
		byVersion[v.Version] = v
		current = v.Version + 1
	}

	fileType := officeHistoryFileType(fi.RealPath)
	var key string
	if version == current {
		key, err = GetOnlyOfficeId(fi.RealPath)
		if err != nil {
			return http.StatusConflict, errors.New("no open editor session for this document")
		}
	} else if v, ok := byVersion[version]; ok {
		key = v.Key
		if v.FileType != "" {
			fileType = v.FileType
		}
	} else {
		return http.StatusNotFound, errors.New("version not found")
	}

	payload := map[string]interface{}{
		"version":  version,
		"key":      key,
		"fileType": fileType,
		"url":      buildOfficeHistoryFileURL(r, d, source, path, version, "file"),
	}
	if prev, ok := byVersion[version-1]; ok && prev.HasChanges {
		prevType := prev.FileType
		if prevType == "" {
			prevType = fileType
		}
		payload["previous"] = map[string]interface{}{
			"key":      prev.Key,
			"fileType": prevType,
			"url":      buildOfficeHistoryFileURL(r, d, source, path, prev.Version, "file"),
		}
		payload["changesUrl"] = buildOfficeHistoryFileURL(r, d, source, path, prev.Version, "changes")
	}

	if secret := settings.Config.Integrations.OnlyOffice.Secret; secret != "" {
		token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims(payload))
		signed, err := token.SignedString([]byte(secret))
		if err != nil {
			return http.StatusInternalServerError, errors.New("failed to sign history data")
		}
		payload["token"] = signed
	}
	return RenderJSON(w, r, payload)
}

// buildOfficeHistoryFileURL returns the URL the document server fetches a
// version's file or change set from.
func buildOfficeHistoryFileURL(r *http.Request, d *Context, source, path string, version int, kind string) string {
	params := url.Values{}
	params.Set("source", source)
	params.Set("path", path)
	params.Set("version", strconv.Itoa(version))
	params.Set("file", kind)
	params.Set("auth", d.Token)
	return joinOnlyOfficeAPIURL(onlyOfficeFileBrowserBaseURL(r), "api/office/history/file") + "?" + params.Encode()
}

// onlyofficeHistoryFileGetHandler serves a stored version or change set to the document server.
//
// @Summary Download an OnlyOffice document version
// @Description Serves the content of a document version (file) or the change set from that version to the next one (changes). Used by the OnlyOffice document server.
// @Tags Office
// @Produce octet-stream
// @Param source query string true "Source name"
// @Param path query string true "File path"
// @Param version query int true "Version number"
// @Param file query string true "file or changes"
// @Success 200 {file} file "Version content"
// @Failure 400 {object} map[string]string "Missing or invalid parameters"
// @Failure 404 {object} map[string]string "History disabled, document or version not found"
// @Router /api/office/history/file [get]
// @Security ApiKeyAuth
func onlyofficeHistoryFileGetHandler(w http.ResponseWriter, r *http.Request, d *Context) (int, error) {
	_, _, fi, status, err := officeHistoryContext(r, d)
	if err != nil {
		return status, err
	}
	version, err := strconv.Atoi(r.URL.Query().Get("version"))
	if err != nil || version < 1 {
		return http.StatusBadRequest, errors.New("invalid version")
	}
	kind := r.URL.Query().Get("file")
	if kind != "file" && kind != "changes" {
		return http.StatusBadRequest, errors.New("file must be file or changes")
	}

	docDir := officeHistoryDocDir(fi.RealPath)
	versions, err := listOfficeVersions(docDir)
	if err != nil {
		return http.StatusInternalServerError, err
	}
	current := 1
	if len(versions) > 0 {
		current = versions[len(versions)-1].Version + 1
	}

	var target string
	switch {
	case version == current && kind == "file":
		target = fi.RealPath
	case version < current:
		name := officeHistoryFileName
		if kind == "changes" {
			name = officeHistoryChangesName
		}
		target = filepath.Join(docDir, strconv.Itoa(version), name)
	default:
		return http.StatusNotFound, errors.New("version not found")
	}

	f, err := os.Open(target)
	if errors.Is(err, os.ErrNotExist) {
		return http.StatusNotFound, errors.New("version not found")
	}
	if err != nil {
		return http.StatusInternalServerError, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return http.StatusInternalServerError, err
	}
	name := fmt.Sprintf("%s.v%d", filepath.Base(fi.RealPath), version)
	if kind == "changes" {
		name = fmt.Sprintf("changes.v%d.zip", version)
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, name, stat.ModTime(), f)
	return 0, nil
}
