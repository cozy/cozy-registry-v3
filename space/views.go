package space

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/cozy/cozy-apps-registry/base"
	"github.com/go-kivik/kivik/v3"
)

// TODO: to improve performances, we should use a single mango index instead of
// several views. To do that, we need to add the channel and a field with the
// version which can be sorted via mango to each document: for new document, it
// would be added in go, and for the existing ones, we need a migration. Then, we
// can index [slug, channel, version_array]. And the mango request should use the
// fields parameter to only include version numbers in the response. Finally,
// we should not forget to remove the CouchDB views.
const (
	viewsHelpers = `
function getVersionChannel(version) {
  if (version.indexOf("-dev.") >= 0) {
    return "dev";
  }
  if (version.indexOf("-beta.") >= 0) {
    return "beta";
  }
  return "stable";
}

function expandVersion(doc) {
  var v = [0, 0, 0];
  var exp = 0;
  var sp = doc.version.split(".");
  if (sp.length >= 3) {
    v[0] = parseInt(sp[0], 10);
    v[1] = parseInt(sp[1], 10);
    v[2] = parseInt(sp[2].split("-")[0], 10);
    var channel = getVersionChannel(doc.version);
    if (channel == "beta" && sp.length > 3) {
      exp = parseInt(sp[3], 10)
    }
  }
  return {
    v: v,
    channel: channel,
    code: (channel == "stable") ? 1 : 0,
    exp: exp,
    date: doc.created_at,
  };
}`

	devView = `
function(doc) {
  ` + viewsHelpers + `
  if (doc.slug != %q) {
    return
  }
  var version = expandVersion(doc);
  var key = version.v.concat(version.code, +new Date(version.date))
  emit(key, doc.version);
}`

	betaView = `
function(doc) {
  ` + viewsHelpers + `
  if (doc.slug != %q) {
    return
  }
  var version = expandVersion(doc);
  var channel = version.channel;
  if (channel == "beta" || channel == "stable") {
    var key = version.v.concat(version.code, version.exp)
    emit(key, doc.version);
  }
}`

	stableView = `
function(doc) {
  ` + viewsHelpers + `
  if (doc.slug != %q) {
    return
  }
  var version = expandVersion(doc);
  var channel = version.channel;
  if (channel == "stable") {
    var key = version.v;
    emit(key, doc.version);
  }
}`
)

type view struct {
	Map string `json:"map"`
}

var versionsViews = map[string]view{
	"dev":    {Map: devView},
	"beta":   {Map: betaView},
	"stable": {Map: stableView},
}

func VersViewDocName(appSlug string) string {
	return "versions-" + appSlug + "-v2"
}

func CreateVersionsViews(c *Space, db *kivik.DB, appSlug string) error {
	docID := fmt.Sprintf("_design/%s", url.PathEscape(VersViewDocName(appSlug)))

	var viewsBodies []string
	for name, view := range versionsViews {
		code := fmt.Sprintf(view.Map, appSlug)
		viewsBodies = append(viewsBodies,
			string(base.SprintfJSON(`%s: {"map": %s}`, name, code)))
	}

	viewsBody := json.RawMessage(`{` + strings.Join(viewsBodies, ",") + `}`)

	doc := struct {
		ID       string          `json:"_id"`
		Views    json.RawMessage `json:"views"`
		Language string          `json:"language"`
	}{
		ID:       docID,
		Views:    viewsBody,
		Language: "javascript",
	}

	_, _, err := db.CreateDoc(context.Background(), doc)
	if err != nil {
		if kivik.StatusCode(err) == http.StatusConflict {
			return nil
		}
		return fmt.Errorf("Could not create versions views: %s", err)
	}
	return nil
}

func CreateVersionsDateView(db *kivik.DB) error {
	var viewsBodies []string

	for channel := range versionsViews {
		code := fmt.Sprintf(`
		function (doc) {
			`+viewsHelpers+`
			var channel = getVersionChannel(doc.version);
			if (channel == "%s") {
				emit(doc.created_at);
			}
			}`, channel)
		viewsBodies = append(viewsBodies,
			string(base.SprintfJSON(`%s: {"map": %s}`, channel, code)))
	}

	docID := fmt.Sprintf("_design/%s", "by-date")
	doc := struct {
		ID       string          `json:"_id"`
		Views    json.RawMessage `json:"views"`
		Language string          `json:"language"`
	}{
		ID:       docID,
		Views:    json.RawMessage(`{` + strings.Join(viewsBodies, ",") + `}`),
		Language: "javascript",
	}
	_, _, err := db.CreateDoc(context.Background(), doc)
	if err != nil {
		if kivik.StatusCode(err) == http.StatusConflict {
			return nil
		}
		return fmt.Errorf("Could not create versions views: %s", err)
	}

	return nil
}
