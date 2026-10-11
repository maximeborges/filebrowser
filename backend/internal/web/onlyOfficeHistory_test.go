package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	jwt "github.com/golang-jwt/jwt/v4"
	"github.com/gtsteffaniak/filebrowser/backend/internal/adapters/fs/files"
	"github.com/gtsteffaniak/filebrowser/backend/internal/database/share"
	"github.com/gtsteffaniak/filebrowser/backend/internal/database/users"
	"github.com/gtsteffaniak/filebrowser/backend/internal/utils"
	"github.com/gtsteffaniak/filebrowser/backend/pkg/indexing/iteminfo"
	"github.com/gtsteffaniak/filebrowser/backend/pkg/settings"
)

const officeHistoryTestSecret = "onlyoffice-history-secret"

// officeHistoryTestEnv is a document on disk edited through mocked OnlyOffice
// callbacks, with history enabled.
type officeHistoryTestEnv struct {
	t        *testing.T
	realPath string
	user     *users.User
	// served by the mocked document server
	nextDocument string
	changesZip   string
}

func newOfficeHistoryTestEnv(t *testing.T, initial string) *officeHistoryTestEnv {
	t.Helper()
	initStreamTestSources(t)

	dir := t.TempDir()
	env := &officeHistoryTestEnv{
		t:        t,
		realPath: filepath.Join(dir, "doc.docx"),
		user: testUserWithSourcePerms("/default", users.SourceFilePermissions{
			View: true, Download: true, Modify: true,
		}),
		changesZip: "changes-zip-bytes",
	}
	if err := os.WriteFile(env.realPath, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}

	origOnlyOffice := settings.Config.Integrations.OnlyOffice
	origNameToSource := settings.Config.Server.NameToSource
	origFileInfo := files.FileInfoFasterFunc
	origWriteFile := files.WriteFileFunc
	origClient := onlyOfficeDownloadClient
	t.Cleanup(func() {
		settings.Config.Integrations.OnlyOffice = origOnlyOffice
		settings.Config.Server.NameToSource = origNameToSource
		files.FileInfoFasterFunc = origFileInfo
		files.WriteFileFunc = origWriteFile
		onlyOfficeDownloadClient = origClient
		utils.OnlyOfficeCache.Delete(env.realPath)
	})

	settings.Config.Integrations.OnlyOffice.Url = "http://onlyoffice.test"
	settings.Config.Integrations.OnlyOffice.Secret = officeHistoryTestSecret
	settings.Config.Integrations.OnlyOffice.History = settings.OnlyOfficeHistory{
		Enabled: true,
		Path:    filepath.Join(dir, "history"),
	}
	settings.Config.Server.NameToSource = map[string]*settings.Source{
		"default": {Name: "default", Path: "/default"},
	}

	files.FileInfoFasterFunc = func(utils.FileOptions, *users.User) (*iteminfo.ExtendedFileInfo, error) {
		stat, err := os.Stat(env.realPath)
		if err != nil {
			return nil, err
		}
		fi := &iteminfo.ExtendedFileInfo{RealPath: env.realPath}
		fi.Name = "doc.docx"
		fi.ModTime = stat.ModTime()
		return fi, nil
	}
	files.WriteFileFunc = func(_ string, _ string, in io.Reader) error {
		data, err := io.ReadAll(in)
		if err != nil {
			return err
		}
		return os.WriteFile(env.realPath, data, 0o600)
	}
	onlyOfficeDownloadClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := env.nextDocument
		if strings.HasSuffix(r.URL.Path, "changes.zip") {
			body = env.changesZip
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})}
	return env
}

// openSession simulates opening the editor: the document gets a session key.
func (e *officeHistoryTestEnv) openSession(key string) {
	utils.OnlyOfficeCache.Set(e.realPath, key)
}

func (e *officeHistoryTestEnv) callback(key string, status int, document string, withChanges bool, history *OnlyOfficeHistory) {
	e.t.Helper()
	e.nextDocument = document
	cb := &OnlyOfficeCallback{Key: key, Status: status, History: history}
	if status != onlyOfficeStatusDocumentClosedWithNoChanges {
		cb.URL = "http://onlyoffice.test/cache/output.docx"
	}
	if withChanges {
		cb.ChangesURL = "http://onlyoffice.test/cache/changes.zip"
	}
	req := httptest.NewRequest(http.MethodPost, "/api/office/callback?source=default&path=/doc.docx", nil)
	rec := httptest.NewRecorder()
	if _, err := processOnlyOfficeCallback(rec, req, &requestContext{User: e.user}, cb); err != nil {
		e.t.Fatalf("callback status %d: %v", status, err)
	}
	if rec.Code != http.StatusOK {
		e.t.Fatalf("callback status %d: HTTP %d %s", status, rec.Code, rec.Body.String())
	}
}

func (e *officeHistoryTestEnv) get(handler handleFunc, query url.Values, d *requestContext) *httptest.ResponseRecorder {
	e.t.Helper()
	if d == nil {
		d = &requestContext{User: e.user, Token: "session-token"}
	}
	query.Set("source", "default")
	query.Set("path", "/doc.docx")
	req := httptest.NewRequest(http.MethodGet, "/api/office/history?"+query.Encode(), nil)
	rec := httptest.NewRecorder()
	status, err := handler(rec, req, d)
	if err != nil || status != 0 && status != http.StatusOK {
		rec.Code = status
		if err != nil {
			rec.Body.Reset()
			rec.Body.WriteString(err.Error())
		}
	}
	return rec
}

func (e *officeHistoryTestEnv) versions() []*officeVersionMeta {
	e.t.Helper()
	versions, err := listOfficeVersions(officeHistoryDocDir(e.realPath))
	if err != nil {
		e.t.Fatal(err)
	}
	return versions
}

func (e *officeHistoryTestEnv) readVersionFile(version int, name string) string {
	e.t.Helper()
	data, err := os.ReadFile(filepath.Join(officeHistoryDocDir(e.realPath), strconv.Itoa(version), name))
	if err != nil {
		e.t.Fatal(err)
	}
	return string(data)
}

func aliceHistory(created string) *OnlyOfficeHistory {
	return &OnlyOfficeHistory{
		ServerVersion: "9.3.4",
		Changes: []interface{}{
			map[string]interface{}{
				"created": created,
				"user":    map[string]interface{}{"id": "7", "name": "alice"},
			},
		},
	}
}

// A session that force saves and then closes stores the content it started
// from as one version, with the change set of the whole session.
func TestOfficeHistoryForceSaveThenCloseStoresSessionStart(t *testing.T) {
	env := newOfficeHistoryTestEnv(t, "v1")
	env.openSession("key-1")

	env.callback("key-1", onlyOfficeStatusForceSaveWhileDocumentStillOpen, "v2-draft", false, nil)
	if got := env.versions(); len(got) != 0 {
		t.Fatalf("force save created %d versions, want 0", len(got))
	}

	env.callback("key-1", onlyOfficeStatusDocumentClosedWithChanges, "v2", true, aliceHistory("2026-10-11 10:00:00"))

	live, _ := os.ReadFile(env.realPath)
	if string(live) != "v2" {
		t.Fatalf("live document = %q, want v2", live)
	}
	versions := env.versions()
	if len(versions) != 1 {
		t.Fatalf("got %d versions, want 1", len(versions))
	}
	v := versions[0]
	if v.Version != 1 || v.Key != "key-1" || !v.HasChanges || v.FileType != "docx" {
		t.Fatalf("version = %+v", v)
	}
	if got := env.readVersionFile(1, officeHistoryFileName); got != "v1" {
		t.Fatalf("version 1 content = %q, want v1 (content before the force save)", got)
	}
	if got := env.readVersionFile(1, officeHistoryChangesName); got != env.changesZip {
		t.Fatalf("changes = %q", got)
	}
	if _, err := os.Stat(filepath.Join(officeHistoryDocDir(env.realPath), officeHistoryPendingDir, "key-1")); !os.IsNotExist(err) {
		t.Fatalf("pending snapshot not removed: %v", err)
	}
}

// A session whose edits were all force saved ends with status 4; its starting
// content is still kept, without a change set.
func TestOfficeHistoryForceSaveThenCloseWithoutChanges(t *testing.T) {
	env := newOfficeHistoryTestEnv(t, "v1")
	env.openSession("key-1")

	env.callback("key-1", onlyOfficeStatusForceSaveWhileDocumentStillOpen, "v2", false, nil)
	env.callback("key-1", onlyOfficeStatusDocumentClosedWithNoChanges, "", false, nil)

	versions := env.versions()
	if len(versions) != 1 || versions[0].HasChanges {
		t.Fatalf("versions = %+v, want one version without changes", versions)
	}
	if got := env.readVersionFile(1, officeHistoryFileName); got != "v1" {
		t.Fatalf("version 1 content = %q, want v1", got)
	}
}

// Opening and closing a document without editing it stores nothing.
func TestOfficeHistoryCloseWithoutEditsStoresNothing(t *testing.T) {
	env := newOfficeHistoryTestEnv(t, "v1")
	env.openSession("key-1")
	env.callback("key-1", onlyOfficeStatusDocumentClosedWithNoChanges, "", false, nil)
	if got := env.versions(); len(got) != 0 {
		t.Fatalf("got %d versions, want 0", len(got))
	}
}

func TestOfficeHistoryDisabledStoresNothing(t *testing.T) {
	env := newOfficeHistoryTestEnv(t, "v1")
	settings.Config.Integrations.OnlyOffice.History.Enabled = false
	env.openSession("key-1")
	env.callback("key-1", onlyOfficeStatusDocumentClosedWithChanges, "v2", true, nil)

	if _, err := os.Stat(settings.Config.Integrations.OnlyOffice.History.Path); !os.IsNotExist(err) {
		t.Fatalf("history directory created while disabled: %v", err)
	}
	rec := env.get(onlyofficeHistoryGetHandler, url.Values{}, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("history endpoint status = %d, want 404", rec.Code)
	}
}

// Three editing sessions: the history lists versions 1-3, each version
// describes the edit that produced it, and the editor gets the data to replay
// the change sets.
func TestOfficeHistoryEndpoints(t *testing.T) {
	env := newOfficeHistoryTestEnv(t, "v1")
	env.openSession("key-1")
	env.callback("key-1", onlyOfficeStatusDocumentClosedWithChanges, "v2", true, aliceHistory("2026-10-11 10:00:00"))
	env.openSession("key-2")
	env.callback("key-2", onlyOfficeStatusDocumentClosedWithChanges, "v3", true, nil)
	env.openSession("key-3") // the editor is open on version 3

	rec := env.get(onlyofficeHistoryGetHandler, url.Values{}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("history status = %d: %s", rec.Code, rec.Body.String())
	}
	var history struct {
		CurrentVersion int                  `json:"currentVersion"`
		History        []officeHistoryEntry `json:"history"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &history); err != nil {
		t.Fatal(err)
	}
	if history.CurrentVersion != 3 || len(history.History) != 3 {
		t.Fatalf("history = %+v, want current 3 and 3 entries", history)
	}
	keys := []string{history.History[0].Key, history.History[1].Key, history.History[2].Key}
	if strings.Join(keys, ",") != "key-1,key-2,key-3" {
		t.Fatalf("keys = %v", keys)
	}
	v2 := history.History[1]
	if v2.User == nil || v2.User.Name != "alice" || v2.Created != "2026-10-11 10:00:00" || v2.ServerVersion != "9.3.4" || len(v2.Changes) == 0 {
		t.Fatalf("version 2 entry = %+v, want alice's change", v2)
	}
	v3 := history.History[2]
	if v3.User == nil || v3.User.Name != "alice" || v3.Changes != nil {
		t.Fatalf("version 3 entry = %+v, want saved-by fallback without changes", v3)
	}
	if history.History[0].User != nil {
		t.Fatalf("version 1 has no recorded edit, got user %+v", history.History[0].User)
	}

	// Version 2: its file, version 1 as previous, and the change set 1 -> 2.
	rec = env.get(onlyofficeHistoryDataGetHandler, url.Values{"version": {"2"}}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("data status = %d: %s", rec.Code, rec.Body.String())
	}
	var data map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if data["key"] != "key-2" || data["fileType"] != "docx" {
		t.Fatalf("data = %v", data)
	}
	prev, ok := data["previous"].(map[string]interface{})
	if !ok || prev["key"] != "key-1" {
		t.Fatalf("previous = %v", data["previous"])
	}
	changesURL, _ := data["changesUrl"].(string)
	parsed, err := url.Parse(changesURL)
	if err != nil || parsed.Query().Get("version") != "1" || parsed.Query().Get("file") != "changes" || parsed.Query().Get("auth") != "session-token" {
		t.Fatalf("changesUrl = %q", changesURL)
	}
	token, _ := data["token"].(string)
	claims := jwt.MapClaims{}
	if _, err := jwt.ParseWithClaims(token, claims, func(*jwt.Token) (interface{}, error) {
		return []byte(officeHistoryTestSecret), nil
	}); err != nil {
		t.Fatalf("history data token does not verify: %v", err)
	}
	if claims["key"] != "key-2" || claims["changesUrl"] != changesURL {
		t.Fatalf("token claims = %v", claims)
	}

	// Version 1 has nothing before it.
	rec = env.get(onlyofficeHistoryDataGetHandler, url.Values{"version": {"1"}}, nil)
	data = map[string]interface{}{}
	if err := json.Unmarshal(rec.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if _, ok := data["previous"]; ok {
		t.Fatalf("version 1 has a previous version: %v", data)
	}

	// The document server fetches the files.
	for _, tc := range []struct {
		version, file, want string
		status              int
	}{
		{"1", "file", "v1", http.StatusOK},
		{"2", "file", "v2", http.StatusOK},
		{"3", "file", "v3", http.StatusOK},
		{"1", "changes", env.changesZip, http.StatusOK},
		{"3", "changes", "", http.StatusNotFound},
		{"4", "file", "", http.StatusNotFound},
		{"1", "../../etc/passwd", "", http.StatusBadRequest},
	} {
		rec = env.get(onlyofficeHistoryFileGetHandler, url.Values{"version": {tc.version}, "file": {tc.file}}, nil)
		if rec.Code != tc.status {
			t.Errorf("file %s v%s: status %d, want %d", tc.file, tc.version, rec.Code, tc.status)
			continue
		}
		if tc.status == http.StatusOK && rec.Body.String() != tc.want {
			t.Errorf("file %s v%s = %q, want %q", tc.file, tc.version, rec.Body.String(), tc.want)
		}
	}
}

func TestOfficeHistoryPrunesOldestVersions(t *testing.T) {
	env := newOfficeHistoryTestEnv(t, "v1")
	settings.Config.Integrations.OnlyOffice.History.MaxVersions = 2
	for i, key := range []string{"key-1", "key-2", "key-3"} {
		env.openSession(key)
		env.callback(key, onlyOfficeStatusDocumentClosedWithChanges, "v"+strconv.Itoa(i+2), true, nil)
	}
	versions := env.versions()
	if len(versions) != 2 || versions[0].Version != 2 || versions[1].Version != 3 {
		t.Fatalf("versions = %+v, want 2 and 3", versions)
	}

	env.openSession("key-4")
	rec := env.get(onlyofficeHistoryGetHandler, url.Values{}, nil)
	var history struct {
		CurrentVersion int                  `json:"currentVersion"`
		History        []officeHistoryEntry `json:"history"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &history); err != nil {
		t.Fatal(err)
	}
	if history.CurrentVersion != 4 || len(history.History) != 3 || history.History[0].Version != 2 {
		t.Fatalf("history = %+v", history)
	}
	// Version 2's predecessor was pruned, so it shows as a plain document.
	rec = env.get(onlyofficeHistoryDataGetHandler, url.Values{"version": {"2"}}, nil)
	var data map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if _, ok := data["previous"]; ok {
		t.Fatalf("pruned predecessor still referenced: %v", data)
	}
}

func TestOfficeHistoryRejectsShares(t *testing.T) {
	env := newOfficeHistoryTestEnv(t, "v1")
	env.openSession("key-1")
	d := &requestContext{User: env.user, Share: share.Share{ShareColumns: share.ShareColumns{Hash: "abc"}}}
	rec := env.get(onlyofficeHistoryGetHandler, url.Values{}, d)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("share history status = %d, want 403", rec.Code)
	}
}

func TestOfficeHistoryRequiresViewPermission(t *testing.T) {
	env := newOfficeHistoryTestEnv(t, "v1")
	env.openSession("key-1")
	d := &requestContext{User: testUserWithSourcePerms("/default", users.SourceFilePermissions{})}
	rec := env.get(onlyofficeHistoryFileGetHandler, url.Values{"version": {"1"}, "file": {"file"}}, d)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}
