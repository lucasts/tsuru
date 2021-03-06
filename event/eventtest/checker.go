// Copyright 2016 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package eventtest

import (
	"fmt"

	"github.com/tsuru/tsuru/db"
	"github.com/tsuru/tsuru/event"
	"gopkg.in/check.v1"
	"gopkg.in/mgo.v2/bson"
)

type EventDesc struct {
	Target          event.Target
	Kind            string
	Owner           string
	StartCustomData map[string]interface{}
	EndCustomData   map[string]interface{}
	LogMatches      string
	ErrorMatches    string
}

type hasEventChecker struct{}

func (hasEventChecker) Info() *check.CheckerInfo {
	return &check.CheckerInfo{Name: "HasEvent", Params: []string{"event desc"}}
}

func (hasEventChecker) Check(params []interface{}, names []string) (bool, string) {
	var evt EventDesc
	switch params[0].(type) {
	case EventDesc:
		evt = params[0].(EventDesc)
	case *EventDesc:
		evt = *params[0].(*EventDesc)
	default:
		return false, "First parameter must be of type EventDesc or *EventDesc"
	}
	conn, err := db.Conn()
	if err != nil {
		return false, err.Error()
	}
	defer conn.Close()
	query := map[string]interface{}{
		"target":  evt.Target,
		"kind":    evt.Kind,
		"owner":   evt.Owner,
		"running": false,
	}
	if evt.StartCustomData != nil {
		for k, v := range evt.StartCustomData {
			query["startcustomdata."+k] = v
		}
	}
	if evt.EndCustomData != nil {
		for k, v := range evt.EndCustomData {
			query["endcustomdata."+k] = v
		}
	}
	if evt.LogMatches != "" {
		query["log"] = bson.M{"$regex": evt.LogMatches}
	}
	if evt.ErrorMatches != "" {
		query["error"] = bson.M{"$regex": evt.ErrorMatches}
	}
	n, err := conn.Events().Find(query).Count()
	if err != nil {
		return false, err.Error()
	}
	if n == 0 {
		all, _ := event.All()
		msg := fmt.Sprintf("Event not found. Existing events in DB: %#v", all)
		return false, msg
	}
	if n > 1 {
		return false, "Multiple events match query"
	}
	return true, ""
}

var HasEvent check.Checker = hasEventChecker{}
