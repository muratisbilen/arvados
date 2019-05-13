// Copyright (C) The Arvados Authors. All rights reserved.
//
// SPDX-License-Identifier: AGPL-3.0

package router

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"git.curoverse.com/arvados.git/sdk/go/arvados"
	"git.curoverse.com/arvados.git/sdk/go/httpserver"
)

const rfc3339NanoFixed = "2006-01-02T15:04:05.000000000Z07:00"

type responseOptions struct {
	Select []string
	Count  string
}

func (rtr *router) responseOptions(opts interface{}) (responseOptions, error) {
	var rOpts responseOptions
	switch opts := opts.(type) {
	case *arvados.GetOptions:
		rOpts.Select = opts.Select
	case *arvados.ListOptions:
		rOpts.Select = opts.Select
		rOpts.Count = opts.Count
	}
	return rOpts, nil
}

func applySelectParam(selectParam []string, orig map[string]interface{}) map[string]interface{} {
	if len(selectParam) == 0 {
		return orig
	}
	selected := map[string]interface{}{}
	for _, attr := range selectParam {
		if v, ok := orig[attr]; ok {
			selected[attr] = v
		}
	}
	// Preserve "kind" even if not requested
	if v, ok := orig["kind"]; ok {
		selected["kind"] = v
	}
	return selected
}

func (rtr *router) sendResponse(w http.ResponseWriter, resp interface{}, opts responseOptions) {
	respKind := kind(resp)
	var tmp map[string]interface{}
	err := rtr.transcode(resp, &tmp)
	if err != nil {
		rtr.sendError(w, err)
		return
	}

	tmp["kind"] = respKind
	if items, ok := tmp["items"].([]interface{}); ok {
		for i, item := range items {
			// Fill in "kind" by inspecting UUID
			item, _ := item.(map[string]interface{})
			uuid, _ := item["uuid"].(string)
			if len(uuid) != 27 {
				// unsure whether this happens
			} else if t, ok := infixMap[uuid[6:11]]; !ok {
				// infix not listed in infixMap
			} else {
				item["kind"] = kind(t)
			}
			items[i] = applySelectParam(opts.Select, item)
		}
		if opts.Count == "none" {
			delete(tmp, "items_available")
		}
	} else {
		tmp = applySelectParam(opts.Select, tmp)
	}

	// Format non-nil timestamps as rfc3339NanoFixed (by default
	// they will have been encoded to time.RFC3339Nano, which
	// omits trailing zeroes).
	for k, v := range tmp {
		if !strings.HasSuffix(k, "_at") {
			continue
		}
		switch tv := v.(type) {
		case *time.Time:
			if tv == nil {
				break
			}
			tmp[k] = tv.Format(rfc3339NanoFixed)
		case time.Time:
			tmp[k] = tv.Format(rfc3339NanoFixed)
		case string:
			t, err := time.Parse(time.RFC3339Nano, tv)
			if err != nil {
				break
			}
			tmp[k] = t.Format(rfc3339NanoFixed)
		}
	}
	json.NewEncoder(w).Encode(tmp)
}

func (rtr *router) sendError(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	if err, ok := err.(interface{ HTTPStatus() int }); ok {
		code = err.HTTPStatus()
	}
	httpserver.Error(w, err.Error(), code)
}

var infixMap = map[string]interface{}{
	"4zz18": arvados.Collection{},
	"j7d0g": arvados.Group{},
}

var mungeKind = regexp.MustCompile(`\..`)

func kind(resp interface{}) string {
	return mungeKind.ReplaceAllStringFunc(fmt.Sprintf("%T", resp), func(s string) string {
		// "arvados.CollectionList" => "arvados#collectionList"
		return "#" + strings.ToLower(s[1:])
	})
}
