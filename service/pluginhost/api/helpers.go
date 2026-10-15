// Package api exposes the plugin HTTP endpoints the plugins page needs.
//
// It lives beside the host rather than in server/controller so the whole
// plugin surface — framework, built-in plugins, HTTP endpoints and GUI — sits
// under service/pluginhost. Removing that directory and the two call sites in
// pre_run.go returns v2rayA to vanilla.
package api

import (
	"fmt"

	"github.com/gin-gonic/gin"
	"github.com/v2rayA/v2rayA/common"
	"github.com/v2rayA/v2rayA/kernel/v2ray"
	"github.com/v2rayA/v2rayA/pkg/util/log"
	"github.com/v2rayA/v2rayA/server/controller"
)

// logError logs an error and returns it, so a handler can `return logError(e)`
// and both record and answer with it.
func logError(err interface{}) error {
	e, ok := err.(error)
	if !ok {
		e = fmt.Errorf("%v", err)
	}
	log.Error("%v", e)
	return e
}

// badRequest answers with BAD_REQUEST naming the offending field.
func badRequest(field string, err interface{}) error {
	return common.Coded("BAD_REQUEST", logError(err), map[string]interface{}{"field": field})
}

// beginMutation takes the same turn the core's mutating endpoints take, so a
// plugin action cannot race a core reload. It also announces the change to the
// plugins, which is what makes a policy edit visible to the watchdog.
func beginMutation(ctx *gin.Context) (release func(), ok bool) {
	return controller.BeginMutation(ctx, func() {
		v2ray.NotifyPlugins(v2ray.EventServersChanged)
	})
}