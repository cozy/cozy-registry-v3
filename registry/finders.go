package registry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Masterminds/semver"
	"github.com/cozy/cozy-apps-registry/cache"
	"github.com/cozy/cozy-apps-registry/config"
	"github.com/cozy/cozy-apps-registry/consts"
	"github.com/cozy/echo"
	"github.com/go-kivik/kivik"
	"github.com/spf13/viper"
)

var validFilters = []string{
	"type",
	"editor",
	"tags",
	"locales",
}

var validSorts = []string{
	"slug",
	"type",
	"editor",
	"created_at",
}

const maxLimit = 200

func getVersionID(appSlug, version string) string {
	return getAppID(appSlug) + "-" + version
}

func getAppID(appSlug string) string {
	return strings.ToLower(appSlug)
}

func findApp(c *Space, appSlug string) (*App, error) {
	if !validSlugReg.MatchString(appSlug) {
		return nil, ErrAppSlugInvalid
	}

	var doc *App
	var err error

	db := c.AppsDB()
	row := db.Get(ctx, getAppID(appSlug))
	if err = row.ScanDoc(&doc); err != nil {
		if kivik.StatusCode(err) == http.StatusNotFound {
			return nil, ErrAppNotFound
		}
		return nil, err
	}

	return doc, nil
}

func FindApp(c *Space, appSlug string, channel Channel) (*App, error) {
	doc, err := findApp(c, appSlug)
	if err != nil {
		return nil, err
	}

	doc.DataUsageCommitment, doc.DataUsageCommitmentBy = defaultDataUserCommitment(doc, nil)
	doc.Versions, err = FindAppVersions(c, doc.Slug, channel, true)
	if err != nil {
		return nil, err
	}
	doc.LatestVersion, err = FindLatestVersion(c, doc.Slug, Stable)
	if err != nil && err != ErrVersionNotFound {
		return nil, err
	}
	doc.Label = calculateAppLabel(doc, doc.LatestVersion)

	return doc, nil
}

type Attachment struct {
	ContentType   string
	Content       io.Reader
	Etag          string
	ContentLength string
}

func FindAppAttachment(c *Space, appSlug, filename string, channel Channel) (*Attachment, error) {
	if !validSlugReg.MatchString(appSlug) {
		return nil, ErrAppSlugInvalid
	}

	ver, err := FindLatestVersion(c, appSlug, channel)
	if err != nil {
		return nil, err
	}

	return FindVersionAttachment(c, appSlug, ver.Version, filename)
}

func FindVersionAttachment(c *Space, appSlug, version, filename string) (*Attachment, error) {
	// Return from swift
	conf, err := config.GetConfig()
	if err != nil {
		return nil, err
	}
	sc := conf.SwiftConnection
	var buf bytes.Buffer
	fp := filepath.Join(appSlug, version, filename)
	prefix := c.Prefix
	if prefix == "" {
		prefix = consts.DefaultSpacePrefix
	}
	headers, err := sc.ObjectGet(prefix, fp, &buf, false, nil)
	if err != nil {
		return nil, err
	}
	att := &Attachment{
		ContentType:   headers["Content-Type"],
		Content:       bytes.NewReader(buf.Bytes()),
		Etag:          headers["Etag"],
		ContentLength: headers["Content-Length"],
	}
	return att, nil
}

func FindVersionOldAttachment(c *Space, appSlug, version, filename string) (*kivik.Attachment, error) {
	db := c.VersDB()

	att, err := db.GetAttachment(ctx, getVersionID(appSlug, version), filename)
	if err != nil {
		if kivik.StatusCode(err) == http.StatusNotFound {
			return nil, echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("Could not find attachment %q", filename))
		}
		return nil, err
	}

	return att, nil
}

func findVersion(appSlug, version string, dbs ...*kivik.DB) (*Version, error) {
	if !validSlugReg.MatchString(appSlug) {
		return nil, ErrAppSlugInvalid
	}
	if !validVersionReg.MatchString(version) {
		return nil, ErrVersionInvalid
	}

	for _, db := range dbs {
		row := db.Get(ctx, getVersionID(appSlug, version))

		var doc *Version
		err := row.ScanDoc(&doc)
		if err != nil {
			// We got error
			if kivik.StatusCode(err) != http.StatusNotFound {
				// And this is a real error
				return nil, err
			}
		} else {
			// We have a doc
			return doc, nil
		}
	}

	return nil, ErrVersionNotFound
}

func FindPendingVersion(c *Space, appSlug, version string) (*Version, error) {
	// Test for pending version
	return findVersion(appSlug, version, c.dbPendingVers)
}

func FindPublishedVersion(c *Space, appSlug, version string) (*Version, error) {
	// Test for released version only
	return findVersion(appSlug, version, c.dbVers)
}

func FindVersion(c *Space, appSlug, version string) (*Version, error) {
	// Test for pending and released version
	return findVersion(appSlug, version, c.dbVers, c.dbPendingVers)
}

func versionViewQuery(c *Space, db *kivik.DB, appSlug, channel string, opts map[string]interface{}) (*kivik.Rows, error) {
	rows, err := db.Query(ctx, versViewDocName(appSlug), channel, opts)
	if err != nil {
		if kivik.StatusCode(err) == http.StatusNotFound {
			if err = createVersionsViews(c, db, appSlug); err != nil {
				return nil, err
			}
			return versionViewQuery(c, db, appSlug, channel, opts)
		}
		return nil, err
	}
	return rows, nil
}

// FindLastsVersionsUpTo returns versions of a channel up to a date
//
// Example: FindLastsVersionUpTo("foo", "stable", myDate) returns all the
// versions created beetween myDate and now
func FindLastsVersionsUpTo(c *Space, appSlug, channel string, date time.Time) ([]*Version, error) {
	db := c.VersDB()
	versions := []*Version{}

	marshaled, err := json.Marshal(date.Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}

	options := map[string]interface{}{
		"startkey":     string(marshaled),
		"include_docs": true,
	}

	rows, err := db.Query(ctx, "by-date", channel, options)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, ErrVersionNotFound
	}

	for rows.Next() {
		var version *Version
		if err := rows.ScanDoc(&version); err != nil {
			return nil, err
		}
		// Filter by version
		if version.Slug == appSlug {
			versions = append(versions, version)
		}
	}
	return versions, nil
}

// findPreviousMinor tries to find the old previous minor version of semver-type
// versions
func findPreviousMinor(version string, versions []string) (string, bool) {
	vs := []*semver.Version{}
	currentVersion, _ := semver.NewVersion(version)

	// Init
	for _, v := range versions {
		sv, _ := semver.NewVersion(v)
		vs = append(vs, sv)
	}

	// Sorting by reverse
	sort.Sort(sort.Reverse(semver.Collection(vs)))

	// Create constraints
	major := currentVersion.Major()
	minor := currentVersion.Minor()
	patch := currentVersion.Patch()
	notActualMinor, _ := semver.NewConstraint(fmt.Sprintf("< %s.%s.%s", strconv.FormatInt(major, 10), strconv.FormatInt(minor, 10), strconv.FormatInt(patch, 10))) // Try to get the next minor version
	inMajor, _ := semver.NewConstraint(fmt.Sprintf("> %s", strconv.FormatInt(major, 10)))

	// Finding
	for _, v := range vs {
		if inMajor.Check(v) && notActualMinor.Check(v) {
			return v.Original(), true
		}
	}
	return "", false
}

// findPreviousMajor tries to find the old previous major version of semver-type
// versions
func findPreviousMajor(version string, versions []string) (string, bool) {
	vs := []*semver.Version{}
	currentVersion, _ := semver.NewVersion(version)

	// Init
	for _, v := range versions {
		sv, _ := semver.NewVersion(v)
		vs = append(vs, sv)
	}

	// Sorting by reverse
	sort.Sort(sort.Reverse(semver.Collection(vs)))

	// Create constraints
	major := currentVersion.Major()
	previousMajor, _ := semver.NewConstraint(fmt.Sprintf("< %s.0.0", strconv.FormatInt(major, 10)))

	// Finding
	for _, v := range vs {
		if previousMajor.Check(v) {
			return v.Original(), true
		}
	}
	return "", false
}

// FindLastNVersions returns the N lasts versions of an app
// If N is greater than available versions, only available are returned
func FindLastNVersions(c *Space, appSlug string, channelStr string, nMajor, nMinor int) ([]*Version, error) {
	channel, err := StrToChannel(channelStr)
	if err != nil {
		return nil, err
	}
	versions, err := FindAppVersions(c, appSlug, channel, false)
	if err != nil {
		return nil, err
	}
	latestVersion, err := FindLatestVersion(c, appSlug, channel)
	if err != nil {
		return nil, err
	}
	var versionsList []string

	switch channel {
	case Stable:
		versionsList = versions.Stable
	case Beta:
		versionsList = versions.Beta
	case Dev:
		versionsList = versions.Dev
	}

	resVersions := []string{}
	minor := latestVersion.Version
	major := latestVersion.Version

	majors := []string{}

	for len(majors) < nMajor {
		minors := []string{}
		minors = append(minors, minor)
		majors = append(majors, major)

		for len(minors) < nMinor {
			previousMinor, ok := findPreviousMinor(minor, versionsList)
			if ok {
				minors = append(minors, previousMinor)
				minor = previousMinor
			} else {
				// No more versions available, append & break
				resVersions = append(resVersions, minors...)
				break
			}
			// Append to final result
			resVersions = append(resVersions, minors...)
		}
		major, ok := findPreviousMajor(major, versionsList)
		if ok {
			minor = major
		} else {
			break
		}
	}
	returned := []*Version{}

	for _, toReturn := range resVersions {
		v, err := FindVersion(c, appSlug, toReturn)
		if err != nil {
			return nil, err
		}
		returned = append(returned, v)
	}
	return returned, nil
}

func FindLatestVersion(c *Space, appSlug string, channel Channel) (*Version, error) {
	if !validSlugReg.MatchString(appSlug) {
		return nil, ErrAppSlugInvalid
	}

	channelStr := channelToStr(channel)

	key := cache.Key(c.Prefix + "/" + appSlug + "/" + channelStr)
	cacheVersionsLatest := viper.Get("cacheVersionsLatest").(cache.Cache)
	if data, ok := cacheVersionsLatest.Get(key); ok {
		var latestVersion *Version
		if err := json.Unmarshal(data, &latestVersion); err == nil {
			return latestVersion, nil
		}
	}

	db := c.VersDB()
	rows, err := versionViewQuery(c, db, appSlug, channelStr, map[string]interface{}{
		"limit":        1,
		"descending":   true,
		"include_docs": true,
	})
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, ErrVersionNotFound
	}

	var data json.RawMessage
	var latestVersion *Version
	if err = rows.ScanDoc(&data); err != nil {
		return nil, err
	}
	if err = json.Unmarshal(data, &latestVersion); err != nil {
		return nil, err
	}

	latestVersion.ID = ""
	latestVersion.Rev = ""
	latestVersion.Attachments = nil

	cacheVersionsLatest.Add(key, cache.Value(data))
	return latestVersion, nil
}

// FindAppVersions return all the app versions. The concat params allows you to
// concatenate stable & beta versions in dev list, and stable versions in beta
// list
func FindAppVersions(c *Space, appSlug string, channel Channel, concat bool) (*AppVersions, error) {
	db := c.VersDB()

	key := cache.Key(c.Prefix + "/" + appSlug + "/" + channelToStr(channel))
	cacheVersionsList := viper.Get("cacheVersionsList").(cache.Cache)
	if data, ok := cacheVersionsList.Get(key); ok {
		var versions *AppVersions
		if err := json.Unmarshal(data, &versions); err == nil {
			return versions, nil
		}
	}

	rows, err := versionViewQuery(c, db, appSlug, "dev", map[string]interface{}{
		"limit":      2000,
		"descending": false,
	})
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	allVersions := make([]string, int(rows.TotalRows()))
	for rows.Next() {
		var version string
		if err = rows.ScanValue(&version); err != nil {
			return nil, err
		}
		allVersions = append(allVersions, version)
	}

	var stable, beta, dev []string
	if concat {
		if channel == Dev {
			dev = allVersions
		}

		for _, v := range allVersions {
			switch GetVersionChannel(v) {
			case Stable:
				stable = append(stable, v)
				fallthrough
			case Beta:
				if channel == Beta || channel == Dev {
					beta = append(beta, v)
				}
			case Dev:
				// do nothing
			default:
				panic("unreachable")
			}
		}
	} else {
		for _, v := range allVersions {
			switch GetVersionChannel(v) {
			case Stable:
				stable = append(stable, v)
			case Beta:
				beta = append(beta, v)
			case Dev:
				dev = append(dev, v)
			default:
				panic("unreachable")
			}
		}
	}

	versions := &AppVersions{
		HasVersions: len(allVersions) > 0,
		Stable:      stable,
		Beta:        beta,
		Dev:         dev,
	}

	if data, err := json.Marshal(versions); err == nil {
		cacheVersionsList.Add(key, data)
	}

	return versions, nil
}

type AppsListOptions struct {
	Limit                int
	Cursor               int
	Sort                 string
	Filters              map[string]string
	LatestVersionChannel Channel
	VersionsChannel      Channel
}

func GetPendingVersions(c *Space) ([]*Version, error) {
	db := c.dbPendingVers
	rows, err := db.AllDocs(ctx, map[string]interface{}{
		"include_docs": true,
	})
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	versions := make([]*Version, 0)
	for rows.Next() {
		if strings.HasPrefix(rows.ID(), "_design") {
			continue
		}

		var version *Version
		if err := rows.ScanDoc(&version); err != nil {
			return nil, err
		}
		versions = append(versions, version)
	}

	return versions, nil
}

// GetAppChannelVersions returns the versions list of an app channel
func GetAppChannelVersions(c *Space, appSlug string, channel Channel) ([]*Version, error) {
	var versions []string
	var resultVersions []*Version

	fv, err := FindAppVersions(c, appSlug, channel, false)
	if err != nil {
		return nil, err
	}
	switch channel {
	case Stable:
		versions = fv.Stable
	case Beta:
		versions = fv.Beta
	case Dev:
		versions = fv.Dev
	}
	for _, v := range versions {
		vers, err := FindVersion(c, appSlug, v)
		if err != nil {
			return nil, err
		}
		resultVersions = append(resultVersions, vers)
	}

	return resultVersions, nil
}

func GetAppsList(c *Space, opts *AppsListOptions) (int, []*App, error) {
	db := c.AppsDB()
	order := "asc"

	sortField := opts.Sort
	if len(sortField) > 0 && sortField[0] == '-' {
		order = "desc"
		sortField = sortField[1:]
	}
	if sortField == "" || !stringInArray(sortField, validSorts) {
		sortField = "slug"
	}

	useIndex := appIndexName(sortField)
	sortFields := appsIndexes[sortField]
	sort := ""
	for _, field := range sortFields {
		if sort != "" {
			sort += ","
		}
		sort += fmt.Sprintf(`{"%s": "%s"}`, field, order)
	}

	selector := ``
	for name, val := range opts.Filters {
		if !stringInArray(name, validFilters) {
			continue
		}
		if selector != "" {
			selector += ","
		}
		switch name {
		case "tags", "locales":
			tags := strings.Split(val, ",")
			selector += string(sprintfJSON(`%s: {"$all": %s}`, name, tags))
		default:
			selector += string(sprintfJSON("%s: %s", name, val))
		}
	}
	if selector == "" {
		selector = string(sprintfJSON(`%s: {"$gt": null}`, sortField))
	}

	if opts.Limit == 0 {
		opts.Limit = 50
	} else if opts.Limit > maxLimit {
		opts.Limit = maxLimit
	}

	designsCount := len(appsIndexes)
	limit := opts.Limit + designsCount + 1
	cursor := opts.Cursor
	req := sprintfJSON(`{
  "use_index": %s,
  "selector": {`+selector+`},
  "skip": %s,
  "sort": [`+sort+`],
  "limit": %s
}`, useIndex, cursor, limit)

	rows, err := db.Find(ctx, req)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()

	res := make([]*App, 0)
	for rows.Next() {
		if strings.HasPrefix(rows.ID(), "_design") {
			continue
		}
		var doc *App
		if err = rows.ScanDoc(&doc); err != nil {
			return 0, nil, err
		}
		res = append(res, doc)
	}
	if len(res) == 0 {
		return -1, res, nil
	}

	if len(res) > opts.Limit {
		res = res[:opts.Limit]
		cursor += len(res)
	} else {
		// we fetch one more element so we know in this case the end of the list
		// has been reached.
		cursor = -1
	}

	for _, app := range res {
		app.DataUsageCommitment, app.DataUsageCommitmentBy = defaultDataUserCommitment(app, nil)
		app.Versions, err = FindAppVersions(c, app.Slug, opts.VersionsChannel, true)
		if err != nil {
			return 0, nil, err
		}
		app.LatestVersion, err = FindLatestVersion(c, app.Slug, opts.LatestVersionChannel)
		if err != nil && err != ErrVersionNotFound {
			return 0, nil, err
		}
		app.Label = calculateAppLabel(app, app.LatestVersion)
	}

	return cursor, res, nil
}

func GetMaintainanceApps(c *Space) ([]*App, error) {
	useIndex := appIndexName("maintenance")
	req := sprintfJSON(`{
  "use_index": %s,
  "selector": {"maintenance_activated": true},
  "limit": 1000
}`, useIndex)
	rows, err := c.dbApps.Find(ctx, req)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	apps := make([]*App, 0)
	for rows.Next() {
		var app App
		if strings.HasPrefix(rows.ID(), "_design") {
			continue
		}
		if err = rows.ScanDoc(&app); err != nil {
			return nil, err
		}
		apps = append(apps, &app)
	}

	return apps, nil
}
