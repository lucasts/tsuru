// Copyright 2016 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package api

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cezarsa/form"
	"github.com/tsuru/tsuru/api/context"
	"github.com/tsuru/tsuru/app"
	"github.com/tsuru/tsuru/app/bind"
	"github.com/tsuru/tsuru/auth"
	"github.com/tsuru/tsuru/db"
	"github.com/tsuru/tsuru/errors"
	"github.com/tsuru/tsuru/event"
	tsuruIo "github.com/tsuru/tsuru/io"
	"github.com/tsuru/tsuru/log"
	"github.com/tsuru/tsuru/permission"
	"github.com/tsuru/tsuru/provision"
	"github.com/tsuru/tsuru/quota"
	"github.com/tsuru/tsuru/rec"
	"github.com/tsuru/tsuru/repository"
	"github.com/tsuru/tsuru/service"
	"gopkg.in/mgo.v2/bson"
)

func appTarget(appName string) event.Target {
	return event.Target{Name: "app", Value: appName}
}

func getAppFromContext(name string, r *http.Request) (app.App, error) {
	var err error
	a := context.GetApp(r)
	if a == nil {
		a, err = getApp(name)
		if err != nil {
			return app.App{}, err
		}
		context.SetApp(r, a)
	}
	return *a, nil
}

func getApp(name string) (*app.App, error) {
	a, err := app.GetByName(name)
	if err != nil {
		return nil, &errors.HTTP{Code: http.StatusNotFound, Message: fmt.Sprintf("App %s not found.", name)}
	}
	return a, nil
}

// title: remove app
// path: /apps/{name}
// method: DELETE
// produce: application/x-json-stream
// responses:
//   200: App removed
//   401: Unauthorized
//   404: Not found
func appDelete(w http.ResponseWriter, r *http.Request, t auth.Token) error {
	a, err := getAppFromContext(r.URL.Query().Get(":app"), r)
	if err != nil {
		return err
	}
	canDelete := permission.Check(t, permission.PermAppDelete,
		append(permission.Contexts(permission.CtxTeam, a.Teams),
			permission.Context(permission.CtxApp, a.Name),
			permission.Context(permission.CtxPool, a.Pool),
		)...,
	)
	if !canDelete {
		return permission.ErrUnauthorized
	}
	evt, err := event.New(&event.Opts{Target: appTarget(a.Name), Kind: permission.PermAppDelete, Owner: t.GetUserName(), CustomData: a})
	if err != nil {
		return err
	}
	defer func() { evt.Done(err) }()
	keepAliveWriter := tsuruIo.NewKeepAliveWriter(w, 30*time.Second, "")
	defer keepAliveWriter.Stop()
	writer := &tsuruIo.SimpleJsonMessageEncoderWriter{Encoder: json.NewEncoder(keepAliveWriter)}
	w.Header().Set("Content-Type", "application/x-json-stream")
	err = app.Delete(&a, writer)
	if err != nil {
		writer.Encode(tsuruIo.SimpleJsonMessage{Error: err.Error()})
	}
	return nil
}

// miniApp is a minimal representation of the app, created to make appList
// faster and transmit less data.
type miniApp struct {
	Name  string            `json:"name"`
	Units []provision.Unit  `json:"units"`
	CName []string          `json:"cname"`
	Ip    string            `json:"ip"`
	Lock  provision.AppLock `json:"lock"`
}

func minifyApp(app app.App) (miniApp, error) {
	units, err := app.Units()
	if err != nil {
		return miniApp{}, err
	}
	return miniApp{
		Name:  app.GetName(),
		Units: units,
		CName: app.GetCname(),
		Ip:    app.GetIp(),
		Lock:  app.GetLock(),
	}, nil
}

func appFilterByContext(contexts []permission.PermissionContext, filter *app.Filter) *app.Filter {
	if filter == nil {
		filter = &app.Filter{}
	}
contextsLoop:
	for _, c := range contexts {
		switch c.CtxType {
		case permission.CtxGlobal:
			filter.Extra = nil
			break contextsLoop
		case permission.CtxTeam:
			filter.ExtraIn("teams", c.Value)
		case permission.CtxApp:
			filter.ExtraIn("name", c.Value)
		case permission.CtxPool:
			filter.ExtraIn("pool", c.Value)
		}
	}
	return filter
}

// title: app list
// path: /apps
// method: GET
// produce: application/json
// responses:
//   200: List apps
//   204: No content
//   401: Unauthorized
func appList(w http.ResponseWriter, r *http.Request, t auth.Token) error {
	extra := make([]interface{}, 0, 1)
	filter := &app.Filter{}
	if name := r.URL.Query().Get("name"); name != "" {
		extra = append(extra, fmt.Sprintf("name=%s", name))
		filter.NameMatches = name
	}
	if platform := r.URL.Query().Get("platform"); platform != "" {
		extra = append(extra, fmt.Sprintf("platform=%s", platform))
		filter.Platform = platform
	}
	if teamOwner := r.URL.Query().Get("teamOwner"); teamOwner != "" {
		extra = append(extra, fmt.Sprintf("teamowner=%s", teamOwner))
		filter.TeamOwner = teamOwner
	}
	if owner := r.URL.Query().Get("owner"); owner != "" {
		extra = append(extra, fmt.Sprintf("owner=%s", owner))
		filter.UserOwner = owner
	}
	if pool := r.URL.Query().Get("pool"); pool != "" {
		extra = append(extra, fmt.Sprintf("pool=%s", pool))
		filter.Pool = pool
	}
	locked, _ := strconv.ParseBool(r.URL.Query().Get("locked"))
	if locked {
		extra = append(extra, fmt.Sprintf("locked=%v", locked))
		filter.Locked = true
	}
	if status, ok := r.URL.Query()["status"]; ok {
		extra = append(extra, fmt.Sprintf("status=%s", strings.Join(status, ",")))
		filter.Statuses = status
	}
	contexts := permission.ContextsForPermission(t, permission.PermAppRead)
	if len(contexts) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
	apps, err := app.List(appFilterByContext(contexts, filter))
	if err != nil {
		return err
	}
	if len(apps) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
	w.Header().Set("Content-Type", "application/json")
	miniApps := make([]miniApp, len(apps))
	for i, app := range apps {
		miniApps[i], err = minifyApp(app)
		if err != nil {
			return err
		}
	}
	return json.NewEncoder(w).Encode(miniApps)
}

// title: app info
// path: /apps/{name}
// method: GET
// produce: application/json
// responses:
//   200: OK
//   401: Unauthorized
//   404: Not found
func appInfo(w http.ResponseWriter, r *http.Request, t auth.Token) error {
	a, err := getAppFromContext(r.URL.Query().Get(":app"), r)
	if err != nil {
		return err
	}
	canRead := permission.Check(t, permission.PermAppRead,
		append(permission.Contexts(permission.CtxTeam, a.Teams),
			permission.Context(permission.CtxApp, a.Name),
			permission.Context(permission.CtxPool, a.Pool),
		)...,
	)
	if !canRead {
		return permission.ErrUnauthorized
	}
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(&a)
}

// title: app create
// path: /apps
// method: POST
// consume: application/x-www-form-urlencoded
// produce: application/json
// responses:
//   201: App created
//   400: Invalid data
//   401: Unauthorized
//   403: Quota exceeded
//   409: App already exists
func createApp(w http.ResponseWriter, r *http.Request, t auth.Token) (err error) {
	a := app.App{
		TeamOwner:   r.FormValue("teamOwner"),
		Platform:    r.FormValue("platform"),
		Plan:        app.Plan{Name: r.FormValue("plan")},
		Name:        r.FormValue("name"),
		Description: r.FormValue("description"),
		Pool:        r.FormValue("pool"),
	}
	if a.TeamOwner == "" {
		a.TeamOwner, err = permission.TeamForPermission(t, permission.PermAppCreate)
		if err != nil {
			return err
		}
	}
	canCreate := permission.Check(t, permission.PermAppCreate,
		permission.Context(permission.CtxTeam, a.TeamOwner),
	)
	if !canCreate {
		return permission.ErrUnauthorized
	}
	u, err := t.User()
	if err != nil {
		return err
	}
	platform, err := app.GetPlatform(a.Platform)
	if err != nil {
		return err
	}
	if platform.Disabled {
		canUsePlat := permission.Check(t, permission.PermPlatformUpdate) ||
			permission.Check(t, permission.PermPlatformCreate)
		if !canUsePlat {
			return &errors.HTTP{Code: http.StatusBadRequest, Message: app.InvalidPlatformError.Error()}
		}
	}
	evt, err := event.New(&event.Opts{Target: appTarget(a.Name), Kind: permission.PermAppCreate, Owner: t.GetUserName(), CustomData: a})
	if err != nil {
		return err
	}
	defer func() { evt.Done(err) }()
	err = app.CreateApp(&a, u)
	if err != nil {
		log.Errorf("Got error while creating app: %s", err)
		if e, ok := err.(*errors.ValidationError); ok {
			return &errors.HTTP{Code: http.StatusBadRequest, Message: e.Message}
		}
		if _, ok := err.(app.NoTeamsError); ok {
			return &errors.HTTP{
				Code:    http.StatusBadRequest,
				Message: "In order to create an app, you should be member of at least one team",
			}
		}
		if e, ok := err.(*app.AppCreationError); ok {
			if e.Err == app.ErrAppAlreadyExists {
				return &errors.HTTP{Code: http.StatusConflict, Message: e.Error()}
			}
			if _, ok := e.Err.(*quota.QuotaExceededError); ok {
				return &errors.HTTP{
					Code:    http.StatusForbidden,
					Message: "Quota exceeded",
				}
			}
		}
		if err == app.InvalidPlatformError {
			return &errors.HTTP{Code: http.StatusBadRequest, Message: err.Error()}
		}
		return err
	}
	repo, err := repository.Manager().GetRepository(a.Name)
	if err != nil {
		return err
	}
	msg := map[string]string{
		"status":         "success",
		"repository_url": repo.ReadWriteURL,
		"ip":             a.Ip,
	}
	jsonMsg, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusCreated)
	w.Header().Set("Content-Type", "application/json")
	w.Write(jsonMsg)
	return nil
}

// title: app update
// path: /apps/{name}
// method: PUT
// consume: application/x-www-form-urlencoded
// produce: application/x-json-stream
// responses:
//   200: App updated
//   401: Unauthorized
//   404: Not found
func updateApp(w http.ResponseWriter, r *http.Request, t auth.Token) error {
	updateData := app.App{
		TeamOwner:   r.FormValue("teamOwner"),
		Plan:        app.Plan{Name: r.FormValue("plan")},
		Pool:        r.FormValue("pool"),
		Description: r.FormValue("description"),
	}
	appName := r.URL.Query().Get(":appname")
	a, err := getAppFromContext(appName, r)
	if err != nil {
		return err
	}
	if updateData.Description == "" && updateData.Plan.Name == "" && updateData.Pool == "" && updateData.TeamOwner == "" {
		msg := "Neither the description, plan, pool or team owner were set. You must define at least one."
		return &errors.HTTP{Code: http.StatusBadRequest, Message: msg}
	}
	if updateData.Description != "" {
		allowed := permission.Check(t, permission.PermAppUpdateDescription,
			append(permission.Contexts(permission.CtxTeam, a.Teams),
				permission.Context(permission.CtxApp, a.Name),
				permission.Context(permission.CtxPool, a.Pool),
			)...,
		)
		if !allowed {
			return permission.ErrUnauthorized
		}

	}
	if updateData.Plan.Name != "" {
		allowed := permission.Check(t, permission.PermAppUpdatePlan,
			append(permission.Contexts(permission.CtxTeam, a.Teams),
				permission.Context(permission.CtxApp, a.Name),
				permission.Context(permission.CtxPool, a.Pool),
			)...,
		)
		if !allowed {
			return permission.ErrUnauthorized
		}
	}
	if updateData.Pool != "" {
		allowed := permission.Check(t, permission.PermAppUpdatePool,
			append(permission.Contexts(permission.CtxTeam, a.Teams),
				permission.Context(permission.CtxApp, a.Name),
				permission.Context(permission.CtxPool, a.Pool),
			)...,
		)
		if !allowed {
			return permission.ErrUnauthorized
		}
	}
	if updateData.TeamOwner != "" {
		allowed := permission.Check(t, permission.PermAppUpdateTeamowner,
			append(permission.Contexts(permission.CtxTeam, a.Teams),
				permission.Context(permission.CtxApp, a.Name),
				permission.Context(permission.CtxPool, a.Pool),
			)...,
		)
		if !allowed {
			return &errors.HTTP{
				Code:    http.StatusForbidden,
				Message: permission.ErrUnauthorized.Error(),
			}
		}
	}
	u, err := t.User()
	if err != nil {
		return err
	}
	keepAliveWriter := tsuruIo.NewKeepAliveWriter(w, 30*time.Second, "")
	defer keepAliveWriter.Stop()
	w.Header().Set("Content-Type", "application/x-json-stream")
	writer := &tsuruIo.SimpleJsonMessageEncoderWriter{Encoder: json.NewEncoder(keepAliveWriter)}
	err = a.Update(updateData, writer)
	if err == app.ErrPlanNotFound {
		writer.Encode(tsuruIo.SimpleJsonMessage{Error: err.Error()})
		return err
	}
	rec.Log(u.Email, "update-app", "app="+appName, "description="+updateData.Description, "pool="+updateData.Pool)
	return err
}

func numberOfUnits(r *http.Request) (uint, error) {
	unitsStr := r.FormValue("units")
	if unitsStr == "" {
		return 0, &errors.HTTP{
			Code:    http.StatusBadRequest,
			Message: "You must provide the number of units.",
		}
	}
	n, err := strconv.ParseUint(unitsStr, 10, 32)
	if err != nil || n == 0 {
		return 0, &errors.HTTP{
			Code:    http.StatusBadRequest,
			Message: "Invalid number of units: the number must be an integer greater than 0.",
		}
	}
	return uint(n), nil
}

// title: add units
// path: /apps/{name}/units
// method: PUT
// consume: application/x-www-form-urlencoded
// produce: application/x-json-stream
// responses:
//   200: Units added
//   400: Invalid data
//   401: Unauthorized
//   404: App not found
func addUnits(w http.ResponseWriter, r *http.Request, t auth.Token) error {
	n, err := numberOfUnits(r)
	if err != nil {
		return err
	}
	processName := r.FormValue("process")
	appName := r.URL.Query().Get(":app")
	u, err := t.User()
	if err != nil {
		return err
	}
	a, err := getAppFromContext(appName, r)
	if err != nil {
		return err
	}
	allowed := permission.Check(t, permission.PermAppUpdateUnitAdd,
		append(permission.Contexts(permission.CtxTeam, a.Teams),
			permission.Context(permission.CtxApp, a.Name),
			permission.Context(permission.CtxPool, a.Pool),
		)...,
	)
	if !allowed {
		return permission.ErrUnauthorized
	}
	rec.Log(u.Email, "add-units", "app="+appName, fmt.Sprintf("units=%d", n))
	w.Header().Set("Content-Type", "application/x-json-stream")
	keepAliveWriter := tsuruIo.NewKeepAliveWriter(w, 30*time.Second, "")
	defer keepAliveWriter.Stop()
	writer := &tsuruIo.SimpleJsonMessageEncoderWriter{Encoder: json.NewEncoder(keepAliveWriter)}
	err = a.AddUnits(n, processName, writer)
	if err != nil {
		writer.Encode(tsuruIo.SimpleJsonMessage{Error: err.Error()})
		return nil
	}
	return nil
}

// title: remove units
// path: /apps/{name}/units
// method: DELETE
// produce: application/x-json-stream
// responses:
//   200: Units removed
//   400: Invalid data
//   401: Unauthorized
//   404: App not found
func removeUnits(w http.ResponseWriter, r *http.Request, t auth.Token) error {
	n, err := numberOfUnits(r)
	if err != nil {
		return err
	}
	u, err := t.User()
	if err != nil {
		return err
	}
	processName := r.FormValue("process")
	appName := r.URL.Query().Get(":app")
	a, err := getAppFromContext(appName, r)
	if err != nil {
		return err
	}
	allowed := permission.Check(t, permission.PermAppUpdateUnitRemove,
		append(permission.Contexts(permission.CtxTeam, a.Teams),
			permission.Context(permission.CtxApp, a.Name),
			permission.Context(permission.CtxPool, a.Pool),
		)...,
	)
	if !allowed {
		return permission.ErrUnauthorized
	}
	rec.Log(u.Email, "remove-units", "app="+appName, fmt.Sprintf("units=%d", n))
	w.Header().Set("Content-Type", "application/x-json-stream")
	keepAliveWriter := tsuruIo.NewKeepAliveWriter(w, 30*time.Second, "")
	defer keepAliveWriter.Stop()
	writer := &tsuruIo.SimpleJsonMessageEncoderWriter{Encoder: json.NewEncoder(keepAliveWriter)}
	err = a.RemoveUnits(uint(n), processName, writer)
	if err != nil {
		writer.Encode(tsuruIo.SimpleJsonMessage{Error: err.Error()})
		return nil
	}
	return nil
}

// title: set unit status
// path: /apps/{app}/units/{unit}
// method: POST
// consume: application/x-www-form-urlencoded
// responses:
//   200: Ok
//   400: Invalid data
//   401: Unauthorized
//   404: App or unit not found
func setUnitStatus(w http.ResponseWriter, r *http.Request, t auth.Token) error {
	unitName := r.URL.Query().Get(":unit")
	if unitName == "" {
		return &errors.HTTP{
			Code:    http.StatusBadRequest,
			Message: "missing unit",
		}
	}
	postStatus := r.FormValue("status")
	status, err := provision.ParseStatus(postStatus)
	if err != nil {
		return &errors.HTTP{
			Code:    http.StatusBadRequest,
			Message: err.Error(),
		}
	}
	appName := r.URL.Query().Get(":app")
	a, err := app.GetByName(appName)
	if err != nil {
		return &errors.HTTP{Code: http.StatusNotFound, Message: err.Error()}
	}
	allowed := permission.Check(t, permission.PermAppUpdateUnitStatus,
		append(permission.Contexts(permission.CtxTeam, a.Teams),
			permission.Context(permission.CtxApp, a.Name),
			permission.Context(permission.CtxPool, a.Pool),
		)...,
	)
	if !allowed {
		return permission.ErrUnauthorized
	}
	err = a.SetUnitStatus(unitName, status)
	if _, ok := err.(*provision.UnitNotFoundError); ok {
		return &errors.HTTP{Code: http.StatusNotFound, Message: err.Error()}
	}
	return err
}

// title: set node status
// path: /node/status
// method: POST
// consume: application/x-www-form-urlencoded
// produce: application/json
// responses:
//   200: Ok
//   400: Invalid data
//   401: Unauthorized
//   404: App or unit not found
func setNodeStatus(w http.ResponseWriter, r *http.Request, t auth.Token) error {
	if t.GetAppName() != app.InternalAppName {
		return &errors.HTTP{Code: http.StatusForbidden, Message: "this token is not allowed to execute this action"}
	}
	err := r.ParseForm()
	if err != nil {
		return &errors.HTTP{Code: http.StatusBadRequest, Message: err.Error()}
	}
	var hostInput provision.NodeStatusData
	dec := form.NewDecoder(nil)
	dec.IgnoreUnknownKeys(true)
	err = dec.DecodeValues(&hostInput, r.Form)
	if err != nil {
		return &errors.HTTP{Code: http.StatusBadRequest, Message: err.Error()}
	}
	result, err := app.UpdateNodeStatus(hostInput)
	if err != nil {
		return err
	}
	w.Header().Add("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(result)
}

// title: grant access to app
// path: /apps/{app}/teams/{team}
// method: PUT
// responses:
//   200: Access granted
//   401: Unauthorized
//   404: App or team not found
//   409: Grant already exists
func grantAppAccess(w http.ResponseWriter, r *http.Request, t auth.Token) error {
	u, err := t.User()
	if err != nil {
		return err
	}
	appName := r.URL.Query().Get(":app")
	teamName := r.URL.Query().Get(":team")
	team := new(auth.Team)
	a, err := getAppFromContext(appName, r)
	if err != nil {
		return err
	}
	allowed := permission.Check(t, permission.PermAppUpdateGrant,
		append(permission.Contexts(permission.CtxTeam, a.Teams),
			permission.Context(permission.CtxApp, a.Name),
			permission.Context(permission.CtxPool, a.Pool),
		)...,
	)
	if !allowed {
		return permission.ErrUnauthorized
	}
	rec.Log(u.Email, "grant-app-access", "app="+appName, "team="+teamName)
	conn, err := db.Conn()
	if err != nil {
		return err
	}
	defer conn.Close()
	err = conn.Teams().Find(bson.M{"_id": teamName}).One(team)
	if err != nil {
		return &errors.HTTP{Code: http.StatusNotFound, Message: "Team not found"}
	}
	err = a.Grant(team)
	if err == app.ErrAlreadyHaveAccess {
		return &errors.HTTP{Code: http.StatusConflict, Message: err.Error()}
	}
	return err
}

// title: revoke access to app
// path: /apps/{app}/teams/{team}
// method: DELETE
// responses:
//   200: Access revoked
//   401: Unauthorized
//   403: Forbidden
//   404: App or team not found
func revokeAppAccess(w http.ResponseWriter, r *http.Request, t auth.Token) error {
	u, err := t.User()
	if err != nil {
		return err
	}
	appName := r.URL.Query().Get(":app")
	teamName := r.URL.Query().Get(":team")
	rec.Log(u.Email, "revoke-app-access", "app="+appName, "team="+teamName)
	team := new(auth.Team)
	a, err := getAppFromContext(appName, r)
	if err != nil {
		return err
	}
	allowed := permission.Check(t, permission.PermAppUpdateRevoke,
		append(permission.Contexts(permission.CtxTeam, a.Teams),
			permission.Context(permission.CtxApp, a.Name),
			permission.Context(permission.CtxPool, a.Pool),
		)...,
	)
	if !allowed {
		return permission.ErrUnauthorized
	}
	conn, err := db.Conn()
	if err != nil {
		return err
	}
	defer conn.Close()
	err = conn.Teams().Find(bson.M{"_id": teamName}).One(team)
	if err != nil {
		return &errors.HTTP{Code: http.StatusNotFound, Message: "Team not found"}
	}
	if len(a.Teams) == 1 {
		msg := "You can not revoke the access from this team, because it is the unique team with access to the app, and an app can not be orphaned"
		return &errors.HTTP{Code: http.StatusForbidden, Message: msg}
	}
	err = a.Revoke(team)
	switch err {
	case app.ErrNoAccess:
		return &errors.HTTP{Code: http.StatusNotFound, Message: err.Error()}
	case app.ErrCannotOrphanApp:
		return &errors.HTTP{Code: http.StatusForbidden, Message: err.Error()}
	default:
		return err
	}
}

// title: run commands
// path: /apps/{app}/run
// consume: application/x-www-form-urlencoded
// produce: application/x-json-stream
// method: POST
// responses:
//   200: Ok
//   401: Unauthorized
//   404: App not found
func runCommand(w http.ResponseWriter, r *http.Request, t auth.Token) error {
	msg := "You must provide the command to run"
	command := r.FormValue("command")
	if len(command) < 1 {
		return &errors.HTTP{Code: http.StatusBadRequest, Message: msg}
	}
	u, err := t.User()
	if err != nil {
		return err
	}
	appName := r.URL.Query().Get(":app")
	once := r.FormValue("once")
	a, err := getAppFromContext(appName, r)
	if err != nil {
		return err
	}
	allowed := permission.Check(t, permission.PermAppRun,
		append(permission.Contexts(permission.CtxTeam, a.Teams),
			permission.Context(permission.CtxApp, a.Name),
			permission.Context(permission.CtxPool, a.Pool),
		)...,
	)
	if !allowed {
		return permission.ErrUnauthorized
	}
	rec.Log(u.Email, "run-command", "app="+appName, "command="+command)
	w.Header().Set("Content-Type", "application/x-json-stream")
	keepAliveWriter := tsuruIo.NewKeepAliveWriter(w, 30*time.Second, "")
	defer keepAliveWriter.Stop()
	writer := &tsuruIo.SimpleJsonMessageEncoderWriter{Encoder: json.NewEncoder(keepAliveWriter)}
	err = a.Run(command, writer, once == "true")
	if err != nil {
		writer.Encode(tsuruIo.SimpleJsonMessage{Error: err.Error()})
		return nil
	}
	return nil
}

// title: get envs
// path: /apps/{app}/env
// method: GET
// produce: application/x-json-stream
// responses:
//   200: OK
//   401: Unauthorized
//   404: App not found
func getEnv(w http.ResponseWriter, r *http.Request, t auth.Token) error {
	var variables []string
	if envs, ok := r.URL.Query()["env"]; ok {
		variables = envs
	}
	appName := r.URL.Query().Get(":app")
	a, err := getAppFromContext(appName, r)
	if err != nil {
		return err
	}
	if !t.IsAppToken() {
		allowed := permission.Check(t, permission.PermAppReadEnv,
			append(permission.Contexts(permission.CtxTeam, a.Teams),
				permission.Context(permission.CtxApp, a.Name),
				permission.Context(permission.CtxPool, a.Pool),
			)...,
		)
		if !allowed {
			return permission.ErrUnauthorized
		}
	}
	return writeEnvVars(w, &a, variables...)
}

func writeEnvVars(w http.ResponseWriter, a *app.App, variables ...string) error {
	var result []bind.EnvVar
	w.Header().Set("Content-Type", "application/json")
	if len(variables) > 0 {
		for _, variable := range variables {
			if v, ok := a.Env[variable]; ok {
				result = append(result, v)
			}
		}
	} else {
		for _, v := range a.Env {
			result = append(result, v)
		}
	}
	return json.NewEncoder(w).Encode(result)
}

// Envs represents the configuration of an environment variable data
// for the remote API
type Envs struct {
	Envs      []struct{ Name, Value string }
	NoRestart bool
	Private   bool
}

// title: set envs
// path: /apps/{app}/env
// method: POST
// consume: application/x-www-form-urlencoded
// produce: application/x-json-stream
// responses:
//   200: Envs updated
//   400: Invalid data
//   401: Unauthorized
//   404: App not found
func setEnv(w http.ResponseWriter, r *http.Request, t auth.Token) error {
	err := r.ParseForm()
	if err != nil {
		return &errors.HTTP{Code: http.StatusBadRequest, Message: err.Error()}
	}
	var e Envs
	dec := form.NewDecoder(nil)
	dec.IgnoreUnknownKeys(true)
	err = dec.DecodeValues(&e, r.Form)
	if err != nil {
		return &errors.HTTP{Code: http.StatusBadRequest, Message: err.Error()}
	}
	if len(e.Envs) == 0 {
		msg := "You must provide the list of environment variables"
		return &errors.HTTP{Code: http.StatusBadRequest, Message: msg}
	}
	u, err := t.User()
	if err != nil {
		return err
	}
	extra := fmt.Sprintf("private=%t", e.Private)
	appName := r.URL.Query().Get(":app")
	a, err := getAppFromContext(appName, r)
	if err != nil {
		return err
	}
	allowed := permission.Check(t, permission.PermAppUpdateEnvSet,
		append(permission.Contexts(permission.CtxTeam, a.Teams),
			permission.Context(permission.CtxApp, a.Name),
			permission.Context(permission.CtxPool, a.Pool),
		)...,
	)
	if !allowed {
		return permission.ErrUnauthorized
	}
	envs := map[string]string{}
	variables := []bind.EnvVar{}
	for _, v := range e.Envs {
		envs[v.Name] = v.Value
		variables = append(variables, bind.EnvVar{Name: v.Name, Value: v.Value, Public: !e.Private})
	}
	rec.Log(u.Email, "set-env", "app="+appName, envs, extra)
	w.Header().Set("Content-Type", "application/x-json-stream")
	keepAliveWriter := tsuruIo.NewKeepAliveWriter(w, 30*time.Second, "")
	defer keepAliveWriter.Stop()
	writer := &tsuruIo.SimpleJsonMessageEncoderWriter{Encoder: json.NewEncoder(keepAliveWriter)}
	err = a.SetEnvs(
		bind.SetEnvApp{
			Envs:          variables,
			PublicOnly:    true,
			ShouldRestart: !e.NoRestart,
		}, writer)
	if err != nil {
		writer.Encode(tsuruIo.SimpleJsonMessage{Error: err.Error()})
		return nil
	}
	return nil
}

// title: unset envs
// path: /apps/{app}/env
// method: DELETE
// produce: application/x-json-stream
// responses:
//   200: Envs removed
//   400: Invalid data
//   401: Unauthorized
//   404: App not found
func unsetEnv(w http.ResponseWriter, r *http.Request, t auth.Token) error {
	msg := "You must provide the list of environment variables."
	if r.URL.Query().Get("env") == "" {
		return &errors.HTTP{Code: http.StatusBadRequest, Message: msg}
	}
	var variables []string
	if envs, ok := r.URL.Query()["env"]; ok {
		variables = envs
	} else {
		return &errors.HTTP{Code: http.StatusBadRequest, Message: msg}
	}
	appName := r.URL.Query().Get(":app")
	u, err := t.User()
	if err != nil {
		return err
	}
	a, err := getAppFromContext(appName, r)
	if err != nil {
		return err
	}
	allowed := permission.Check(t, permission.PermAppUpdateEnvUnset,
		append(permission.Contexts(permission.CtxTeam, a.Teams),
			permission.Context(permission.CtxApp, a.Name),
			permission.Context(permission.CtxPool, a.Pool),
		)...,
	)
	if !allowed {
		return permission.ErrUnauthorized
	}
	rec.Log(u.Email, "unset-env", "app="+appName, fmt.Sprintf("envs=%s", variables))
	w.Header().Set("Content-Type", "application/x-json-stream")
	keepAliveWriter := tsuruIo.NewKeepAliveWriter(w, 30*time.Second, "")
	defer keepAliveWriter.Stop()
	writer := &tsuruIo.SimpleJsonMessageEncoderWriter{Encoder: json.NewEncoder(keepAliveWriter)}
	noRestart, _ := strconv.ParseBool(r.URL.Query().Get("noRestart"))
	err = a.UnsetEnvs(
		bind.UnsetEnvApp{
			VariableNames: variables,
			PublicOnly:    true,
			ShouldRestart: !noRestart,
		}, writer)
	if err != nil {
		writer.Encode(tsuruIo.SimpleJsonMessage{Error: err.Error()})
		return nil
	}
	return nil
}

// title: set cname
// path: /apps/{app}/cname
// method: POST
// consume: application/x-www-form-urlencoded
// responses:
//   200: Ok
//   400: Invalid data
//   401: Unauthorized
//   404: App not found
func setCName(w http.ResponseWriter, r *http.Request, t auth.Token) error {
	err := r.ParseForm()
	if err != nil {
		msg := "You must provide the cname."
		return &errors.HTTP{Code: http.StatusBadRequest, Message: msg}
	}
	cnames := r.Form["cname"]
	if len(cnames) == 0 {
		msg := "You must provide the cname."
		return &errors.HTTP{Code: http.StatusBadRequest, Message: msg}
	}
	u, err := t.User()
	if err != nil {
		return err
	}
	appName := r.URL.Query().Get(":app")
	rec.Log(u.Email, "add-cname", "app="+appName, "cname="+strings.Join(cnames, ", "))
	a, err := getAppFromContext(appName, r)
	if err != nil {
		return err
	}
	allowed := permission.Check(t, permission.PermAppUpdateCnameAdd,
		append(permission.Contexts(permission.CtxTeam, a.Teams),
			permission.Context(permission.CtxApp, a.Name),
			permission.Context(permission.CtxPool, a.Pool),
		)...,
	)
	if !allowed {
		return permission.ErrUnauthorized
	}
	if err = a.AddCName(cnames...); err == nil {
		return nil
	}
	if err.Error() == "Invalid cname" {
		return &errors.HTTP{Code: http.StatusBadRequest, Message: err.Error()}
	}
	return err
}

// title: unset cname
// path: /apps/{app}/cname
// method: DELETE
// responses:
//   200: Ok
//   400: Invalid data
//   401: Unauthorized
//   404: App not found
func unsetCName(w http.ResponseWriter, r *http.Request, t auth.Token) error {
	cnames := r.URL.Query()["cname"]
	if len(cnames) == 0 {
		msg := "You must provide the cname."
		return &errors.HTTP{Code: http.StatusBadRequest, Message: msg}
	}
	u, err := t.User()
	if err != nil {
		return err
	}
	appName := r.URL.Query().Get(":app")
	a, err := getAppFromContext(appName, r)
	if err != nil {
		return err
	}
	allowed := permission.Check(t, permission.PermAppUpdateCnameRemove,
		append(permission.Contexts(permission.CtxTeam, a.Teams),
			permission.Context(permission.CtxApp, a.Name),
			permission.Context(permission.CtxPool, a.Pool),
		)...,
	)
	if !allowed {
		return permission.ErrUnauthorized
	}
	rec.Log(u.Email, "remove-cname", "app="+appName, "cnames="+strings.Join(cnames, ", "))
	if err = a.RemoveCName(cnames...); err == nil {
		return nil
	}
	if err.Error() == "Invalid cname" {
		return &errors.HTTP{Code: http.StatusBadRequest, Message: err.Error()}
	}
	return err
}

// title: app log
// path: /apps/{app}/log
// method: GET
// produce: application/x-json-stream
// responses:
//   200: Ok
//   400: Invalid data
//   401: Unauthorized
//   404: App not found
func appLog(w http.ResponseWriter, r *http.Request, t auth.Token) error {
	var err error
	var lines int
	if l := r.URL.Query().Get("lines"); l != "" {
		lines, err = strconv.Atoi(l)
		if err != nil {
			msg := `Parameter "lines" must be an integer.`
			return &errors.HTTP{Code: http.StatusBadRequest, Message: msg}
		}
	} else {
		return &errors.HTTP{Code: http.StatusBadRequest, Message: `Parameter "lines" is mandatory.`}
	}
	w.Header().Set("Content-Type", "application/x-json-stream")
	source := r.URL.Query().Get("source")
	unit := r.URL.Query().Get("unit")
	follow := r.URL.Query().Get("follow")
	appName := r.URL.Query().Get(":app")
	extra := []interface{}{
		"app=" + appName,
		fmt.Sprintf("lines=%d", lines),
	}
	if source != "" {
		extra = append(extra, "source="+source)
	}
	if follow == "1" {
		extra = append(extra, "follow=1")
	}
	if unit != "" {
		extra = append(extra, "unit="+unit)
	}
	filterLog := app.Applog{Source: source, Unit: unit}
	a, err := getAppFromContext(appName, r)
	if err != nil {
		return err
	}
	allowed := permission.Check(t, permission.PermAppReadLog,
		append(permission.Contexts(permission.CtxTeam, a.Teams),
			permission.Context(permission.CtxApp, a.Name),
			permission.Context(permission.CtxPool, a.Pool),
		)...,
	)
	if !allowed {
		return permission.ErrUnauthorized
	}
	logs, err := a.LastLogs(lines, filterLog)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(w)
	err = encoder.Encode(logs)
	if err != nil {
		return err
	}
	if follow != "1" {
		return nil
	}
	var closeChan <-chan bool
	if notifier, ok := w.(http.CloseNotifier); ok {
		closeChan = notifier.CloseNotify()
	} else {
		closeChan = make(chan bool)
	}
	l, err := app.NewLogListener(&a, filterLog)
	if err != nil {
		return err
	}
	logTracker.add(l)
	defer func() {
		logTracker.remove(l)
		l.Close()
	}()
	logChan := l.ListenChan()
	for {
		var logMsg app.Applog
		select {
		case <-closeChan:
			return nil
		case logMsg = <-logChan:
		}
		if logMsg == (app.Applog{}) {
			break
		}
		err := encoder.Encode([]app.Applog{logMsg})
		if err != nil {
			break
		}
	}
	return nil
}

func getServiceInstance(serviceName, instanceName, appName string) (*service.ServiceInstance, *app.App, error) {
	var app app.App
	conn, err := db.Conn()
	if err != nil {
		return nil, nil, err
	}
	defer conn.Close()
	instance, err := getServiceInstanceOrError(serviceName, instanceName)
	if err != nil {
		return nil, nil, err
	}
	err = conn.Apps().Find(bson.M{"name": appName}).One(&app)
	if err != nil {
		err = &errors.HTTP{Code: http.StatusNotFound, Message: fmt.Sprintf("App %s not found.", appName)}
		return nil, nil, err
	}
	return instance, &app, nil
}

// title: bind service instance
// path: /services/{service}/instances/{instance}/{app}
// method: PUT
// consume: application/x-www-form-urlencoded
// produce: application/x-json-stream
// responses:
//   200: Ok
//   400: Invalid data
//   401: Unauthorized
//   404: App not found
func bindServiceInstance(w http.ResponseWriter, r *http.Request, t auth.Token) error {
	instanceName := r.URL.Query().Get(":instance")
	appName := r.URL.Query().Get(":app")
	serviceName := r.URL.Query().Get(":service")
	noRestart, _ := strconv.ParseBool(r.FormValue("noRestart"))
	instance, a, err := getServiceInstance(serviceName, instanceName, appName)
	if err != nil {
		return err
	}
	allowed := permission.Check(t, permission.PermServiceInstanceUpdateBind,
		append(permission.Contexts(permission.CtxTeam, instance.Teams),
			permission.Context(permission.CtxServiceInstance, instance.Name),
		)...,
	)
	if !allowed {
		return permission.ErrUnauthorized
	}
	allowed = permission.Check(t, permission.PermAppUpdateBind,
		append(permission.Contexts(permission.CtxTeam, a.Teams),
			permission.Context(permission.CtxApp, a.Name),
			permission.Context(permission.CtxPool, a.Pool),
		)...,
	)
	if !allowed {
		return permission.ErrUnauthorized
	}
	rec.Log(t.GetUserName(), "bind-app", "instance="+instanceName, "app="+appName)
	w.Header().Set("Content-Type", "application/x-json-stream")
	keepAliveWriter := tsuruIo.NewKeepAliveWriter(w, 30*time.Second, "")
	defer keepAliveWriter.Stop()
	writer := &tsuruIo.SimpleJsonMessageEncoderWriter{Encoder: json.NewEncoder(keepAliveWriter)}
	err = instance.BindApp(a, !noRestart, writer)
	if err != nil {
		writer.Encode(tsuruIo.SimpleJsonMessage{Error: err.Error()})
		return nil
	}
	fmt.Fprintf(writer, "\nInstance %q is now bound to the app %q.\n", instanceName, appName)
	envs := a.InstanceEnv(instanceName)
	if len(envs) > 0 {
		fmt.Fprintf(writer, "The following environment variables are available for use in your app:\n\n")
		for k := range envs {
			fmt.Fprintf(writer, "- %s\n", k)
		}
		fmt.Fprintf(writer, "- %s\n", app.TsuruServicesEnvVar)
	}
	return nil
}

// title: unbind service instance
// path: /services/{service}/instances/{instance}/{app}
// method: DELETE
// produce: application/x-json-stream
// responses:
//   200: Ok
//   400: Invalid data
//   401: Unauthorized
//   404: App not found
func unbindServiceInstance(w http.ResponseWriter, r *http.Request, t auth.Token) error {
	instanceName, appName, serviceName := r.URL.Query().Get(":instance"), r.URL.Query().Get(":app"),
		r.URL.Query().Get(":service")
	noRestart, _ := strconv.ParseBool(r.URL.Query().Get("noRestart"))
	u, err := t.User()
	if err != nil {
		return err
	}
	instance, a, err := getServiceInstance(serviceName, instanceName, appName)
	if err != nil {
		return err
	}
	allowed := permission.Check(t, permission.PermServiceInstanceUpdateUnbind,
		append(permission.Contexts(permission.CtxTeam, instance.Teams),
			permission.Context(permission.CtxServiceInstance, instance.Name),
		)...,
	)
	if !allowed {
		return permission.ErrUnauthorized
	}
	allowed = permission.Check(t, permission.PermAppUpdateUnbind,
		append(permission.Contexts(permission.CtxTeam, a.Teams),
			permission.Context(permission.CtxApp, a.Name),
			permission.Context(permission.CtxPool, a.Pool),
		)...,
	)
	if !allowed {
		return permission.ErrUnauthorized
	}
	rec.Log(u.Email, "unbind-app", "instance="+instanceName, "app="+appName)
	w.Header().Set("Content-Type", "application/x-json-stream")
	keepAliveWriter := tsuruIo.NewKeepAliveWriter(w, 30*time.Second, "")
	defer keepAliveWriter.Stop()
	writer := &tsuruIo.SimpleJsonMessageEncoderWriter{Encoder: json.NewEncoder(keepAliveWriter)}
	err = instance.UnbindApp(a, !noRestart, writer)
	if err != nil {
		writer.Encode(tsuruIo.SimpleJsonMessage{Error: err.Error()})
		return nil
	}
	fmt.Fprintf(writer, "\nInstance %q is not bound to the app %q anymore.\n", instanceName, appName)
	return nil
}

// title: app restart
// path: /apps/{app}/restart
// method: POST
// consume: application/x-www-form-urlencoded
// produce: application/x-json-stream
// responses:
//   200: Ok
//   401: Unauthorized
//   404: App not found
func restart(w http.ResponseWriter, r *http.Request, t auth.Token) error {
	process := r.FormValue("process")
	u, err := t.User()
	if err != nil {
		return err
	}
	appName := r.URL.Query().Get(":app")
	a, err := getAppFromContext(appName, r)
	if err != nil {
		return err
	}
	allowed := permission.Check(t, permission.PermAppUpdateRestart,
		append(permission.Contexts(permission.CtxTeam, a.Teams),
			permission.Context(permission.CtxApp, a.Name),
			permission.Context(permission.CtxPool, a.Pool),
		)...,
	)
	if !allowed {
		return permission.ErrUnauthorized
	}
	rec.Log(u.Email, "restart", "app="+appName)
	w.Header().Set("Content-Type", "application/x-json-stream")
	keepAliveWriter := tsuruIo.NewKeepAliveWriter(w, 30*time.Second, "")
	defer keepAliveWriter.Stop()
	writer := &tsuruIo.SimpleJsonMessageEncoderWriter{Encoder: json.NewEncoder(keepAliveWriter)}
	err = a.Restart(process, writer)
	if err != nil {
		writer.Encode(tsuruIo.SimpleJsonMessage{Error: err.Error()})
		return err
	}
	return nil
}

// title: app sleep
// path: /apps/{app}/sleep
// method: POST
// consume: application/x-www-form-urlencoded
// produce: application/x-json-stream
// responses:
//   200: Ok
//   400: Invalid data
//   401: Unauthorized
//   404: App not found
func sleep(w http.ResponseWriter, r *http.Request, t auth.Token) error {
	process := r.FormValue("process")
	u, err := t.User()
	if err != nil {
		return err
	}
	appName := r.URL.Query().Get(":app")
	a, err := getAppFromContext(appName, r)
	if err != nil {
		return err
	}
	proxy := r.FormValue("proxy")
	if proxy == "" {
		return &errors.HTTP{Code: http.StatusBadRequest, Message: "Empty proxy URL"}
	}
	proxyURL, err := url.Parse(proxy)
	if err != nil {
		log.Errorf("Invalid url for proxy param: %v", proxy)
		return err
	}
	allowed := permission.Check(t, permission.PermAppUpdateSleep,
		append(permission.Contexts(permission.CtxTeam, a.Teams),
			permission.Context(permission.CtxApp, a.Name),
			permission.Context(permission.CtxPool, a.Pool),
		)...,
	)
	if !allowed {
		return permission.ErrUnauthorized
	}
	rec.Log(u.Email, "sleep", "app="+appName)
	w.Header().Set("Content-Type", "application/x-json-stream")
	keepAliveWriter := tsuruIo.NewKeepAliveWriter(w, 30*time.Second, "")
	defer keepAliveWriter.Stop()
	writer := &tsuruIo.SimpleJsonMessageEncoderWriter{Encoder: json.NewEncoder(keepAliveWriter)}
	err = a.Sleep(writer, process, proxyURL)
	if err != nil {
		writer.Encode(tsuruIo.SimpleJsonMessage{Error: err.Error()})
		return err
	}
	return nil
}

// title: app log
// path: /apps/{app}/log
// method: POST
// consume: application/x-www-form-urlencoded
// responses:
//   200: Ok
//   400: Invalid data
//   401: Unauthorized
//   404: App not found
func addLog(w http.ResponseWriter, r *http.Request, t auth.Token) error {
	a, err := app.GetByName(r.URL.Query().Get(":app"))
	if err != nil {
		return err
	}
	err = r.ParseForm()
	if err != nil {
		return &errors.HTTP{Code: http.StatusBadRequest, Message: err.Error()}
	}
	if t.GetAppName() != app.InternalAppName {
		allowed := permission.Check(t, permission.PermAppUpdateLog,
			append(permission.Contexts(permission.CtxTeam, a.Teams),
				permission.Context(permission.CtxApp, a.Name),
				permission.Context(permission.CtxPool, a.Pool),
			)...,
		)
		if !allowed {
			return permission.ErrUnauthorized
		}
	}
	logs := r.Form["message"]
	source := r.FormValue("source")
	if source == "" {
		source = "app"
	}
	unit := r.FormValue("unit")
	for _, log := range logs {
		err := a.Log(log, source, unit)
		if err != nil {
			return err
		}
	}
	return nil
}

// title: app swap
// path: /swap
// method: POST
// consume: application/x-www-form-urlencoded
// responses:
//   200: Ok
//   400: Invalid data
//   401: Unauthorized
//   404: App not found
//   409: App locked
//   412: Number of units or platform don't match
func swap(w http.ResponseWriter, r *http.Request, t auth.Token) error {
	u, err := t.User()
	if err != nil {
		return err
	}
	app1Name := r.FormValue("app1")
	app2Name := r.FormValue("app2")
	forceSwap := r.FormValue("force")
	cnameOnly, _ := strconv.ParseBool(r.FormValue("cnameOnly"))
	if forceSwap == "" {
		forceSwap = "false"
	}
	locked1, err := app.AcquireApplicationLockWait(app1Name, t.GetUserName(), "/swap", lockWaitDuration)
	if err != nil {
		return err
	}
	defer app.ReleaseApplicationLock(app1Name)
	locked2, err := app.AcquireApplicationLockWait(app2Name, t.GetUserName(), "/swap", lockWaitDuration)
	if err != nil {
		return err
	}
	defer app.ReleaseApplicationLock(app2Name)
	app1, err := getApp(app1Name)
	if err != nil {
		return err
	}
	if !locked1 {
		return &errors.HTTP{Code: http.StatusConflict, Message: fmt.Sprintf("%s: %s", app1.Name, &app1.Lock)}
	}
	app2, err := getApp(app2Name)
	if err != nil {
		return err
	}
	if !locked2 {
		return &errors.HTTP{Code: http.StatusConflict, Message: fmt.Sprintf("%s: %s", app2.Name, &app2.Lock)}
	}
	allowed1 := permission.Check(t, permission.PermAppUpdateSwap,
		append(permission.Contexts(permission.CtxTeam, app1.Teams),
			permission.Context(permission.CtxApp, app1.Name),
			permission.Context(permission.CtxPool, app1.Pool),
		)...,
	)
	allowed2 := permission.Check(t, permission.PermAppUpdateSwap,
		append(permission.Contexts(permission.CtxTeam, app2.Teams),
			permission.Context(permission.CtxApp, app2.Name),
			permission.Context(permission.CtxPool, app2.Pool),
		)...,
	)
	if !allowed1 || !allowed2 {
		return permission.ErrUnauthorized
	}
	// compare apps by platform type and number of units
	if forceSwap == "false" {
		if app1.Platform != app2.Platform {
			return &errors.HTTP{
				Code:    http.StatusPreconditionFailed,
				Message: "platforms don't match",
			}
		}
		app1Units, err := app1.Units()
		if err != nil {
			return err
		}
		app2Units, err := app2.Units()
		if err != nil {
			return err
		}
		if len(app1Units) != len(app2Units) {
			return &errors.HTTP{
				Code:    http.StatusPreconditionFailed,
				Message: "number of units doesn't match",
			}
		}
	}
	rec.Log(u.Email, "swap", "app1="+app1Name, "app2="+app2Name)
	return app.Swap(app1, app2, cnameOnly)
}

// title: app start
// path: /apps/{app}/start
// method: POST
// consume: application/x-www-form-urlencoded
// produce: application/x-json-stream
// responses:
//   200: Ok
//   401: Unauthorized
//   404: App not found
func start(w http.ResponseWriter, r *http.Request, t auth.Token) error {
	process := r.FormValue("process")
	u, err := t.User()
	if err != nil {
		return err
	}
	appName := r.URL.Query().Get(":app")
	rec.Log(u.Email, "start", "app="+appName)
	a, err := getAppFromContext(appName, r)
	if err != nil {
		return err
	}
	allowed := permission.Check(t, permission.PermAppUpdateStart,
		append(permission.Contexts(permission.CtxTeam, a.Teams),
			permission.Context(permission.CtxApp, a.Name),
			permission.Context(permission.CtxPool, a.Pool),
		)...,
	)
	if !allowed {
		return permission.ErrUnauthorized
	}
	w.Header().Set("Content-Type", "application/x-json-stream")
	keepAliveWriter := tsuruIo.NewKeepAliveWriter(w, 30*time.Second, "")
	defer keepAliveWriter.Stop()
	writer := &tsuruIo.SimpleJsonMessageEncoderWriter{Encoder: json.NewEncoder(keepAliveWriter)}
	err = a.Start(writer, process)
	if err != nil {
		writer.Encode(tsuruIo.SimpleJsonMessage{Error: err.Error()})
		return err
	}
	return nil
}

// title: app stop
// path: /apps/{app}/stop
// method: POST
// consume: application/x-www-form-urlencoded
// produce: application/x-json-stream
// responses:
//   200: Ok
//   401: Unauthorized
//   404: App not found
func stop(w http.ResponseWriter, r *http.Request, t auth.Token) error {
	process := r.FormValue("process")
	u, err := t.User()
	if err != nil {
		return err
	}
	appName := r.URL.Query().Get(":app")
	rec.Log(u.Email, "stop", "app="+appName)
	a, err := getAppFromContext(appName, r)
	if err != nil {
		return err
	}
	allowed := permission.Check(t, permission.PermAppUpdateStop,
		append(permission.Contexts(permission.CtxTeam, a.Teams),
			permission.Context(permission.CtxApp, a.Name),
			permission.Context(permission.CtxPool, a.Pool),
		)...,
	)
	if !allowed {
		return permission.ErrUnauthorized
	}
	w.Header().Set("Content-Type", "application/x-json-stream")
	keepAliveWriter := tsuruIo.NewKeepAliveWriter(w, 30*time.Second, "")
	defer keepAliveWriter.Stop()
	writer := &tsuruIo.SimpleJsonMessageEncoderWriter{Encoder: json.NewEncoder(keepAliveWriter)}
	err = a.Stop(writer, process)
	if err != nil {
		writer.Encode(tsuruIo.SimpleJsonMessage{Error: err.Error()})
		return err
	}
	return nil
}

// title: app unlock
// path: /apps/{app}/lock
// method: DELETE
// produce: application/json
// responses:
//   200: Ok
//   401: Unauthorized
//   404: App not found
func forceDeleteLock(w http.ResponseWriter, r *http.Request, t auth.Token) error {
	appName := r.URL.Query().Get(":app")
	a, err := getAppFromContext(appName, r)
	if err != nil {
		return err
	}
	allowed := permission.Check(t, permission.PermAppAdminUnlock,
		append(permission.Contexts(permission.CtxTeam, a.Teams),
			permission.Context(permission.CtxApp, a.Name),
			permission.Context(permission.CtxPool, a.Pool),
		)...,
	)
	if !allowed {
		return permission.ErrUnauthorized
	}
	app.ReleaseApplicationLock(a.Name)
	return nil
}

// title: register unit
// path: /apps/{app}/units/register
// method: POST
// consume: application/x-www-form-urlencoded
// produce: application/json
// responses:
//   200: Ok
//   401: Unauthorized
//   404: App not found
func registerUnit(w http.ResponseWriter, r *http.Request, t auth.Token) error {
	appName := r.URL.Query().Get(":app")
	a, err := app.GetByName(appName)
	if err != nil {
		return err
	}
	allowed := permission.Check(t, permission.PermAppUpdateUnitRegister,
		append(permission.Contexts(permission.CtxTeam, a.Teams),
			permission.Context(permission.CtxApp, a.Name),
			permission.Context(permission.CtxPool, a.Pool),
		)...,
	)
	if !allowed {
		return permission.ErrUnauthorized
	}
	defer r.Body.Close()
	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return err
	}
	val, err := url.ParseQuery(string(data))
	if err != nil {
		return err
	}
	hostname := val.Get("hostname")
	var customData map[string]interface{}
	rawCustomData := val.Get("customdata")
	if rawCustomData != "" {
		err = json.Unmarshal([]byte(rawCustomData), &customData)
		if err != nil {
			return err
		}
	}
	err = a.RegisterUnit(hostname, customData)
	if err != nil {
		if _, ok := err.(*provision.UnitNotFoundError); ok {
			return &errors.HTTP{Code: http.StatusNotFound, Message: err.Error()}
		}
		return err
	}
	return writeEnvVars(w, a)
}

// title: metric envs
// path: /apps/{app}/metric/envs
// method: GET
// produce: application/json
// responses:
//   200: Ok
//   401: Unauthorized
//   404: App not found
func appMetricEnvs(w http.ResponseWriter, r *http.Request, t auth.Token) error {
	a, err := getAppFromContext(r.URL.Query().Get(":app"), r)
	if err != nil {
		return err
	}
	allowed := permission.Check(t, permission.PermAppReadMetric,
		append(permission.Contexts(permission.CtxTeam, a.Teams),
			permission.Context(permission.CtxApp, a.Name),
			permission.Context(permission.CtxPool, a.Pool),
		)...,
	)
	if !allowed {
		return permission.ErrUnauthorized
	}
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(a.MetricEnvs())
}

// title: rebuild routes
// path: /apps/{app}/routes
// method: POST
// produce: application/json
// responses:
//   200: Ok
//   401: Unauthorized
//   404: App not found
func appRebuildRoutes(w http.ResponseWriter, r *http.Request, t auth.Token) error {
	u, err := t.User()
	if err != nil {
		return err
	}
	a, err := getAppFromContext(r.URL.Query().Get(":app"), r)
	if err != nil {
		return err
	}
	allowed := permission.Check(t, permission.PermAppAdminRoutes,
		append(permission.Contexts(permission.CtxTeam, a.Teams),
			permission.Context(permission.CtxApp, a.Name),
			permission.Context(permission.CtxPool, a.Pool),
		)...,
	)
	if !allowed {
		return permission.ErrUnauthorized
	}
	rec.Log(u.Email, "app-rebuild-routes", "app="+r.URL.Query().Get(":app"))
	w.Header().Set("Content-Type", "application/json")
	result, err := a.RebuildRoutes()
	if err != nil {
		return err
	}
	return json.NewEncoder(w).Encode(&result)
}
